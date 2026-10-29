"""Adapter task rows. The goose migration owns the PostgreSQL table."""

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone


class MemoryLedger:
    def __init__(self):
        self.rows: dict[str, dict] = {}
        self.by_task: dict[str, str] = {}

    def claim(self, message_id: str, task_id: str, timeout_seconds: int) -> bool:
        if message_id in self.rows:
            return False
        deadline = datetime.now(timezone.utc) + timedelta(seconds=timeout_seconds)
        self.rows[message_id] = {
            "message_id": message_id,
            "task_id": task_id,
            "status": "working",
            "artifact": None,
            "task_json": None,
            "cancelled": False,
            "deadline_at": deadline.isoformat(),
        }
        self.by_task[task_id] = message_id
        return True

    def get_message(self, message_id: str) -> dict | None:
        row = self.rows.get(message_id)
        return dict(row) if row else None

    def get_task(self, task_id: str) -> dict | None:
        message_id = self.by_task.get(task_id)
        return self.get_message(message_id) if message_id else None

    def save_task(self, task_id: str, status: str, artifact: dict | None, task_json: dict | None, cancelled: bool = False) -> None:
        message_id = self.by_task.get(task_id)
        if message_id is None:
            return
        row = self.rows[message_id]
        if row["task_id"] != task_id:
            return
        row["status"] = status
        row["artifact"] = artifact
        row["task_json"] = task_json
        row["cancelled"] = cancelled

    def alias(self, message_id: str, task_id: str) -> None:
        if message_id in self.rows:
            self.by_task[task_id] = message_id

    def cancel(self, task_id: str) -> None:
        row = self.get_task(task_id)
        if row:
            self.rows[row["message_id"]]["cancelled"] = True
            self.rows[row["message_id"]]["status"] = "canceled"


class PostgresLedger:
    def __init__(self, dsn: str):
        self.dsn = dsn

    def _connect(self):
        import psycopg

        return psycopg.connect(self.dsn)

    def claim(self, message_id: str, task_id: str, timeout_seconds: int) -> bool:
        with self._connect() as conn:
            row = conn.execute(
                """
                INSERT INTO a2a_adapter_tasks (message_id, task_id, status, deadline_at)
                VALUES (%s, %s, 'working', now() + make_interval(secs => %s))
                ON CONFLICT (message_id) DO NOTHING
                RETURNING message_id
                """,
                (message_id, task_id, int(timeout_seconds)),
            ).fetchone()
            conn.commit()
            return row is not None

    def get_message(self, message_id: str) -> dict | None:
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT message_id, task_id, status, artifact, task_json, cancelled, deadline_at
                FROM a2a_adapter_tasks WHERE message_id = %s
                """,
                (message_id,),
            ).fetchone()
        return _row(row)

    def get_task(self, task_id: str) -> dict | None:
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT message_id, task_id, status, artifact, task_json, cancelled, deadline_at
                FROM a2a_adapter_tasks WHERE task_id = %s
                UNION ALL
                SELECT t.message_id, t.task_id, t.status, t.artifact, t.task_json, t.cancelled, t.deadline_at
                FROM a2a_adapter_task_aliases a
                JOIN a2a_adapter_tasks t ON t.message_id = a.message_id
                WHERE a.task_id = %s
                LIMIT 1
                """,
                (task_id, task_id),
            ).fetchone()
        return _row(row)

    def alias(self, message_id: str, task_id: str) -> None:
        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO a2a_adapter_task_aliases (task_id, message_id)
                VALUES (%s, %s)
                ON CONFLICT (task_id) DO NOTHING
                """,
                (task_id, message_id),
            )
            conn.commit()

    def save_task(self, task_id: str, status: str, artifact: dict | None, task_json: dict | None, cancelled: bool = False) -> None:
        with self._connect() as conn:
            conn.execute(
                """
                UPDATE a2a_adapter_tasks
                SET status = %s, artifact = %s::jsonb, task_json = %s::jsonb,
                    cancelled = %s, updated_at = now()
                WHERE task_id = %s
                """,
                (status, json.dumps(artifact) if artifact is not None else None,
                 json.dumps(task_json) if task_json is not None else None, cancelled, task_id),
            )
            conn.commit()

    def cancel(self, task_id: str) -> None:
        with self._connect() as conn:
            conn.execute(
                """
                UPDATE a2a_adapter_tasks
                SET cancelled = true, status = 'canceled', updated_at = now()
                WHERE task_id = %s
                """,
                (task_id,),
            )
            conn.commit()


def _row(row) -> dict | None:
    if row is None:
        return None
    artifact, task_json = row[3], row[4]
    if isinstance(artifact, str):
        artifact = json.loads(artifact)
    if isinstance(task_json, str):
        task_json = json.loads(task_json)
    deadline = row[6]
    return {
        "message_id": row[0],
        "task_id": row[1],
        "status": row[2],
        "artifact": artifact,
        "task_json": task_json,
        "cancelled": row[5],
        "deadline_at": deadline.isoformat() if hasattr(deadline, "isoformat") else deadline,
    }
