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

from .schemas.memory import (
    EpisodeCard,
    EpisodeOutcome,
    Conclusion,
    KnownCause,
    KnownPattern,
    ServiceBaseline,
    ServiceProfile,
    SignatureItem,
    Symptom,
    EvidenceSignature,
    TimelineAttachment,
    TimelineNode,
)

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


def episode_to_node(episode: dict) -> TimelineNode:
    """旧 EpisodeCard -> TimelineNode 的**兼容投影**（不迁移数据、不删旧表）。

    旧事件簿仍是已落库事实，只有读取时按时间线视图投影一次；
    node_id 用 ``episode:<id>``，便于与附件/新节点区分。
    """
    episode_id = str(episode.get("id") or "")
    started = episode.get("started_at")
    returned = TimelineNode(
        node_id=f"episode:{episode_id}",
        service_id=str(episode.get("service_id") or ""),
        incident_id=episode.get("incident_id"),
        node_type="agent_diagnosis",
        source_system="agent_memory",
        source_record_id=episode_id,
        occurred_at=started,
        recorded_at=started,
        time_accuracy="derived",
        summary=str(episode.get("card") or "")[:500],
        source_locator={"legacy": "episodes"},
    )
    return returned


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

    # ---- 时间轴附件（C3 / 规格 §7.7.7） ----
    def save_attachment(self, attachment: TimelineAttachment) -> bool:
        """按 UNIQUE(anchor_type,anchor_id,attachment_type,source_system,source_record_id) 幂等写入。"""
        payload = attachment.model_dump(mode="json")
        row = self.conn.execute(
            """INSERT INTO timeline_attachments (attachment_id, service_id, anchor_type, anchor_id,
                   attachment_type, source_system, source_record_id, run_id, incident_id,
                   occurred_at, recorded_at, summary, trust_level, source_refs, content_digest, metadata)
               VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s::jsonb,%s,%s::jsonb)
               ON CONFLICT (anchor_type, anchor_id, attachment_type, source_system, source_record_id)
               DO NOTHING
               RETURNING attachment_id""",
            (
                attachment.attachment_id, attachment.service_id, attachment.anchor_type,
                attachment.anchor_id, attachment.attachment_type, attachment.source_system,
                attachment.source_record_id, attachment.run_id, attachment.incident_id,
                attachment.occurred_at, attachment.recorded_at, attachment.summary[:1000],
                attachment.trust_level,
                json.dumps(payload["source_refs"], ensure_ascii=False),
                attachment.content_digest,
                json.dumps(payload["metadata"], ensure_ascii=False),
            ),
        ).fetchone()
        return row is not None

    def list_attachments(self, service_id: str, *, limit: int = 20,
                         since: datetime | None = None,
                         until: datetime | None = None) -> list[dict]:
        """按服务 + 时间倒序读附件（不复制原文，只返回结构）。

        ``since``/``until`` 会被真实下推到 SQL：时间窗过滤必须作用在返回结果上，
        不能读出来再交给调用方自理。
        """
        rows = self.conn.execute(
            """SELECT attachment_id, service_id, anchor_type, anchor_id, attachment_type,
                      source_system, source_record_id, run_id, incident_id, occurred_at,
                      recorded_at, summary, trust_level, source_refs, content_digest, metadata
               FROM timeline_attachments
               WHERE service_id=%s
                 AND (%s::timestamptz IS NULL OR COALESCE(occurred_at, recorded_at) >= %s)
                 AND (%s::timestamptz IS NULL OR COALESCE(occurred_at, recorded_at) <= %s)
               ORDER BY COALESCE(occurred_at, recorded_at) DESC
               LIMIT %s""",
            (service_id, since, since, until, until, limit),
        ).fetchall()
        return [
            {
                "attachment_id": r[0], "service_id": r[1], "anchor_type": r[2], "anchor_id": r[3],
                "attachment_type": r[4], "source_system": r[5], "source_record_id": r[6],
                "run_id": r[7], "incident_id": r[8], "occurred_at": r[9], "recorded_at": r[10],
                "summary": r[11], "trust_level": r[12], "source_refs": r[13],
                "content_digest": r[14], "metadata": r[15],
            }
            for r in rows
        ]

    def list_diagnoses(self, service_id: str, start: datetime, end: datetime,
                       limit: int = 20) -> list[dict]:
        """窗口内的历史诊断（旧 Episode 投影），作为时间线里的历史节点。"""
        rows = self.conn.execute(
            """SELECT id, service_id, incident_id, started_at, resolved_at, card, created_at,
                      target_endpoint, profile_key
               FROM episodes
               WHERE service_id=%s AND started_at >= %s AND started_at <= %s
               ORDER BY started_at DESC
               LIMIT %s""",
            (service_id, start, end, limit),
        ).fetchall()
        return [episode_to_node({
            "id": str(r[0]), "service_id": r[1], "incident_id": r[2],
            "started_at": r[3].isoformat() if hasattr(r[3], "isoformat") else r[3],
            "card": r[5] or "",
        }).model_dump(mode="json") for r in rows]

    def list_corrections(self, service_id: str, start: datetime, end: datetime,
                         limit: int = 20) -> list[dict]:
        """人工纠正：挂在 episode 上，按创建时间过滤，供 include_corrected_history 使用。"""
        rows = self.conn.execute(
            """SELECT c.id, c.episode_id, c.field, c.from_value, c.to_value, c.reason, c.created_at
               FROM episode_corrections c
               JOIN episodes e ON e.id = c.episode_id
               WHERE e.service_id=%s AND e.started_at >= %s AND e.started_at <= %s
               ORDER BY c.created_at DESC
               LIMIT %s""",
            (service_id, start, end, limit),
        ).fetchall()
        return [
            {"id": r[0], "episode_id": str(r[1]), "field": r[2], "from_value": r[3],
             "to_value": r[4], "reason": r[5], "created_at": r[6]}
            for r in rows
        ]

    def timeline_nodes(self, service_id: str, days: int, limit: int = 50) -> list[TimelineNode]:
        """时间线视角：旧 Episode 投影 + 挂在节点上的附件（新旧并存）。"""
        attachments = self.list_attachments(service_id, limit=limit)
        nodes = [episode_to_node(item) for item in self.list_recent(service_id, days, limit)]
        by_incident: dict[str, list] = {}
        for item in attachments:
            key = str(item.get("incident_id") or item.get("anchor_id") or "")
            by_incident.setdefault(key, []).append(item)
        for node in nodes:
            node.attachments = [
                TimelineAttachment(**item) for item in by_incident.get(str(node.incident_id), [])
            ]
        return nodes


class InMemoryMemoryRepo(MemoryRepo):
    """测试用：不依赖 Postgres / pgvector，向量检索在 Python 里算余弦。"""

    def __init__(self) -> None:  # noqa: D107
        self.episodes: list[dict] = []
        self.vectors: dict[str, list[float]] = {}
        self.profiles: dict[str, dict] = {}
        self.corrections: list[dict] = []
        self.attachments: list[dict] = []

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
                                 "from_value": from_value, "to_value": to_value, "reason": reason,
                                 "created_at": datetime.now(timezone.utc).isoformat()})

    # ---- 时间轴附件（C3）：内存实现与 Postgres 同语义 ----
    def save_attachment(self, attachment: TimelineAttachment) -> bool:
        payload = attachment.model_dump(mode="json")
        key = attachment.unique_key()
        if any((item["anchor_type"], item["anchor_id"], item["attachment_type"],
                item["source_system"], item["source_record_id"]) == key for item in self.attachments):
            return False
        self.attachments.append({
            "attachment_id": attachment.attachment_id, "service_id": attachment.service_id,
            "anchor_type": attachment.anchor_type, "anchor_id": attachment.anchor_id,
            "attachment_type": attachment.attachment_type, "source_system": attachment.source_system,
            "source_record_id": attachment.source_record_id, "run_id": attachment.run_id,
            "incident_id": attachment.incident_id,
            "occurred_at": attachment.occurred_at.isoformat() if attachment.occurred_at else None,
            "recorded_at": attachment.recorded_at.isoformat(),
            "summary": attachment.summary[:1000], "trust_level": attachment.trust_level,
            "source_refs": payload["source_refs"], "content_digest": attachment.content_digest,
            "metadata": payload["metadata"],
        })
        return True

    def list_attachments(self, service_id: str, *, limit: int = 20,
                         since: datetime | None = None,
                         until: datetime | None = None) -> list[dict]:
        rows = [item for item in self.attachments if item["service_id"] == service_id]

        def stamp_of(item: dict) -> str:
            return item.get("occurred_at") or item.get("recorded_at") or ""

        if since is not None:
            stamp = since.isoformat()
            rows = [item for item in rows if stamp_of(item) >= stamp]
        if until is not None:
            stamp = until.isoformat()
            rows = [item for item in rows if stamp_of(item) <= stamp]
        rows.sort(key=stamp_of, reverse=True)
        return rows[:limit]

    def list_diagnoses(self, service_id: str, start: datetime, end: datetime,
                       limit: int = 20) -> list[dict]:
        nodes = []
        for episode in self.episodes:
            if episode.get("service_id") != service_id:
                continue
            started = episode.get("started_at")
            if not started:
                continue
            try:
                stamp = datetime.fromisoformat(str(started))
            except ValueError:
                continue
            if stamp.tzinfo is None:
                stamp = stamp.replace(tzinfo=timezone.utc)
            if not (start <= stamp <= end):
                continue
            nodes.append(episode_to_node(episode).model_dump(mode="json"))
        return nodes[:limit]

    def list_corrections(self, service_id: str, start: datetime, end: datetime,
                         limit: int = 20) -> list[dict]:
        rows = []
        for row in self.corrections:
            episode_id = str(row.get("episode_id") or "")
            owner = next((e for e in self.episodes if str(e.get("id")) == episode_id), None)
            if owner is None or owner.get("service_id") != service_id:
                continue
            started = owner.get("started_at")
            if started:
                try:
                    stamp = datetime.fromisoformat(str(started))
                except ValueError:
                    stamp = None
                if stamp is not None:
                    if stamp.tzinfo is None:
                        stamp = stamp.replace(tzinfo=timezone.utc)
                    if not (start <= stamp <= end):
                        continue
            rows.append({"id": row.get("id") or episode_id, **row})
        return rows[:limit]

    def timeline_nodes(self, service_id: str, days: int, limit: int = 50) -> list[TimelineNode]:
        attachments = self.list_attachments(service_id, limit=limit)
        nodes = [episode_to_node(item) for item in self.list_recent(service_id, days, limit)]
        for node in nodes:
            node.attachments = [
                TimelineAttachment(**item) for item in attachments
                if str(item.get("incident_id") or item.get("anchor_id")) == str(node.incident_id)
            ]
        return nodes
