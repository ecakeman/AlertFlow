from datetime import datetime, timezone


def _now():
    return datetime.now(timezone.utc)


def accept_task(conn, task: dict) -> dict:
    """按 task_id 幂等接收。更高代次更新接收记录并允许重新执行。"""
    with conn.cursor() as cur:
        cur.execute(
            """
            SELECT execution_generation, status, thread_id FROM cognitive.agent_task_receipts WHERE task_id=%s
            """,
            (task["task_id"],),
        )
        row = cur.fetchone()
        now = _now()
        thread_id = f"{'diagnosis' if task['task_type']=='DIAGNOSIS' else 'archive'}:{task['episode_id']}"
        if row is None:
            cur.execute(
                """
                INSERT INTO cognitive.agent_task_receipts
                (task_id, episode_id, task_type, execution_generation, thread_id, status,
                 input_version_from, input_version_to, final_input_version, accepted_at, callback_state, updated_at)
                VALUES (%s,%s,%s,%s,%s,'ACCEPTED',%s,%s,%s,%s,'NOT_SENT',%s)
                """,
                (task["task_id"], task["episode_id"], task["task_type"], task["execution_generation"], thread_id,
                 task.get("input_version_from"), task.get("input_version_to"), task.get("final_input_version"), now, now),
            )
            conn.commit()
            return {"accepted": True, "duplicate": False, "status": "ACCEPTED", "thread_id": thread_id}
        generation, status, existing_thread = row
        if generation == task["execution_generation"]:
            conn.rollback()
            return {"accepted": True, "duplicate": True, "status": status, "thread_id": existing_thread}
        if task["execution_generation"] < generation:
            conn.rollback()
            return {"accepted": False, "duplicate": True, "status": status, "thread_id": existing_thread, "message": "stale generation"}
        cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET execution_generation=%s, status='ACCEPTED', callback_state='NOT_SENT', updated_at=%s,
                input_version_from=%s, input_version_to=%s, final_input_version=%s
            WHERE task_id=%s
            """,
            (task["execution_generation"], now, task.get("input_version_from"), task.get("input_version_to"),
             task.get("final_input_version"), task["task_id"]),
        )
    conn.commit()
    return {"accepted": True, "duplicate": False, "status": "ACCEPTED", "thread_id": thread_id}


def mark_running(conn, task_id: str):
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET status='RUNNING', started_at=COALESCE(started_at, %s), updated_at=%s
            WHERE task_id=%s
            """,
            (_now(), _now(), task_id),
        )
    conn.commit()


def mark_finished(conn, task_id: str, status: str, result: dict, callback_state: str):
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET status=%s, completed_at=%s, result_json=%s::jsonb, callback_state=%s, updated_at=%s
            WHERE task_id=%s
            """,
            (status, _now(), _json(result), callback_state, _now(), task_id),
        )
    conn.commit()


def unfinished(conn) -> list[dict]:
    with conn.cursor() as cur:
        cur.execute(
            """
            SELECT task_id, episode_id, task_type, execution_generation, thread_id,
                   input_version_from, input_version_to, final_input_version, status
            FROM cognitive.agent_task_receipts
            WHERE status IN ('ACCEPTED','RUNNING')
            ORDER BY accepted_at
            """
        )
        cols = ["task_id", "episode_id", "task_type", "execution_generation", "thread_id",
                "input_version_from", "input_version_to", "final_input_version", "status"]
        return [dict(zip(cols, row, strict=True)) for row in cur.fetchall()]


def _json(value):
    import psycopg
    return psycopg.types.json.Json(value)


def advisory_lock(conn, thread_id: str):
    key = int.from_bytes(__import__("hashlib").sha256(thread_id.encode()).digest()[:8], "big", signed=True)
    with conn.cursor() as cur:
        cur.execute("SELECT pg_advisory_lock(%s)", (key,))
    return key


def advisory_unlock(conn, key: int):
    with conn.cursor() as cur:
        cur.execute("SELECT pg_advisory_unlock(%s)", (key,))
