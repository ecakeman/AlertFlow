"""Runbook A2A 协议契约（规格 §7.7.4D）。

A2A 的 Part.text 只承载 JSON：
- 请求：RunbookQueryV1（父级构造，含统一时间窗与已验证事实摘要）
- 响应：RunbookArtifactV1（专家建议 + 双 ID 引用，status 以 A2A Task 终态为准）

这里刻意只定义一份协议，Mock 服务与真实 RAGFlow 适配器共用；
旧版 `runbook-research.v1`（schema/content 字段）只作为兼容别名解析，
不作为第二套协议。所有解析失败都返回结构化异常，由工具层转成 coverage gap。
"""

from __future__ import annotations

import json
from typing import Any, Literal

from pydantic import BaseModel, Field, ValidationError

SCHEMA_VERSION = "1"
ARTIFACT_SCHEMA = "runbook-artifact.v1"
LEGACY_SCHEMA = "runbook-research.v1"


class RunbookFactHint(BaseModel):
    """发往专家侧的事实摘要：只带已验证事实的引用与短摘要，不带原始日志。"""

    model_config = {"extra": "forbid"}

    fact_id: str = ""
    evidence_id: str = ""
    summary: str
    value: str = ""


class RunbookQueryV1(BaseModel):
    model_config = {"extra": "forbid"}

    schema_version: Literal["1"] = SCHEMA_VERSION
    request_id: str
    run_id: str
    service_id: str
    incident_id: str | None = None
    service_name: str | None = None
    symptom_summary: str = ""
    question: str
    known_facts: list[RunbookFactHint] = Field(default_factory=list)
    window_start: str
    window_end: str
    max_citations: int = 8
    max_output_chars: int = 8000
    deadline_at: str | None = None


class CitationV1(BaseModel):
    """双 ID 引用：document_id 定位文档，chunk_id 定位片段，二者缺一不可追溯。"""

    model_config = {"extra": "ignore"}

    document_id: str
    chunk_id: str
    title: str = ""
    excerpt: str = ""


class RecommendationV1(BaseModel):
    model_config = {"extra": "ignore"}

    id: str
    text: str
    risk: str = "unknown"
    preconditions: list[str] = Field(default_factory=list)


class ArtifactV1(BaseModel):
    model_config = {"extra": "ignore"}

    schema_version: Literal["1"] = SCHEMA_VERSION
    status: Literal["completed", "failed", "canceled"] = "completed"
    summary: str = ""
    recommendations: list[RecommendationV1] = Field(default_factory=list)
    citations: list[CitationV1] = Field(default_factory=list)
    warnings: list[str] = Field(default_factory=list)
    citations_available: bool = False


class InvalidArtifact(ValueError):
    """artifact 不是合法 JSON 或缺少必要结构。"""


def build_query(
    *,
    request_id: str,
    run_id: str,
    service_id: str,
    question: str,
    window_start: str,
    window_end: str,
    incident_id: str | None = None,
    service_name: str | None = None,
    symptom_summary: str = "",
    known_facts: list[RunbookFactHint] | None = None,
    max_citations: int = 8,
    max_output_chars: int = 8000,
    deadline_at: str | None = None,
) -> RunbookQueryV1:
    return RunbookQueryV1(
        request_id=request_id,
        run_id=run_id,
        service_id=service_id,
        incident_id=incident_id,
        service_name=service_name,
        symptom_summary=symptom_summary,
        question=question,
        known_facts=list(known_facts or []),
        window_start=window_start,
        window_end=window_end,
        max_citations=max_citations,
        max_output_chars=max_output_chars,
        deadline_at=deadline_at,
    )


def query_payload(query: RunbookQueryV1) -> dict:
    """A2A Part.text 的 JSON 内容（不含 None，避免各实现自行解释空值）。"""
    return query.model_dump(mode="json", exclude_none=True)


def parse_artifact(payload: dict | str) -> ArtifactV1:
    """把 A2A artifact / RAGFlow completion 归一化为 ArtifactV1。

    兼容规则（只此一份协议，旧字段是同义别名）：
    - schema_version 缺失但 schema == "runbook-research.v1" → 视为 v1；
    - content 视为 summary；
    - citation 的 doc_id/chunk_id/id 归一化为 document_id/chunk_id；
    - 缺 document_id 或 chunk_id 的 citation 丢弃并告警，citations_available=false；
    - 带 error 的 artifact 视为 failed，即使也带了建议文本。
    """
    data = _load(payload)
    warnings = [str(w) for w in data.get("warnings") or []]
    status = str(data.get("status") or "completed").lower()
    if status not in ("completed", "failed", "canceled"):
        warnings.append(f"unknown artifact status {status!r}, treated as failed")
        status = "failed"
    if data.get("error"):
        status = "failed"

    summary = data.get("summary")
    if not isinstance(summary, str) or not summary:
        summary = data.get("content") if isinstance(data.get("content"), str) else ""
    citations: list[CitationV1] = []
    for raw in data.get("citations") or []:
        if not isinstance(raw, dict):
            warnings.append("citation is not an object and was dropped")
            continue
        document_id = raw.get("document_id") or raw.get("doc_id")
        chunk_id = raw.get("chunk_id") or raw.get("id")
        if not document_id or not chunk_id:
            warnings.append("citation missing document_id or chunk_id and was dropped")
            continue
        citations.append(
            CitationV1(
                document_id=str(document_id),
                chunk_id=str(chunk_id),
                title=str(raw.get("title") or ""),
                excerpt=str(raw.get("excerpt") or ""),
            )
        )
    declared = data.get("citations_available")
    available = bool(citations) and (declared is not False)
    if declared and not citations:
        warnings.append("artifact claimed citations but none were traceable")

    recommendations: list[RecommendationV1] = []
    for index, raw in enumerate(data.get("recommendations") or []):
        if not isinstance(raw, dict):
            warnings.append("recommendation is not an object and was dropped")
            continue
        text = raw.get("text") or raw.get("description") or raw.get("summary")
        if not isinstance(text, str) or not text.strip():
            warnings.append("recommendation has no text and was dropped")
            continue
        recommendations.append(
            RecommendationV1(
                id=str(raw.get("id") or f"rec-{index + 1}"),
                text=text.strip(),
                risk=str(raw.get("risk") or "unknown"),
                preconditions=[str(p) for p in raw.get("preconditions") or [] if str(p).strip()],
            )
        )
    return ArtifactV1(
        schema_version=SCHEMA_VERSION,
        status=status,  # type: ignore[arg-type]
        summary=str(summary)[:8000],
        recommendations=recommendations,
        citations=citations,
        warnings=warnings,
        citations_available=available,
    )


def artifact_body(artifact: ArtifactV1) -> dict:
    """artifact 的统一 JSON 表示，同时保留旧版 schema/content 别名字段。"""
    return {
        "schema": LEGACY_SCHEMA,
        "schema_version": SCHEMA_VERSION,
        "status": artifact.status,
        "summary": artifact.summary,
        "content": artifact.summary,
        "recommendations": [r.model_dump(mode="json") for r in artifact.recommendations],
        "citations": [c.model_dump(mode="json") for c in artifact.citations],
        "warnings": list(artifact.warnings),
        "citations_available": artifact.citations_available,
    }


def _load(payload: dict | str) -> dict:
    data: Any = payload
    if isinstance(payload, str):
        text = payload.strip()
        if text.startswith("```"):
            # 有些实现会把 JSON 包在 markdown 代码块里
            text = text.strip("`")
            text = text[text.find("{") :] if "{" in text else text
        try:
            data = json.loads(text)
        except json.JSONDecodeError as exc:
            raise InvalidArtifact(f"artifact is not JSON: {exc}") from exc
    if not isinstance(data, dict):
        raise InvalidArtifact("artifact must be a JSON object")
    if data.get("schema_version") not in (None, SCHEMA_VERSION):
        raise InvalidArtifact(f"unsupported artifact schema_version {data.get('schema_version')!r}")
    schema = data.get("schema")
    if schema not in (None, LEGACY_SCHEMA, ARTIFACT_SCHEMA):
        raise InvalidArtifact(f"unsupported artifact schema {schema!r}")
    if "summary" not in data and "content" not in data and not data.get("error"):
        # 允许只有建议的 artifact，但完全空响应按不合规处理
        if not data.get("recommendations") and not data.get("citations"):
            raise InvalidArtifact("artifact has no summary, content or recommendations")
    return data


__all__ = [
    "ARTIFACT_SCHEMA",
    "ArtifactV1",
    "CitationV1",
    "InvalidArtifact",
    "LEGACY_SCHEMA",
    "RecommendationV1",
    "RunbookFactHint",
    "RunbookQueryV1",
    "SCHEMA_VERSION",
    "artifact_body",
    "build_query",
    "parse_artifact",
    "query_payload",
]
