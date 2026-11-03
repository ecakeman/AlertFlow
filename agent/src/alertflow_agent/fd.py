import os
from datetime import datetime, timezone

import psycopg
from pgvector.psycopg import register_vector_async

from alertflow_agent.schemas import FaultSearchIndex
from alertflow_agent.textutil import embed, rrf, search_text, segment, token_overlap


def stable_fd_id(episode_id: str) -> str:
    import uuid

    return str(uuid.uuid5(uuid.UUID("6f0c1a4e-1b3a-4b8e-9c1a-0000000000fd"), episode_id))


async def connect(dsn: str | None = None):
    conn = await psycopg.AsyncConnection.connect(dsn or os.environ["ALERTFLOW_DATABASE_URL"])
    await register_vector_async(conn)
    return conn


async def save_fault_document(conn, episode_id: str, service_id: str, investigation: str, report: dict, index: FaultSearchIndex) -> str:
    fd_id = stable_fd_id(episode_id)
    text = search_text(index.summary, index.symptoms, index.observations, index.diagnosis)
    vector = embed(text)
    now = datetime.now(timezone.utc)
    async with conn.cursor() as cur:
        await cur.execute(
            """
            INSERT INTO cognitive.fault_documents
            (fd_id, episode_id, service_id, archived_at, investigation_content, diagnosis_report,
             search_index, search_text, search_vector, embedding, embedding_model, index_status, created_at, updated_at)
            VALUES (%s,%s,%s,%s,%s,%s::jsonb,%s::jsonb,%s,to_tsvector('simple', %s),%s,%s,'BUILDING',%s,%s)
            ON CONFLICT (episode_id) DO UPDATE
            SET investigation_content=EXCLUDED.investigation_content,
                diagnosis_report=EXCLUDED.diagnosis_report,
                search_index=EXCLUDED.search_index,
                search_text=EXCLUDED.search_text,
                search_vector=EXCLUDED.search_vector,
                embedding=EXCLUDED.embedding,
                embedding_model=EXCLUDED.embedding_model,
                index_status='BUILDING',
                index_error=NULL,
                updated_at=EXCLUDED.updated_at
            RETURNING fd_id
            """,
            (fd_id, episode_id, service_id, now, investigation, psycopg.types.json.Json(report),
             psycopg.types.json.Json(index.model_dump()), text, segment(text), vector,
             "deterministic-v1", now, now),
        )
        stored = (await cur.fetchone())[0]
        await cur.execute(
            """
            UPDATE cognitive.fault_documents
            SET index_status='READY', updated_at=%s
            WHERE fd_id=%s AND search_vector IS NOT NULL AND embedding IS NOT NULL
            """,
            (now, stored),
        )
    await conn.commit()
    return str(stored)


async def _ranked(conn, sql: str, params: tuple) -> list[str]:
    async with conn.cursor() as cur:
        await cur.execute(sql, params)
        rows = await cur.fetchall()
    return [str(row[0]) for row in rows]


async def search_faults(conn, service_id: str, query: str, top_k: int = 5) -> list[dict]:
    q = segment(query)
    vector = embed(query)
    same_bm25 = await _ranked(conn, """
        SELECT fd_id FROM cognitive.fault_documents
        WHERE service_id=%s AND index_status='READY' AND search_vector @@ plainto_tsquery('simple', %s)
        ORDER BY ts_rank_cd(search_vector, plainto_tsquery('simple', %s)) DESC LIMIT 20
    """, (service_id, q, q))
    other_bm25 = await _ranked(conn, """
        SELECT fd_id FROM cognitive.fault_documents
        WHERE service_id<>%s AND index_status='READY' AND search_vector @@ plainto_tsquery('simple', %s)
        ORDER BY ts_rank_cd(search_vector, plainto_tsquery('simple', %s)) DESC LIMIT 20
    """, (service_id, q, q))
    same_vec = await _ranked(conn, """
        SELECT fd_id FROM cognitive.fault_documents
        WHERE service_id=%s AND index_status='READY' AND embedding IS NOT NULL
        ORDER BY embedding <=> %s::vector LIMIT 20
    """, (service_id, vector))
    other_vec = await _ranked(conn, """
        SELECT fd_id FROM cognitive.fault_documents
        WHERE service_id<>%s AND index_status='READY' AND embedding IS NOT NULL
        ORDER BY embedding <=> %s::vector LIMIT 20
    """, (service_id, vector))
    fused = rrf([same_bm25, same_vec, other_bm25, other_vec])
    if not fused:
        return []
    ids = list(fused)
    async with conn.cursor() as cur:
        await cur.execute(
            """
            SELECT fd_id, service_id, search_index, search_text, investigation_content, diagnosis_report
            FROM cognitive.fault_documents WHERE fd_id = ANY(%s)
            """,
            (ids,),
        )
        rows = await cur.fetchall()
    ranked = []
    for fd_id, svc, index, text, investigation, report in rows:
        ranked.append({
            "fd_id": str(fd_id),
            "service_id": str(svc),
            "summary": (index or {}).get("summary", ""),
            "matched_content": text,
            "investigation_content": investigation,
            "diagnosis_report": report,
            "rrf": fused[str(fd_id)],
            "rerank": token_overlap(query, text or ""),
        })
    ranked.sort(key=lambda item: (item["rerank"], item["rrf"]), reverse=True)
    for item in ranked:
        item.pop("rrf")
        item.pop("rerank")
    return ranked[:top_k]
