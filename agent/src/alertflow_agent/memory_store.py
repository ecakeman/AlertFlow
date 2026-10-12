"""长期记忆的存储层：事件簿（episodes）与服务档案（service_profile）。

独立成模块，不并进 db.py——运行状态（runs/reports）与长期记忆是两件事，
读写模式、失败影响面都不同。存储层只认模型，不认自由字典。

向量检索走 pgvector 的余弦算子 `<=>`；未配置嵌入时 `embedding` 为 NULL，
这些行不参与向量召回，但仍可被预筛/精确读取命中。
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timedelta, timezone

from .schemas.memory import EpisodeCard, EpisodeOutcome, Conclusion, KnownCause, KnownPattern, ServiceBaseline, ServiceProfile, SignatureItem, Symptom, EvidenceSignature

log = logging.getLogger("alertflow_agent")


def _vec(values: list[float]) -> str:
    return "[" + ",".join(f"{v:.6f}" for v in values) + "]"


def _row_to_card(row: dict) -> dict:
    return {
        "id": row["id"],
        "service_id": row["service_id"],
        "profile_key": row.get("profile_key") or "",
        "started_at": row["started_at"].isoformat() if row.get("started_at") else None,
        "severity": row.get("severity"),
        "card": row.get("card") or "",
        "conclusion": row.get("conclusion") or {},
    }


class MemoryRepo:
    """Postgres + pgvector 实现。"""

    def __init__(self, url: str) -> None:
        import psycopg

        self.url = url
        self.conn = psycopg.connect(url, autocommit=True)

    # ---- 事件簿 ----
    def save_episode(self, card: EpisodeCard, embedding: list[float] | None, embed_model: str = "") -> str:
        payload = card.model_dump(mode="json")
        row = self.conn.execute(
            """
            INSERT INTO episodes (service_id, target_endpoint, profile_key, incident_id, run_id,
                started_at, resolved_at, duration_s, severity, outcome, symptom, signature,
                conclusion, evidence_ids, card, card_hash, version)
            VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s::jsonb,%s::jsonb,%s::jsonb,%s,%s,%s,%s)
            ON CONFLICT (service_id, target_endpoint, profile_key, incident_id)
            DO UPDATE SET severity=EXCLUDED.severity, outcome=EXCLUDED.outcome,
                symptom=EXCLUDED.symptom, signature=EXCLUDED.signature,
                conclusion=EXCLUDED.conclusion, evidence_ids=EXCLUDED.evidence_ids,
                card=EXCLUDED.card, card_hash=EXCLUDED.card_hash, version=episodes.version+1
            RETURNING id
            """,
            (
                card.service_id, card.target_endpoint, card.profile_key, card.incident_id, card.run_id,
                card.started_at, card.resolved_at, card.duration_s, card.severity,
                card.outcome.resolved_by,
                json.dumps(payload["symptom"], ensure_ascii=False),
                json.dumps(payload["signature"], ensure_ascii=False),
                json.dumps(payload["conclusion"], ensure_ascii=False),
                card.evidence_ids, card.render(), "", card.version,
            ),
        ).fetchone()
        episode_id = str(row[0])
        if embedding:
            self.conn.execute(
                "UPDATE episodes SET embedding=%s::vector, embed_model=%s WHERE id=%s",
                (_vec(embedding), embed_model, episode_id),
            )
        return episode_id

    def get_episode(self, episode_id: str) -> dict | None:
        row = self.conn.execute(
            "SELECT id, service_id, started_at, severity, card, conclusion FROM episodes WHERE id=%s",
            (episode_id,),
        ).fetchone()
        if not row:
            return None
        return _row_to_card({"id": str(row[0]), "service_id": row[1], "started_at": row[2],
                             "severity": row[3], "card": row[4], "conclusion": row[5]})

    def list_recent(self, service_id: str, days: int, limit: int) -> list[dict]:
        since = datetime.now(timezone.utc) - timedelta(days=days)
        rows = self.conn.execute(
            """SELECT id, service_id, started_at, severity, card, conclusion, profile_key FROM episodes
               WHERE service_id=%s AND started_at >= %s ORDER BY started_at DESC LIMIT %s""",
            (service_id, since, limit),
        ).fetchall()
        return [_row_to_card({"id": str(r[0]), "service_id": r[1], "started_at": r[2],
                              "severity": r[3], "card": r[4], "conclusion": r[5], "profile_key": r[6]}) for r in rows]

    def search(self, service_id: str, embedding: list[float], k: int) -> list[tuple[dict, float]]:
        """pgvector 余弦召回（带服务预筛）。返回 [(episode, 相似度)]，相似度越大越像。"""
        rows = self.conn.execute(
            """SELECT id, service_id, started_at, severity, card, conclusion, profile_key,
                      1 - (embedding <=> %s::vector) AS score
               FROM episodes
               WHERE service_id=%s AND embedding IS NOT NULL
               ORDER BY embedding <=> %s::vector
               LIMIT %s""",
            (_vec(embedding), service_id, _vec(embedding), k),
        ).fetchall()
        return [
            (_row_to_card({"id": str(r[0]), "service_id": r[1], "started_at": r[2],
                           "severity": r[3], "card": r[4], "conclusion": r[5], "profile_key": r[6]}), float(r[7]))
            for r in rows
        ]

    # ---- 服务档案 ----
    def get_profile(self, service_id: str) -> dict | None:
        row = self.conn.execute(
            "SELECT service_id, baseline, known_causes, known_patterns, episode_count FROM service_profile WHERE service_id=%s",
            (service_id,),
        ).fetchone()
        if not row:
            return None
        return {"service_id": row[0], "baseline": row[1], "known_causes": row[2],
                "known_patterns": row[3], "episode_count": row[4]}

    def upsert_profile(self, profile: ServiceProfile) -> None:
        payload = profile.model_dump(mode="json")
        self.conn.execute(
            """INSERT INTO service_profile (service_id, baseline, known_causes, known_patterns, episode_count, updated_at)
               VALUES (%s,%s::jsonb,%s::jsonb,%s::jsonb,%s,now())
               ON CONFLICT (service_id) DO UPDATE SET baseline=EXCLUDED.baseline,
                 known_causes=EXCLUDED.known_causes, known_patterns=EXCLUDED.known_patterns,
                 episode_count=EXCLUDED.episode_count, updated_at=now()""",
            (profile.service_id, json.dumps(payload["baseline"], ensure_ascii=False),
             json.dumps(payload["known_causes"], ensure_ascii=False),
             json.dumps(payload["known_patterns"], ensure_ascii=False), profile.episode_count),
        )

    def count_episodes(self, service_id: str) -> int:
        row = self.conn.execute("SELECT count(*) FROM episodes WHERE service_id=%s", (service_id,)).fetchone()
        return int(row[0]) if row else 0

    def record_correction(self, episode_id: str, field: str, from_value, to_value, reason: str = "") -> None:
        self.conn.execute(
            "INSERT INTO episode_corrections (episode_id, field, from_value, to_value, reason) VALUES (%s,%s,%s::jsonb,%s::jsonb,%s)",
            (episode_id, field, json.dumps(from_value, ensure_ascii=False),
             json.dumps(to_value, ensure_ascii=False), reason),
        )


class InMemoryMemoryRepo(MemoryRepo):
    """测试用：不依赖 Postgres / pgvector，向量检索在 Python 里算余弦。"""

    def __init__(self) -> None:  # noqa: D107
        self.episodes: list[dict] = []
        self.vectors: dict[str, list[float]] = {}
        self.profiles: dict[str, dict] = {}
        self.corrections: list[dict] = []

    def save_episode(self, card: EpisodeCard, embedding: list[float] | None, embed_model: str = "") -> str:
        import uuid

        existing = next((e for e in self.episodes
                         if e["service_id"] == card.service_id and e["incident_id"] == card.incident_id), None)
        episode_id = existing["id"] if existing else str(uuid.uuid4())
        record = {"id": episode_id, "service_id": card.service_id, "incident_id": card.incident_id,
                  "started_at": card.started_at.isoformat() if card.started_at else None,
                  "severity": card.severity, "card": card.render(),
                  "conclusion": card.conclusion.model_dump(mode="json")}
        if existing:
            existing.update(record)
        else:
            self.episodes.append(record)
        if embedding:
            self.vectors[episode_id] = embedding
        return episode_id

    def get_episode(self, episode_id: str) -> dict | None:
        return next((e for e in self.episodes if e["id"] == episode_id), None)

    def list_recent(self, service_id: str, days: int, limit: int) -> list[dict]:
        rows = [e for e in self.episodes if e["service_id"] == service_id]
        return sorted(rows, key=lambda e: e["started_at"] or "", reverse=True)[:limit]

    def search(self, service_id: str, embedding: list[float], k: int) -> list[tuple[dict, float]]:
        def cosine(a: list[float], b: list[float]) -> float:
            import math

            dot = sum(x * y for x, y in zip(a, b))
            na = math.sqrt(sum(x * x for x in a))
            nb = math.sqrt(sum(y * y for y in b))
            return dot / (na * nb) if na and nb else 0.0

        scored = []
        for episode in self.episodes:
            vector = self.vectors.get(episode["id"])
            if episode["service_id"] == service_id and vector:
                scored.append((episode, cosine(embedding, vector)))
        scored.sort(key=lambda item: item[1], reverse=True)
        return scored[:k]

    def get_profile(self, service_id: str) -> dict | None:
        return self.profiles.get(service_id)

    def upsert_profile(self, profile: ServiceProfile) -> None:
        self.profiles[profile.service_id] = profile.model_dump(mode="json")

    def count_episodes(self, service_id: str) -> int:
        return sum(1 for e in self.episodes if e["service_id"] == service_id)

    def record_correction(self, episode_id: str, field: str, from_value, to_value, reason: str = "") -> None:
        self.corrections.append({"episode_id": episode_id, "field": field,
                                 "from": from_value, "to": to_value, "reason": reason})
