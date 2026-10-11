from __future__ import annotations

import json
import threading
import uuid
from datetime import datetime, timezone
from pathlib import Path


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


class MemoryStore:
    def __init__(self) -> None:
        self.lock = threading.RLock()
        self.runs: dict[str, dict] = {}
        self.by_key: dict[str, str] = {}
        self.events: dict[str, list[dict]] = {}
        self.evidence: dict[str, dict] = {}
        self.reports: dict[str, list[dict]] = {}

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

    def finish(self, run_id: str, **fields) -> bool:
        with self.lock:
            row = self.runs[run_id]
            if row["status"] == "cancelled":
                return False
            row.update(fields)
            return True

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

    def commit_report(self, run_id: str, version: int, report: dict, status: str, stop_reason, usage: dict, queue_revise: bool = False, cited: list | None = None, replace_partial: bool = False) -> str:
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

    def finish(self, run_id: str, **fields) -> bool:
        return self._update_where(run_id, "status <> 'cancelled'", fields)

    def transition(self, run_id: str, allowed: set[str], **fields) -> bool:
        marks = ", ".join(["%s"] * len(allowed))
        return self._update_where(run_id, f"status IN ({marks})", fields, list(allowed))

    def _update_where(self, run_id: str, clause: str, fields: dict, extra: list | None = None) -> bool:
        if not fields:
            return False
        sets = []
        vals = []
        for key, value in fields.items():
            if key in {"input_event", "usage"}:
                sets.append(f"{key} = %s::jsonb")
                vals.append(json.dumps(value))
            else:
                sets.append(f"{key} = %s")
                vals.append(value)
        vals.extend(extra or [])
        vals.append(run_id)
        cur = self.conn.execute(f"UPDATE runs SET {', '.join(sets)} WHERE {clause} AND id = %s", vals)
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

    def commit_report(self, run_id: str, version: int, report: dict, status: str, stop_reason, usage: dict, queue_revise: bool = False, cited: list | None = None, replace_partial: bool = False) -> str:
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
