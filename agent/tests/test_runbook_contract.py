"""RunbookQueryV1 / ArtifactV1 契约测试。

覆盖：请求构造、RAGFlow completion 字段映射、双 ID 引用、错误/超时/
不合规响应、Lead 消费（evidence + fact catalog），以及 A2A 鉴权配置。
"""

from __future__ import annotations

import json
import socket
import threading
import time
from pathlib import Path

import httpx
import pytest
import uvicorn

from alertflow_agent.a2a_adapter.ragflow import completion_body, parse_completion, query_from_runbook_request
from alertflow_agent.a2a_adapter.server import build_app
from alertflow_agent.a2a_adapter.store import MemoryLedger
from alertflow_agent.config import Settings
from alertflow_agent.integrations.runbook import consult
from alertflow_agent.integrations.runbook_contract import (
    ArtifactV1,
    InvalidArtifact,
    RunbookQueryV1,
    artifact_body,
    build_query,
    parse_artifact,
    query_payload,
)
from alertflow_agent.runtime.tools import run_tool

FIXTURES = Path(__file__).resolve().parent / "fixtures" / "ragflow"


def _artifact(**overrides):
    body = {
        "schema_version": "1",
        "status": "completed",
        "summary": "重启进程并检查连接池",
        "recommendations": [
            {"id": "r1", "text": "restart the process", "risk": "low", "preconditions": ["确认无进行中任务"]}
        ],
        "citations": [{"document_id": "doc-1", "chunk_id": "chunk-1", "title": "runbook", "excerpt": "restart"}],
        "warnings": [],
        "citations_available": True,
    }
    body.update(overrides)
    return body


def _port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def _listen(app, port):
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning"))
    server.install_signal_handlers = lambda: None
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    deadline = time.time() + 5
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.2):
                return server
        except OSError:
            time.sleep(0.05)
    raise RuntimeError("a2a test server did not start")


def _start(monkeypatch, research, *, timeout_seconds=5, auth_token=""):
    import alertflow_agent.a2a_adapter.server as server

    monkeypatch.setattr(server, "research", research)
    port = _port()
    url = f"http://127.0.0.1:{port}"
    app = build_app(
        MemoryLedger(),
        {"ragflow_base_url": "http://ragflow.test", "ragflow_agent_id": "agent", "timeout_seconds": timeout_seconds},
        url,
        auth_token=auth_token,
    )
    http = _listen(app, port)
    return url, http


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"},
        "evidence": {},
        "run_id": "run-contract",
        "coverage": {"gaps": []},
        "fact_catalog": [{"fact_id": "f1", "evidence_id": "chk:1", "path": "status", "value": "down"}],
    }


def _settings(url, **kwargs):
    return Settings(a2a_adapter_url=url, **kwargs)


# --- 协议定义 ---


def test_query_is_versioned_and_omits_nulls():
    query = build_query(
        request_id="runbook:run-1",
        run_id="run-1",
        service_id="api",
        question="restart?",
        window_start="2026-10-11T09:00:00+00:00",
        window_end="2026-10-11T10:00:00+00:00",
    )
    body = query_payload(query)
    assert body["schema_version"] == "1"
    assert body["request_id"] == "runbook:run-1"
    assert "incident_id" not in body
    assert RunbookQueryV1.model_validate(body).window_end.endswith("+00:00")


def test_legacy_artifact_is_parsed_as_v1_alias():
    artifact = parse_artifact(
        {
            "schema": "runbook-research.v1",
            "content": "restart the process",
            "citations": [{"document_id": "doc", "id": "chunk"}],
            "citations_available": True,
        }
    )
    assert artifact.schema_version == "1"
    assert artifact.summary == "restart the process"
    assert [c.chunk_id for c in artifact.citations] == ["chunk"]
    assert artifact.citations_available is True


def test_citation_without_both_ids_is_not_a_citation():
    artifact = parse_artifact(_artifact(citations=[{"document_id": "doc"}]))
    assert artifact.citations == []
    assert artifact.citations_available is False
    assert any("chunk_id" in warning for warning in artifact.warnings)


def test_failed_artifact_status_is_not_a_success():
    failed = parse_artifact(_artifact(status="failed"))
    assert failed.status == "failed"
    errored = parse_artifact({"schema_version": "1", "error": "runbook lookup failed"})
    assert errored.status == "failed"


def test_non_compliant_artifact_is_rejected():
    with pytest.raises(InvalidArtifact):
        parse_artifact({"schema_version": "2", "summary": "x"})
    with pytest.raises(InvalidArtifact):
        parse_artifact("not json")
    with pytest.raises(InvalidArtifact):
        parse_artifact({"schema_version": "1"})


def test_artifact_body_keeps_legacy_aliases():
    body = artifact_body(parse_artifact(_artifact()))
    assert body["schema"] == "runbook-research.v1"
    assert body["schema_version"] == "1"
    assert body["content"] == body["summary"]
    assert [c["chunk_id"] for c in body["citations"]] == ["chunk-1"]


# --- RAGFlow v0.27.3 字段映射 ---


def test_completion_body_matches_v027_fixture():
    fixture = json.loads((FIXTURES / "completion-request.json").read_text())
    assert completion_body("AGENT_ID", fixture["query"]) == fixture
    assert set(completion_body("a", "q")) == {"agent_id", "query", "stream"}


def test_completion_reference_is_mapped_to_double_id_citations():
    payload = json.loads((FIXTURES / "completion-response.json").read_text())
    artifact = parse_completion(payload)
    citation = artifact["citations"][0]
    assert citation["document_id"] == "4bdd2ff65e1511f0907f09f583941b45"
    assert citation["chunk_id"] == "4b8935ac0a22deb1"
    assert citation["title"]
    assert artifact["citations_available"] is True
    uncited = json.loads((FIXTURES / "completion-response-uncited.json").read_text())
    assert parse_completion(uncited)["citations_available"] is False


def test_runbook_request_is_mapped_to_readable_ragflow_query():
    payload = query_payload(
        build_query(
            request_id="r",
            run_id="run-1",
            service_id="api",
            symptom_summary="5xx spike",
            question="how do I restart?",
            window_start="2026-10-11T09:00:00+00:00",
            window_end="2026-10-11T10:00:00+00:00",
        )
    )
    text = json.dumps(payload, ensure_ascii=False)
    query = query_from_runbook_request(text)
    assert "服务: api" in query
    assert "症状: 5xx spike" in query
    assert "问题: how do I restart?" in query
    assert "schema_version" not in query
    assert query_from_runbook_request("plain question") == "plain question"


# --- Mock A2A 端到端（真实 A2A 协议 + 假 RAGFlow） ---


def test_mock_a2a_receives_runbook_query_and_lead_consumes_artifact(monkeypatch):
    seen = []

    def research(base_url, api_key, agent_id, query, timeout):
        seen.append(query)
        return artifact_body(parse_artifact(_artifact()))

    url, http = _start(monkeypatch, research)
    try:
        state = _state()
        result = run_tool("consult_runbook_agent", {"question": "how do I restart?"}, state, _settings(url), {})
        assert result["status"] == "success", result
        assert result["data"] == {"citations_available": True, "citations": 1}
        # 适配器把 RunbookQueryV1 映射成可读 query 交给 RAGFlow
        assert seen and "问题: how do I restart?" in seen[0]
        evidence = state["evidence"]["runbook:runbook:run-contract"]
        assert evidence["data"]["citations_available"] is True
        citation = evidence["data"]["citations"][0]
        assert citation["document_id"] == "doc-1" and citation["chunk_id"] == "chunk-1"
        assert evidence["data"]["recommendations"][0]["text"] == "restart the process"
    finally:
        http.should_exit = True


def test_lead_request_contains_scope_window_and_verified_facts(monkeypatch):
    captured = {}

    def research(base_url, api_key, agent_id, query, timeout):
        captured["query"] = query
        return artifact_body(parse_artifact(_artifact()))

    url, http = _start(monkeypatch, research)
    try:
        import alertflow_agent.a2a_adapter.server as server

        original = server.query_from_runbook_request
        captured["json"] = None

        def spy(text, **kwargs):
            captured["json"] = text
            return original(text, **kwargs)

        server.query_from_runbook_request = spy
        try:
            run_tool("consult_runbook_agent", {"question": "restart?"}, _state(), _settings(url), {})
        finally:
            server.query_from_runbook_request = original
        query = RunbookQueryV1.model_validate_json(captured["json"])
        assert query.run_id == "run-contract"
        assert query.service_id == "api"
        assert query.question == "restart?"
        assert query.window_start.endswith("+00:00") and query.window_end.endswith("+00:00")
        assert query.known_facts and query.known_facts[0].evidence_id == "chk:1"
    finally:
        http.should_exit = True


def test_invalid_citation_response_keeps_advice_but_marks_uncited(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        return artifact_body(parse_artifact(_artifact(citations=[{"document_id": "doc-only"}])))

    url, http = _start(monkeypatch, research)
    try:
        state = _state()
        result = run_tool("consult_runbook_agent", {"question": "restart?"}, state, _settings(url), {})
        assert result["status"] == "success"
        data = state["evidence"]["runbook:runbook:run-contract"]["data"]
        assert data["citations_available"] is False
        assert data["citations"] == []
        assert data["summary"] == "重启进程并检查连接池"
    finally:
        http.should_exit = True


def test_failed_artifact_never_becomes_evidence(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        return artifact_body(parse_artifact(_artifact(status="failed")))

    url, http = _start(monkeypatch, research)
    try:
        state = _state()
        result = run_tool("consult_runbook_agent", {"question": "restart?"}, state, _settings(url), {})
        assert result["status"] == "error"
        assert "runbook lookup failed" in state["coverage"]["gaps"]
        assert "runbook:runbook:run-contract" not in state["evidence"]
    finally:
        http.should_exit = True


def test_a2a_timeout_is_a_gap_not_a_failure(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        time.sleep(2)
        return artifact_body(parse_artifact(_artifact()))

    url, http = _start(monkeypatch, research, timeout_seconds=30)
    try:
        settings = _settings(url, a2a_adapter_timeout_seconds=0.3)
        state = _state()
        result = run_tool("consult_runbook_agent", {"question": "restart?"}, state, settings, {})
        assert result["status"] == "error"
        assert state["coverage"]["gaps"] == ["runbook agent did not answer"]
    finally:
        http.should_exit = True


# --- A2A Endpoint / 鉴权配置化 ---


def test_a2a_bearer_token_is_configurable(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        return artifact_body(parse_artifact(_artifact()))

    url, http = _start(monkeypatch, research, auth_token="secret")
    try:
        card = httpx.get(f"{url}/.well-known/agent-card.json", timeout=5)
        assert card.status_code == 200
        with pytest.raises(Exception):
            consult(url, {"schema_version": "1", "service_id": "api", "question": "q"}, "m-no-token", timeout=2)
        first = consult(
            url,
            {"schema_version": "1", "service_id": "api", "question": "q"},
            "m-token",
            timeout=2,
            token="secret",
        )
        assert first["schema_version"] == "1"
        state = _state()
        result = run_tool(
            "consult_runbook_agent",
            {"question": "restart?"},
            state,
            _settings(url, a2a_adapter_token="secret"),
            {},
        )
        assert result["status"] == "success"
    finally:
        http.should_exit = True


# --- 7.2 Runbook evidence → Fact Catalog ---


def test_runbook_artifact_reaches_the_existing_fact_catalog(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        return artifact_body(parse_artifact(_artifact()))

    url, http = _start(monkeypatch, research)
    try:
        state = _state()
        run_tool("consult_runbook_agent", {"question": "restart?"}, state, _settings(url), {})
        from alertflow_agent.runtime.verify import build_catalog

        catalog = build_catalog(state["evidence"])
        runbook = [entry for entry in catalog if entry["evidence_id"] == "runbook:runbook:run-contract"]
        paths = {entry["path"] for entry in runbook}
        assert {"schema", "content", "citations_available"} <= paths
        content = next(entry for entry in runbook if entry["path"] == "content")
        assert "重启进程" in content["value"]
        assert any(entry["path"].startswith("citations") for entry in runbook)
    finally:
        http.should_exit = True


def test_malformed_artifact_does_not_break_the_investigation(monkeypatch):
    def research(base_url, api_key, agent_id, query, timeout):
        return {"schema_version": "2", "summary": "future protocol"}

    url, http = _start(monkeypatch, research)
    try:
        state = _state()
        result = run_tool("consult_runbook_agent", {"question": "restart?"}, state, _settings(url), {})
        assert result["status"] == "error"
        assert state["coverage"]["gaps"] == ["runbook artifact is invalid"]
        assert state["evidence"] == {}
    finally:
        http.should_exit = True


# --- 7.3 RAGFlow 三配置缺一即配置错误 ---


@pytest.mark.parametrize(
    "base_url,api_key,agent_id",
    [
        ("", "token", "agent"),
        ("http://ragflow.test", "", "agent"),
        ("http://ragflow.test", "token", ""),
    ],
)
def test_ragflow_missing_config_fails_before_any_request(monkeypatch, base_url, api_key, agent_id):
    import alertflow_agent.a2a_adapter.ragflow as ragflow

    def boom(*args, **kwargs):
        raise AssertionError("ragflow was called")

    monkeypatch.setattr(ragflow.httpx, "post", boom)
    with pytest.raises(RuntimeError, match="not configured"):
        ragflow.research(base_url, api_key, agent_id, "restart", 1)
