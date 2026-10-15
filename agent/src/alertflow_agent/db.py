from __future__ import annotations

import json
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _set_clause(fields: dict) -> tuple[str, list]:
    """Build a SQL SET clause. JSON columns take a JSON string and a ::jsonb cast."""
    sets: list[str] = []
    values: list = []
    for key, value in fields.items():
        if key in {"input_event", "usage", "report", "payload", "evidence"}:
            sets.append(f"{key} = %s::jsonb")
            values.append(json.dumps(value))
        else:
            sets.append(f"{key} = %s")
            values.append(value)
    return ", ".join(sets), values


def psycopg_connect(url: str):
    """Open a short-lived connection. Kept in one place so every JSON write casts."""
    import psycopg

    return psycopg.connect(url)


class MemoryStore:
    def __init__(self) -> None:
        self.lock = threading.RLock()
        self.runs: dict[str, dict] = {}
        self.by_key: dict[str, str] = {}
        self.events: dict[str, list[dict]] = {}
        self.evidence: dict[str, dict] = {}
        self.reports: dict[str, list[dict]] = {}
        self.callbacks: list[dict] = []
        self.callback_ids: dict[str, str] = {}

    def insert_run(self, row: dict) -> dict | None:
        with self.lock:
            if row["dedupe_key"] in self.by_key:
                return self.runs[self.by_key[row["dedupe_key"]]]
            self.runs[row["id"]] = dict(row)
            self.by_key[row["dedupe_key"]] = row["id"]
            self.events[row["id"]] = []
            return None

    def get(self, run_id: str) -> dict | None:
        with self.lock:
            row = self.runs.get(run_id)
            return dict(row) if row else None

    def find_incident(self, incident_id: int, event_type: str = "incident_start") -> dict | None:
        with self.lock:
            for row in self.runs.values():
                if row.get("incident_id") == incident_id and row.get("event_type") == event_type:
                    return dict(row)
            return None

    def update(self, run_id: str, **fields) -> None:
        with self.lock:
            self.runs[run_id].update(fields)

    def finish(self, run_id: str, callback: dict | None = None, **fields) -> bool:
        with self.lock:
            row = self.runs[run_id]
            if row["status"] == "cancelled":
                return False
            row.update(fields)
            self._enqueue_callback_locked(callback)
            return True

    # --- 回调 outbox ---------------------------------------------------------
    # 入队与运行状态变更共用同一把锁（Postgres 侧是同一个事务）：
    # 「报告写好了但回调没排队」不会发生。

    def _enqueue_callback_locked(self, callback: dict | None) -> bool:
        if not callback:
            return False
        message_id = callback["message_id"]
        if message_id in self.callback_ids:
            return False
        row = {
            "id": str(uuid.uuid4()),
            "message_id": message_id,
            "message_type": callback.get("message_type") or "",
            "correlation_id": callback.get("correlation_id") or "",
            "payload": callback,
            "status": "pending",
            "attempt_count": 0,
            "next_attempt_at": 0.0,
            "last_error": None,
            "lease_until": 0.0,
            "sent_at": None,
        }
        self.callbacks.append(row)
        self.callback_ids[message_id] = row["id"]
        return True

    def enqueue_callback(self, callback: dict) -> bool:
        with self.lock:
            return self._enqueue_callback_locked(callback)

    def claim_callbacks(self, limit: int, lease_seconds: int, max_attempts: int) -> list[dict]:
        now = time.time()
        claimed: list[dict] = []
        with self.lock:
            for row in self.callbacks:
                if len(claimed) >= limit:
                    break
                if row["status"] in {"sent", "dead"}:
                    continue
                due = row["status"] in {"pending", "retry_wait"} and row["next_attempt_at"] <= now
                expired = row["status"] == "sending" and row["lease_until"] <= now
                if not (due or expired):
                    continue
                if row["attempt_count"] >= max_attempts:
                    row["status"] = "dead"
                    row["last_error"] = row["last_error"] or "attempt limit reached"
                    continue
                row["status"] = "sending"
                row["attempt_count"] += 1
                row["lease_until"] = now + lease_seconds
                claimed.append(dict(row))
        return claimed

    def finish_callback(self, callback_id: str, status: str, error: str | None, retry_after_seconds: float) -> None:
        with self.lock:
            for row in self.callbacks:
                if row["id"] != callback_id:
                    continue
                row["status"] = status
                row["last_error"] = error
                row["lease_until"] = 0.0
                if status == "retry_wait":
                    row["next_attempt_at"] = time.time() + retry_after_seconds
                if status == "sent":
                    row["sent_at"] = time.time()
                return

    def callback_rows(self) -> list[dict]:
        with self.lock:
            return [dict(row) for row in self.callbacks]

    def transition(self, run_id: str, allowed: set[str], **fields) -> bool:
        with self.lock:
            row = self.runs.get(run_id)
            if not row or row["status"] not in allowed:
                return False
            row.update(fields)
            return True

    def has_event(self, run_id: str, kind: str, event_id: str) -> bool:
        with self.lock:
            return any(
                item["type"] == kind and (item["payload"] or {}).get("event_id") == event_id
                for item in self.events.get(run_id, [])
            )

    def append_event(self, run_id: str, kind: str, payload: dict) -> int:
        with self.lock:
            return self._append_locked(run_id, kind, payload)

    def _append_locked(self, run_id: str, kind: str, payload: dict) -> int:
        seq = len(self.events[run_id]) + 1
        self.events[run_id].append({"seq": seq, "type": kind, "payload": payload, "ts": _now()})
        return seq

    def record_resolve(self, run_id: str, event_id: str, resolved_at, revise: bool) -> str:
        with self.lock:
            row = self.runs.get(run_id)
            if not row:
                return "ignored"
            if self.has_event(run_id, "incident.resolved", event_id):
                return "duplicate"
            current = json.loads(json.dumps(row.get("input_event") or {}))
            current["resolved_at"] = resolved_at
            action = "resolved"
            if revise:
                current["pending_revise"] = True
                action = "recorded"
                if row["status"] in {"completed", "partial"}:
                    current["event"] = "incident_resolve"
                    row["mode"] = "baseline"
                    row["stage"] = "revise"
                    row["status"] = "queued"
                    action = "revise"
            row["input_event"] = current
            self._append_locked(run_id, "incident.resolved", {"event_id": event_id, "resolved_at": resolved_at})
            return action

    def commit_report(self, run_id: str, version: int, report: dict, status: str, stop_reason, usage: dict, queue_revise: bool = False, cited: list | None = None, replace_partial: bool = False, callback: dict | None = None) -> str:
        with self.lock:
            row = self.runs[run_id]
            if row["status"] == "cancelled":
                return "cancelled"
            rows = self.reports.setdefault(run_id, [])
            existing = next((item for item in rows if item["version"] == version), None)
            if existing is not None:
                old_status = (existing.get("report") or {}).get("status")
                if not (replace_partial and version == 1 and old_status == "partial"):
                    row.update(status="failed", failure_reason="report_conflict", stage="persist", finished_at=_now())
                    self._append_locked(run_id, "run.failed", {"reason": "report_conflict"})
                    return "conflict"
                existing["report"] = report
            else:
                rows.append({"version": version, "report": report, "created_at": _now()})
            row.update(status=status, stop_reason=stop_reason, failure_reason=None, stage="persist", finished_at=_now(), usage=usage or {})
            self._append_locked(run_id, "report.saved", {"version": version, "status": status, "cited": cited or []})
            if queue_revise and version == 1 and (row.get("input_event") or {}).get("pending_revise"):
                current = json.loads(json.dumps(row["input_event"]))
                current["event"] = "incident_resolve"
                row.update(input_event=current, status="queued", stage="revise", mode="baseline")
            # 与报告写入同一个临界区：报告落库了，回调就一定已经排队。
            self._enqueue_callback_locked(callback)
            return "saved"

    def list_events(self, run_id: str, after: int = 0) -> list[dict]:
        with self.lock:
            return [e for e in self.events.get(run_id, []) if e["seq"] > after]

    def save_evidence(self, run_id: str, evidence: dict) -> None:
        with self.lock:
            self.evidence[run_id] = evidence

    def get_evidence(self, run_id: str) -> dict:
        with self.lock:
            return self.evidence.get(run_id, {})

    def save_report(self, run_id: str, version: int, report: dict) -> bool:
        with self.lock:
            rows = self.reports.setdefault(run_id, [])
            if any(r["version"] == version for r in rows):
                return False
            rows.append({"version": version, "report": report, "created_at": _now()})
            return True

    def latest_report(self, run_id: str) -> dict | None:
        with self.lock:
            rows = self.reports.get(run_id, [])
            return dict(rows[-1]) if rows else None

    def list_runs(self) -> list[dict]:
        with self.lock:
            return [dict(r) for r in self.runs.values()]

    def claim_queued(self) -> dict | None:
        with self.lock:
            for row in self.runs.values():
                if row["status"] == "queued":
                    row["status"] = "running"
                    row["started_at"] = _now()
                    return dict(row)
            return None

    def running_ids(self) -> list[str]:
        with self.lock:
            return [r["id"] for r in self.runs.values() if r["status"] == "running"]


class PostgresStore:
    def __init__(self, url: str) -> None:
        import psycopg

        self.url = url
        self.conn = psycopg.connect(url, autocommit=True)

    def insert_run(self, row: dict) -> dict | None:
        import psycopg

        try:
            self.conn.execute(
                """INSERT INTO runs (id, dedupe_key, incident_id, service_id, event_type, mode, status, stage, input_event, usage)
                   VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s::jsonb, %s::jsonb)""",
                (
                    row["id"], row["dedupe_key"], row.get("incident_id"), row["service_id"], row["event_type"],
                    row["mode"], row["status"], row.get("stage") or "", json.dumps(row["input_event"]), json.dumps(row.get("usage") or {}),
                ),
            )
        except psycopg.errors.UniqueViolation:
            existing = self.conn.execute("SELECT id::text FROM runs WHERE dedupe_key = %s", (row["dedupe_key"],)).fetchone()
            return self.get(existing[0]) if existing else None
        return None

    def get(self, run_id: str) -> dict | None:
        cur = self.conn.execute(
            """SELECT id::text, dedupe_key, incident_id, service_id, event_type, mode, status, stage,
                      stop_reason, failure_reason, input_event, usage, created_at, started_at, finished_at
               FROM runs WHERE id = %s""",
            (run_id,),
        )
        row = cur.fetchone()
        if not row:
            return None
        keys = ["id", "dedupe_key", "incident_id", "service_id", "event_type", "mode", "status", "stage",
                "stop_reason", "failure_reason", "input_event", "usage", "created_at", "started_at", "finished_at"]
        return dict(zip(keys, row))

    def find_incident(self, incident_id: int, event_type: str = "incident_start") -> dict | None:
        row = self.conn.execute(
            "SELECT id::text FROM runs WHERE incident_id = %s AND event_type = %s ORDER BY created_at LIMIT 1",
            (incident_id, event_type),
        ).fetchone()
        return self.get(row[0]) if row else None

    def update(self, run_id: str, **fields) -> None:
        if not fields:
            return
        sets = []
        vals = []
        for key, value in fields.items():
            if key in {"input_event", "usage"}:
                sets.append(f"{key} = %s::jsonb")
                vals.append(json.dumps(value))
            else:
                sets.append(f"{key} = %s")
                vals.append(value)
        vals.append(run_id)
        self.conn.execute(f"UPDATE runs SET {', '.join(sets)} WHERE id = %s", vals)

    def finish(self, run_id: str, callback: dict | None = None, **fields) -> bool:
        if not callback:
            return self._update_where(run_id, "status <> 'cancelled'", fields)
        import psycopg

        with psycopg.connect(self.url) as conn:
            with conn.transaction():
                row = conn.execute("SELECT status FROM runs WHERE id = %s FOR UPDATE", (run_id,)).fetchone()
                if not row or row[0] == "cancelled":
                    return False
                clause, values = _set_clause(fields)
                values.append(run_id)
                conn.execute(f"UPDATE runs SET {clause} WHERE id = %s", values)
                self._insert_callback_tx(conn, callback)
                return True

    # --- 回调 outbox ---------------------------------------------------------

    def _insert_callback_tx(self, conn, callback: dict | None) -> bool:
        if not callback:
            return False
        cur = conn.execute(
            """INSERT INTO agent_callback_outbox (message_id, message_type, correlation_id, payload)
               VALUES (%s, %s, %s, %s::jsonb) ON CONFLICT (message_id) DO NOTHING""",
            (callback["message_id"], callback.get("message_type") or "",
             callback.get("correlation_id") or "", json.dumps(callback)),
        )
        return cur.rowcount == 1

    def enqueue_callback(self, callback: dict) -> bool:
        with psycopg_connect(self.url) as conn:
            with conn.transaction():
                return self._insert_callback_tx(conn, callback)

    def claim_callbacks(self, limit: int, lease_seconds: int, max_attempts: int) -> list[dict]:
        with psycopg_connect(self.url) as conn:
            with conn.transaction():
                # 先给用尽重试次数的行收尾，再领取到期的。
                conn.execute(
                    """UPDATE agent_callback_outbox
                       SET status = 'dead',
                           last_error = COALESCE(last_error, 'attempt limit reached'),
                           lease_until = NULL,
                           updated_at = now()
                       WHERE attempt_count >= %s
                         AND status IN ('pending', 'retry_wait', 'sending')
                         AND (status <> 'sending' OR lease_until <= now())""",
                    (max_attempts,),
                )
                rows = conn.execute(
                    """UPDATE agent_callback_outbox AS c
                       SET status = 'sending',
                           attempt_count = c.attempt_count + 1,
                           lease_until = now() + %s * interval '1 second',
                           updated_at = now()
                       FROM (
                           SELECT id FROM agent_callback_outbox
                           WHERE (status IN ('pending', 'retry_wait') AND next_attempt_at <= now())
                              OR (status = 'sending' AND lease_until <= now())
                           ORDER BY next_attempt_at
                           FOR UPDATE SKIP LOCKED
                           LIMIT %s
                       ) AS picked
                       WHERE c.id = picked.id
                       RETURNING c.id::text, c.message_id, c.message_type, c.correlation_id,
                                 c.payload, c.attempt_count""",
                    (lease_seconds, limit),
                ).fetchall()
        return [
            {"id": r[0], "message_id": r[1], "message_type": r[2], "correlation_id": r[3],
             "payload": r[4], "attempt_count": r[5]}
            for r in rows
        ]

    def finish_callback(self, callback_id: str, status: str, error: str | None, retry_after_seconds: float) -> None:
        if status == "retry_wait":
            self.conn.execute(
                """UPDATE agent_callback_outbox
                   SET status = 'retry_wait', last_error = %s,
                       next_attempt_at = now() + %s * interval '1 second',
                       lease_until = NULL, updated_at = now()
                   WHERE id = %s""",
                (error, retry_after_seconds, callback_id),
            )
            return
        if status == "sent":
            self.conn.execute(
                """UPDATE agent_callback_outbox
                   SET status = 'sent', sent_at = now(), last_error = NULL,
                       lease_until = NULL, updated_at = now()
                   WHERE id = %s""",
                (callback_id,),
            )
            return
        self.conn.execute(
            """UPDATE agent_callback_outbox
               SET status = 'dead', last_error = %s, lease_until = NULL, updated_at = now()
               WHERE id = %s""",
            (error, callback_id),
        )

    def callback_rows(self) -> list[dict]:
        rows = self.conn.execute(
            """SELECT id::text, message_id, message_type, correlation_id, payload, status,
                      attempt_count, last_error, sent_at
               FROM agent_callback_outbox ORDER BY created_at"""
        ).fetchall()
        keys = ["id", "message_id", "message_type", "correlation_id", "payload", "status",
                "attempt_count", "last_error", "sent_at"]
        return [dict(zip(keys, r)) for r in rows]

    def transition(self, run_id: str, allowed: set[str], **fields) -> bool:
        marks = ", ".join(["%s"] * len(allowed))
        return self._update_where(run_id, f"status IN ({marks})", fields, list(allowed))

    def _update_where(self, run_id: str, clause: str, fields: dict, extra: list | None = None) -> bool:
        if not fields:
            return False
        assignments, vals = _set_clause(fields)
        vals.extend(extra or [])
        vals.append(run_id)
        cur = self.conn.execute(f"UPDATE runs SET {assignments} WHERE {clause} AND id = %s", vals)
        return cur.rowcount == 1

    def has_event(self, run_id: str, kind: str, event_id: str) -> bool:
        row = self.conn.execute(
            "SELECT 1 FROM run_events WHERE run_id = %s AND type = %s AND payload->>'event_id' = %s LIMIT 1",
            (run_id, kind, event_id),
        ).fetchone()
        return row is not None

    def append_event(self, run_id: str, kind: str, payload: dict) -> int:
        import psycopg

        with psycopg.connect(self.url) as conn:
            with conn.transaction():
                locked = conn.execute("SELECT id FROM runs WHERE id = %s FOR UPDATE", (run_id,)).fetchone()
                if not locked:
                    raise KeyError(run_id)
                seq = conn.execute(
                    "SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = %s",
                    (run_id,),
                ).fetchone()[0]
                conn.execute(
                    "INSERT INTO run_events (run_id, seq, type, payload) VALUES (%s, %s, %s, %s::jsonb)",
                    (run_id, seq, kind, json.dumps(payload)),
                )
                return seq

    def record_resolve(self, run_id: str, event_id: str, resolved_at, revise: bool) -> str:
        import psycopg

        with psycopg.connect(self.url) as conn:
            try:
                with conn.transaction():
                    row = conn.execute(
                        "SELECT status, input_event FROM runs WHERE id = %s FOR UPDATE",
                        (run_id,),
                    ).fetchone()
                    if not row:
                        return "ignored"
                    seen = conn.execute(
                        """SELECT 1 FROM run_events
                           WHERE run_id = %s AND type = 'incident.resolved' AND payload->>'event_id' = %s""",
                        (run_id, event_id),
                    ).fetchone()
                    if seen:
                        return "duplicate"
                    current = dict(row[1] or {})
                    current["resolved_at"] = resolved_at
                    status, stage, mode, action = row[0], None, None, "resolved"
                    if revise:
                        current["pending_revise"] = True
                        action = "recorded"
                        if row[0] in {"completed", "partial"}:
                            current["event"] = "incident_resolve"
                            status, stage, mode, action = "queued", "revise", "baseline", "revise"
                    assignments = ["input_event = %s::jsonb", "status = %s"]
                    values = [json.dumps(current), status]
                    if stage is not None:
                        assignments.append("stage = %s")
                        values.append(stage)
                    if mode is not None:
                        assignments.append("mode = %s")
                        values.append(mode)
                    values.append(run_id)
                    conn.execute(f"UPDATE runs SET {', '.join(assignments)} WHERE id = %s", values)
                    seq = conn.execute(
                        "SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = %s",
                        (run_id,),
                    ).fetchone()[0]
                    conn.execute(
                        "INSERT INTO run_events (run_id, seq, type, payload) VALUES (%s, %s, 'incident.resolved', %s::jsonb)",
                        (run_id, seq, json.dumps({"event_id": event_id, "resolved_at": resolved_at})),
                    )
                    return action
            except psycopg.errors.UniqueViolation:
                return "duplicate"

    def commit_report(self, run_id: str, version: int, report: dict, status: str, stop_reason, usage: dict, queue_revise: bool = False, cited: list | None = None, replace_partial: bool = False, callback: dict | None = None) -> str:
        import psycopg

        with psycopg.connect(self.url) as conn:
            with conn.transaction():
                row = conn.execute(
                    "SELECT status, input_event FROM runs WHERE id = %s FOR UPDATE",
                    (run_id,),
                ).fetchone()
                if not row:
                    return "ignored"
                if row[0] == "cancelled":
                    return "cancelled"
                inserted = conn.execute(
                    """INSERT INTO reports (run_id, version, report) VALUES (%s, %s, %s::jsonb)
                       ON CONFLICT DO NOTHING""",
                    (run_id, version, json.dumps(report)),
                )
                if inserted.rowcount != 1:
                    replaced = None
                    if replace_partial and version == 1:
                        replaced = conn.execute(
                            """UPDATE reports SET report = %s::jsonb
                               WHERE run_id = %s AND version = 1 AND report->>'status' = 'partial'""",
                            (json.dumps(report), run_id),
                        )
                    if replaced is None or replaced.rowcount != 1:
                        conn.execute(
                            """UPDATE runs SET status = 'failed', failure_reason = 'report_conflict', stage = 'persist', finished_at = now()
                               WHERE id = %s AND status <> 'cancelled'""",
                            (run_id,),
                        )
                        self._append_tx(conn, run_id, "run.failed", {"reason": "report_conflict"})
                        return "conflict"
                current = dict(row[1] or {})
                final_status, final_stage, final_mode = status, "persist", None
                if queue_revise and version == 1 and current.get("pending_revise"):
                    current["event"] = "incident_resolve"
                    final_status, final_stage, final_mode = "queued", "revise", "baseline"
                assignments = ["status = %s", "stop_reason = %s", "failure_reason = NULL", "stage = %s", "finished_at = now()", "usage = %s::jsonb", "input_event = %s::jsonb"]
                values = [final_status, stop_reason, final_stage, json.dumps(usage or {}), json.dumps(current)]
                if final_mode is not None:
                    assignments.append("mode = %s")
                    values.append(final_mode)
                values.append(run_id)
                conn.execute(
                    f"UPDATE runs SET {', '.join(assignments)} WHERE id = %s AND status <> 'cancelled'",
                    values,
                )
                self._append_tx(conn, run_id, "report.saved", {"version": version, "status": status, "cited": cited or []})
                # 与报告写入同一个事务：报告落库了，回调就一定已经排队。
                self._insert_callback_tx(conn, callback)
                return "saved"

    def _append_tx(self, conn, run_id: str, kind: str, payload: dict) -> int:
        seq = conn.execute(
            "SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = %s",
            (run_id,),
        ).fetchone()[0]
        conn.execute(
            "INSERT INTO run_events (run_id, seq, type, payload) VALUES (%s, %s, %s, %s::jsonb)",
            (run_id, seq, kind, json.dumps(payload)),
        )
        return seq

    def list_events(self, run_id: str, after: int = 0) -> list[dict]:
        rows = self.conn.execute(
            "SELECT seq, type, payload, ts FROM run_events WHERE run_id = %s AND seq > %s ORDER BY seq",
            (run_id, after),
        ).fetchall()
        return [{"seq": r[0], "type": r[1], "payload": r[2], "ts": r[3].isoformat()} for r in rows]

    def save_evidence(self, run_id: str, evidence: dict) -> None:
        self.conn.execute(
            """INSERT INTO run_evidence (run_id, evidence) VALUES (%s, %s::jsonb)
               ON CONFLICT (run_id) DO UPDATE SET evidence = EXCLUDED.evidence, updated_at = now()""",
            (run_id, json.dumps(evidence)),
        )

    def get_evidence(self, run_id: str) -> dict:
        row = self.conn.execute("SELECT evidence FROM run_evidence WHERE run_id = %s", (run_id,)).fetchone()
        return row[0] if row else {}

    def save_report(self, run_id: str, version: int, report: dict) -> bool:
        cur = self.conn.execute(
            """INSERT INTO reports (run_id, version, report) VALUES (%s, %s, %s::jsonb)
               ON CONFLICT DO NOTHING""",
            (run_id, version, json.dumps(report)),
        )
        return cur.rowcount == 1

    def latest_report(self, run_id: str) -> dict | None:
        row = self.conn.execute(
            "SELECT version, report, created_at FROM reports WHERE run_id = %s ORDER BY version DESC LIMIT 1",
            (run_id,),
        ).fetchone()
        if not row:
            return None
        return {"version": row[0], "report": row[1], "created_at": row[2].isoformat()}

    def list_runs(self) -> list[dict]:
        rows = self.conn.execute("SELECT id::text FROM runs ORDER BY created_at DESC").fetchall()
        return [self.get(r[0]) for r in rows]

    def claim_queued(self) -> dict | None:
        row = self.conn.execute(
            """UPDATE runs SET status = 'running', started_at = now()
               WHERE id = (SELECT id FROM runs WHERE status = 'queued' ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
               RETURNING id::text"""
        ).fetchone()
        return self.get(row[0]) if row else None

    def running_ids(self) -> list[str]:
        rows = self.conn.execute("SELECT id::text FROM runs WHERE status = 'running'").fetchall()
        return [r[0] for r in rows]


def new_run_id() -> str:
    return str(uuid.uuid4())


def apply_schema(url: str) -> None:
    import psycopg

    sql = Path(__file__).resolve().parents[2].joinpath("schema.sql").read_text()
    with psycopg.connect(url, autocommit=True) as conn:
        conn.execute(sql)


def ensure_database(url: str) -> None:
    """Apply schema.sql. Create the database when the role is allowed to."""
    import psycopg
    from urllib.parse import urlparse

    parsed = urlparse(url)
    name = parsed.path.lstrip("/") or "alertflow_agent"
    admin = parsed._replace(path="/postgres").geturl()
    try:
        with psycopg.connect(admin, autocommit=True) as conn:
            exists = conn.execute("SELECT 1 FROM pg_database WHERE datname = %s", (name,)).fetchone()
            if not exists:
                conn.execute(f'CREATE DATABASE "{name}"')
    except Exception:
        pass
    try:
        apply_schema(url)
    except Exception as exc:
        raise RuntimeError(
            f"could not apply schema to {name}; create that database first if this role cannot CREATE DATABASE"
        ) from exc
