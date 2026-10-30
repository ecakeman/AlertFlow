"""Translate one runbook question into a RAGFlow v0.27.3 completion."""

from __future__ import annotations

import json

import httpx

OUTPUT_CAP = 8000


def completion_body(agent_id: str, query: str) -> dict:
    return {"agent_id": agent_id, "query": query, "stream": False}


def parse_completion(payload: dict) -> dict:
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
            citations.append({"document_id": str(document_id), "id": str(chunk_id)})
    return {
        "schema": "runbook-research.v1",
        "content": content,
        "citations": citations,
        "citations_available": bool(citations),
    }


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


def artifact_text(artifact: dict) -> str:
    return json.dumps(artifact, ensure_ascii=False)[: OUTPUT_CAP + 512]
