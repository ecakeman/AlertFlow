import hashlib
from datetime import datetime, timezone

from alertflow_agent.textutil import content_hash


def _now():
    return datetime.now(timezone.utc)


def _json(value):
    import psycopg

    if value is None:
        return None
    return psycopg.types.json.Json(value)


async def read_artifact(conn, episode_id: str, artifact_type: str) -> dict | None:
    async with conn.cursor() as cur:
        await cur.execute(
            """
            SELECT version, content_type, content_text, content_json, content_hash, execution_generation
            FROM cognitive.episode_artifacts
            WHERE episode_id=%s AND artifact_type=%s
            """,
            (episode_id, artifact_type),
        )
        row = await cur.fetchone()
    if row is None:
        return None
    return {
        "version": row[0],
        "content_type": row[1],
        "content_text": row[2],
        "content_json": row[3],
        "content_hash": row[4],
        "execution_generation": row[5],
    }


async def write_artifact(conn, episode_id: str, artifact_type: str, content_type: str, content_text: str | None,
                         content_json, task_id: str, generation: int, expected_version: int | None) -> dict:
    digest_source = content_text if content_text is not None else str(content_json)
    digest = content_hash(digest_source)
    current = await read_artifact(conn, episode_id, artifact_type)
    now = _now()
    if current and current["content_hash"] == digest and current["version"] == (expected_version or current["version"]):
        return current
    if current is None:
        version = 1
        if expected_version not in (None, 0):
            raise ValueError("artifact version conflict")
        async with conn.cursor() as cur:
            await cur.execute(
                """
                INSERT INTO cognitive.episode_artifacts
                (episode_id, artifact_type, version, content_type, content_text, content_json, content_hash,
                 updated_by_task_id, execution_generation, updated_at)
                VALUES (%s,%s,%s,%s,%s,%s::jsonb,%s,%s,%s,%s)
                """,
                (episode_id, artifact_type, version, content_type, content_text, _json(content_json), digest, task_id, generation, now),
            )
    else:
        if expected_version is not None and current["version"] != expected_version:
            raise ValueError("artifact version conflict")
        version = current["version"] + 1
        async with conn.cursor() as cur:
            await cur.execute(
                """
                UPDATE cognitive.episode_artifacts
                SET version=%s, content_type=%s, content_text=%s, content_json=%s::jsonb, content_hash=%s,
                    updated_by_task_id=%s, execution_generation=%s, updated_at=%s
                WHERE episode_id=%s AND artifact_type=%s AND version=%s
                """,
                (version, content_type, content_text, _json(content_json), digest, task_id, generation, now,
                 episode_id, artifact_type, current["version"]),
            )
            if cur.rowcount != 1:
                raise ValueError("artifact version conflict")
    await conn.commit()
    saved = await read_artifact(conn, episode_id, artifact_type)
    if saved is None:
        raise RuntimeError("artifact write was not visible")
    return saved


def investigation_round_marker(task_id: str, generation: int) -> str:
    return f"<!-- task:{task_id} gen:{generation} -->"


def render_investigation(service_name: str, started_at: str, existing: str | None, task_id: str, generation: int,
                         findings: str, understanding: str, questions: str) -> str:
    marker = investigation_round_marker(task_id, generation)
    if existing and marker in existing:
        return existing
    overview = f"# 故障概况\n\n- 服务：{service_name}\n- 开始时间：{started_at}\n"
    if existing and "# 调查发现" in existing:
        updated = existing.rstrip() + f"\n\n## 轮次\n\n{marker}\n\n{findings}\n"
        updated = _replace_section(updated, "阶段性认识", understanding)
        updated = _replace_section(updated, "待确认问题", questions)
        return updated
    return (
        f"{overview}\n"
        f"# 调查发现\n\n## 轮次\n\n{marker}\n\n{findings}\n\n"
        f"# 阶段性认识\n\n{understanding}\n\n"
        f"# 待确认问题\n\n{questions}\n"
    )


def _replace_section(text: str, title: str, body: str) -> str:
    heading = f"# {title}"
    start = text.find(heading)
    if start < 0:
        return text + f"\n{heading}\n\n{body}\n"
    rest = text[start + len(heading):]
    nxt = rest.find("\n# ")
    if nxt < 0:
        return text[:start] + f"{heading}\n\n{body}\n"
    return text[:start] + f"{heading}\n\n{body}\n" + rest[nxt + 1:]


def round_marker_hash(task_id: str, generation: int) -> str:
    return hashlib.sha256(f"{task_id}:{generation}".encode()).hexdigest()
