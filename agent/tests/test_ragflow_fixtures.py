"""v0.27.3 completion 样例。字段来自该 tag 的 HTTP 文档和 agent_api.py，不是一次真实调用。"""

import json
from pathlib import Path

FIXTURES = Path(__file__).resolve().parent / "fixtures" / "ragflow"


def _load(name):
    return json.loads((FIXTURES / name).read_text())


def citations(payload):
    data = ((payload.get("data") or {}).get("data") or {})
    reference = data.get("reference") or {}
    chunks = reference.get("chunks") if isinstance(reference, dict) else None
    if isinstance(chunks, dict):
        chunks = list(chunks.values())
    found = []
    for chunk in chunks or []:
        if not isinstance(chunk, dict):
            continue
        document_id = chunk.get("document_id") or chunk.get("doc_id")
        chunk_id = chunk.get("id") or chunk.get("chunk_id")
        if document_id and chunk_id:
            found.append({"document_id": document_id, "chunk_id": chunk_id})
    return found


def test_completion_fixture_matches_v027_standard_request():
    body = _load("completion-request.json")
    assert body == {
        "agent_id": "AGENT_ID",
        "query": "What does the runbook say about this outage?",
        "stream": False,
    }
    assert "messages" not in body


def test_document_ids_are_citations_and_answer_text_is_not():
    cited = citations(_load("completion-response.json"))
    assert cited == [{"document_id": "doc-1", "chunk_id": "chunk-1"}]
    uncited = _load("completion-response-uncited.json")
    assert citations(uncited) == []
    assert "doc-1" in uncited["data"]["data"]["content"]
