"""Translate one runbook question into a RAGFlow v0.27.3 completion."""

from __future__ import annotations

import json

import httpx

from alertflow_agent.integrations.runbook_contract import (
    LEGACY_SCHEMA,
    artifact_body,
    parse_artifact,
)

OUTPUT_CAP = 8000


def completion_body(agent_id: str, query: str) -> dict:
    return {"agent_id": agent_id, "query": query, "stream": False}


def parse_completion(payload: dict) -> dict:
    """RAGFlow v0.27.3 completion → ArtifactV1（Document/Chunk 双 ID 引用）。

    RAGFlow 只返回 answer 文本与 reference.chunks，不产出 recommendations，
    因此摘要即 content；引用归一化交给 runbook_contract.parse_artifact。
    """
    data = ((payload or {}).get("data") or {}).get("data") or {}
    content = str(data.get("content") or "")[:OUTPUT_CAP]
    raw = (data.get("reference") or {}).get("chunks") or []
    if isinstance(raw, dict):
        raw = list(raw.values())
    citations = []
    for chunk in raw:
        if not isinstance(chunk, dict):
            continue
        document_id = chunk.get("document_id") or chunk.get("doc_id")
        chunk_id = chunk.get("id") or chunk.get("chunk_id")
        if document_id and chunk_id:
            citations.append(
                {
                    "document_id": str(document_id),
                    "chunk_id": str(chunk_id),
                    "title": str(chunk.get("document_name") or chunk.get("doc_name") or ""),
                    "excerpt": str(chunk.get("content") or "")[:500],
                }
            )
    artifact = parse_artifact(
        {
            "schema": LEGACY_SCHEMA,
            "summary": content,
            "content": content,
            "citations": citations,
            "citations_available": bool(citations),
        }
    )
    return artifact_body(artifact)


def research(base_url: str, api_key: str, agent_id: str, query: str, timeout: float) -> dict:
    url = str(base_url or "").strip().rstrip("/")
    token = str(api_key or "").strip()
    if not url or not token or not str(agent_id or "").strip():
        raise RuntimeError("ragflow is not configured")
    headers = {"Content-Type": "application/json", "Authorization": f"Bearer {token}"}
    response = httpx.post(
        f"{url}/api/v1/agents/chat/completions",
        headers=headers,
        json=completion_body(agent_id, query),
        timeout=timeout,
    )
    response.raise_for_status()
    body = response.json()
    if body.get("code") not in (0, None):
        raise RuntimeError("ragflow completion failed")
    return parse_completion(body)


def query_from_runbook_request(text: str, *, max_chars: int = OUTPUT_CAP) -> str:
    """把 A2A 的 RunbookQueryV1 转成 RAGFlow completion 的 query。

    v0.27.3 的 completion 只有 query 文本字段，没有结构化 messages；
    因此把契约字段映射成一段可读 prompt，而不是把 JSON 原样塞给 RAGFlow。
    非 JSON 文本（旧客户端/测试）原样透传。
    """
    raw = str(text or "").strip()
    if not raw.startswith("{"):
        return raw[:max_chars]
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return raw[:max_chars]
    if not isinstance(data, dict) or not (data.get("question") or data.get("service_id")):
        return raw[:max_chars]
    lines = ["你是运维 Runbook 专家。请基于服务知识给出可执行的处置建议，并标注引用。"]
    if data.get("service_id"):
        lines.append(f"服务: {data.get('service_id')}")
    if data.get("service_name"):
        lines.append(f"服务名: {data.get('service_name')}")
    if data.get("symptom_summary"):
        lines.append(f"症状: {data.get('symptom_summary')}")
    if data.get("window_start") or data.get("window_end"):
        lines.append(f"调查窗口: {data.get('window_start') or '?'} ~ {data.get('window_end') or '?'}")
    facts = data.get("known_facts") or []
    if facts:
        lines.append("已核实事实:")
        for fact in facts[:8]:
            if isinstance(fact, dict) and fact.get("summary"):
                lines.append(f"- {str(fact.get('summary'))[:200]}")
    if data.get("question"):
        lines.append(f"问题: {data.get('question')}")
    return "\n".join(lines)[:max_chars]


def artifact_text(artifact: dict) -> str:
    return json.dumps(artifact, ensure_ascii=False)[: OUTPUT_CAP + 512]
