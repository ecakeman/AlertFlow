import hashlib
from datetime import datetime, timezone


def _now():
    return datetime.now(timezone.utc)


def _json(value):
    import psycopg
    return psycopg.types.json.Json(value)


def thread_id_of(task: dict) -> str:
    kind = "diagnosis" if task["task_type"] == "DIAGNOSIS" else "archive"
    return f"{kind}:{task['episode_id']}"


def advisory_key(thread_id: str) -> int:
    return int.from_bytes(hashlib.sha256(thread_id.encode()).digest()[:8], "big", signed=True)


async def accept_task(conn, task: dict) -> dict:
    """按 task_id 幂等接收。更高代次更新接收记录并允许重新执行。"""
    async with conn.cursor() as cur:
        await cur.execute(
            """
            SELECT execution_generation, status, thread_id FROM cognitive.agent_task_receipts WHERE task_id=%s
            """,
            (task["task_id"],),
        )
        row = await cur.fetchone()
        now = _now()
        thread_id = thread_id_of(task)
        if row is None:
            await cur.execute(
                """
                INSERT INTO cognitive.agent_task_receipts
                (task_id, episode_id, task_type, execution_generation, thread_id, status,
                 input_version_from, input_version_to, final_input_version, accepted_at, callback_state, updated_at)
                VALUES (%s,%s,%s,%s,%s,'ACCEPTED',%s,%s,%s,%s,'NOT_SENT',%s)
                """,
                (task["task_id"], task["episode_id"], task["task_type"], task["execution_generation"], thread_id,
                 task.get("input_version_from"), task.get("input_version_to"), task.get("final_input_version"), now, now),
            )
            await conn.commit()
            return {"accepted": True, "duplicate": False, "status": "ACCEPTED", "thread_id": thread_id}
        generation, status, existing_thread = row
        if generation == task["execution_generation"]:
            await conn.rollback()
            return {"accepted": True, "duplicate": True, "status": status, "thread_id": existing_thread}
        if task["execution_generation"] < generation:
            await conn.rollback()
            return {"accepted": False, "duplicate": True, "status": status, "thread_id": existing_thread, "message": "stale generation"}
        await cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET execution_generation=%s, status='ACCEPTED', callback_state='NOT_SENT', updated_at=%s,
                input_version_from=%s, input_version_to=%s, final_input_version=%s
            WHERE task_id=%s
            """,
            (task["execution_generation"], now, task.get("input_version_from"), task.get("input_version_to"),
             task.get("final_input_version"), task["task_id"]),
        )
    await conn.commit()
    return {"accepted": True, "duplicate": False, "status": "ACCEPTED", "thread_id": thread_id}


async def mark_running(conn, task_id: str):
    now = _now()
    async with conn.cursor() as cur:
        await cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET status='RUNNING', started_at=COALESCE(started_at, %s), updated_at=%s
            WHERE task_id=%s
            """,
            (now, now, task_id),
        )
    await conn.commit()


async def mark_finished(conn, task_id: str, status: str, result: dict, callback_state: str):
    now = _now()
    async with conn.cursor() as cur:
        await cur.execute(
            """
            UPDATE cognitive.agent_task_receipts
            SET status=%s, completed_at=%s, result_json=%s::jsonb, callback_state=%s, updated_at=%s
            WHERE task_id=%s
            """,
            (status, now, _json(result), callback_state, now, task_id),
        )
    await conn.commit()


async def unfinished(conn) -> list[dict]:
    async with conn.cursor() as cur:
        await cur.execute(
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
        rows = await cur.fetchall()
    out = []
    for row in rows:
        item = dict(zip(cols, row, strict=True))
        item["task_id"] = str(item["task_id"])
        item["episode_id"] = str(item["episode_id"])
        out.append(item)
    return out


async def advisory_lock(conn, thread_id: str) -> int:
    """同一条连接上的会话锁。持锁期间不能把连接交还连接池。"""
    key = advisory_key(thread_id)
    async with conn.cursor() as cur:
        await cur.execute("SELECT pg_advisory_lock(%s)", (key,))
        await cur.fetchone()
    return key


async def advisory_unlock(conn, key: int):
    async with conn.cursor() as cur:
        await cur.execute("SELECT pg_advisory_unlock(%s)", (key,))
        await cur.fetchone()
