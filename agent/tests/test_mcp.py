import json
from pathlib import Path

import httpx
import pytest

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.integrations.mcp import (
    CappedTransport,
    HTTPSource,
    MCPError,
    scope_logql,
    scope_promql,
    sources_from_settings,
    summarize_result,
)
from alertflow_agent.runtime.context import build_blocks
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.runtime.tools import run_tool
from alertflow_agent.runtime.verify import build_catalog, fact_id, verify
from tests.fakes import FakeAlertFlow, event_row

PROM = json.dumps({"api": 'service="api"'})
LOKI = json.dumps({"api": '{service="api"}'})


class Recorder:
    def __init__(self, text="1", error=None):
        self.calls = []
        self.text = text
        self.error = error

    def call(self, name, args):
        self.calls.append((name, args))
        if self.error:
            raise self.error
        if callable(self.text):
            return self.text(name, args)
        return self.text


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"},
        "evidence": {},
        "coverage": {"gaps": []},
    }


def _settings(**extra):
    base = dict(
        prometheus_mcp_url="http://prometheus.example/mcp",
        loki_mcp_url="http://loki.example/stream",
        prometheus_service_selectors=PROM,
        loki_service_selectors=LOKI,
        prometheus_mcp_token="prom-secret",
        loki_mcp_token="loki-secret",
    )
    base.update(extra)
    return Settings(**base)


def _gaps(state):
    return state["coverage"]["gaps"]


def test_blank_urls_do_not_construct_a_client(monkeypatch):
    def boom(self, *args, **kwargs):
        raise AssertionError("client constructed")

    monkeypatch.setattr(HTTPSource, "__init__", boom)
    assert sources_from_settings(Settings()) is None


def test_unconfigured_tool_is_unknown_and_does_not_call():
    client = Recorder()
    result = run_tool("query", {"query": "up"}, _state(), Settings(), {"mcp": client})
    assert result["status"] == "invalid_request"
    assert result["note"] == "unknown tool"
    assert client.calls == []
    result = run_tool("loki_query", {"query": '{service="api"}'}, _state(), Settings(), {"mcp": client})
    assert result["note"] == "unknown tool"
    assert client.calls == []


def test_unmapped_service_does_not_call_or_store():
    client = Recorder()
    state = _state()
    settings = _settings(prometheus_service_selectors="", loki_service_selectors="")
    result = run_tool("query", {"query": "up"}, state, settings, {"mcp": client})
    assert "PROMETHEUS_UNMAPPED" in _gaps(state)
    assert state["evidence"] == {}
    assert client.calls == []
    result = run_tool("loki_query", {"query": '{job="x"}'}, state, settings, {"mcp": client})
    assert "LOKI_UNMAPPED" in _gaps(state)
    assert client.calls == []
    assert "no evidence" in result["note"]


def test_unscoped_query_is_not_sent():
    client = Recorder()
    state = _state()
    result = run_tool("query", {"query": "up or down"}, state, _settings(), {"mcp": client})
    assert result["status"] == "error"
    assert "PROMETHEUS_UNSCOPED" in _gaps(state)
    assert client.calls == []
    assert scope_promql("up", 'service="api"') == 'up{service="api"}'
    assert scope_logql('{job="x"} |= "e"', '{service="api"}') == '{job="x",service="api"} |= "e"'


def test_configured_query_writes_scoped_evidence_once():
    client = Recorder(text='[{"value": "1"}]')
    state = _state()
    result = run_tool("query", {"query": "up"}, state, _settings(), {"mcp": client})
    assert result["status"] == "success"
    sent = client.calls[0][1]
    assert sent["query"] == 'up{service="api"}'
    assert sent["end"] - sent["start"] == 3600
    item = state["evidence"][result["evidence_ids"][0]]
    assert item["source_type"] == "prometheus"
    assert item["data"]["query"] == 'up{service="api"}'
    assert item["data"]["samples"] == 1
    assert item["data"]["value"] == "1"
    assert "prom-secret" not in json.dumps(item)
    assert run_tool("series", {}, state, _settings(), {"mcp": client})["note"] == "unknown tool"
    assert [name for name, _ in client.calls] == ["query"]


def test_window_row_and_request_caps():
    client = Recorder(text=json.dumps([{"value": "alpha"}, {"value": "beta"}, {"value": "gamma"}]))
    state = _state()
    settings = _settings(mcp_max_samples=2, mcp_max_window_seconds=60)
    result = run_tool(
        "range_query",
        {"query": "up", "start": 1, "end": 10**12},
        state,
        settings,
        {"mcp": client},
    )
    sent = client.calls[0][1]
    assert sent["end"] - sent["start"] == 60
    item = state["evidence"][result["evidence_ids"][0]]
    assert item["data"]["samples"] == 2
    assert item["data"]["value"] == "alpha"
    assert "gamma" not in json.dumps(item)
    assert item["limitations"] == ["truncated to the configured row cap"]

    huge = Recorder()
    blocked = _state()
    tiny = _settings(mcp_max_response_bytes=32)
    refused = run_tool("query", {"query": "up_" + ("x" * 80)}, blocked, tiny, {"mcp": huge})
    assert "PROMETHEUS_TOO_LARGE" in _gaps(blocked)
    assert huge.calls == []
    assert refused["status"] == "error"


def test_call_budget_stops_before_another_connection():
    client = Recorder(text="1")
    state = _state()
    settings = _settings(mcp_max_calls=1)
    assert run_tool("query", {"query": "up"}, state, settings, {"mcp": client})["status"] == "success"
    second = run_tool("range_query", {"query": "up"}, state, settings, {"mcp": client})
    assert second["status"] == "error"
    assert "PROMETHEUS_BUDGET" in _gaps(state)
    assert [name for name, _ in client.calls] == ["query"]


def test_each_failure_kind_is_a_gap_and_not_evidence():
    state = _state()
    settings = _settings(mcp_max_calls=10)
    kinds = {
        "timeout": "PROMETHEUS_TIMEOUT",
        "disconnect": "PROMETHEUS_DISCONNECT",
        "auth": "PROMETHEUS_AUTH",
        "rate_limit": "PROMETHEUS_RATE_LIMIT",
        "malformed": "PROMETHEUS_MALFORMED",
    }
    for kind, code in kinds.items():
        client = Recorder(error=MCPError(kind))
        result = run_tool("query", {"query": "up"}, state, settings, {"mcp": client})
        assert result["status"] == "error"
        assert "no evidence" in result["note"]
        assert code in _gaps(state)
        assert client.calls == [("query", client.calls[0][1])]
    empty = Recorder(text="[]")
    result = run_tool("loki_query", {"query": '{job="x"}'}, state, settings, {"mcp": empty})
    assert "LOKI_EMPTY" in _gaps(state)
    assert state["evidence"] == {}
    assert "loki-secret" not in result["note"]


def test_label_catalog_is_not_stored_as_service_evidence():
    client = Recorder(text="job,instance,service")
    state = _state()
    result = run_tool("label_names", {}, state, _settings(), {"mcp": client})
    assert result["status"] == "success"
    assert result["evidence_ids"] == []
    assert state["evidence"] == {}
    assert client.calls


def test_one_source_can_fail_while_the_other_is_stored():
    def answer(name, args):
        if name in {"query", "range_query", "label_values", "label_names", "metric_metadata"}:
            raise MCPError("timeout")
        return json.dumps([{"line": "checkout failed"}])

    client = Recorder(text=answer)
    state = _state()
    assert run_tool("query", {"query": "up"}, state, _settings(), {"mcp": client})["status"] == "error"
    ok = run_tool("loki_query", {"query": '{job="x"}'}, state, _settings(), {"mcp": client})
    assert ok["status"] == "success"
    assert "PROMETHEUS_TIMEOUT" in _gaps(state)
    stored = state["evidence"][ok["evidence_ids"][0]]
    assert stored["source_type"] == "loki"
    assert stored["data"]["lines"] == 1
    assert 'service="api"' in stored["data"]["query"]


def test_failed_prometheus_read_still_finishes_the_run():
    store = MemoryStore()
    row = event_row("mcp")
    row["input_event"]["script"] = ["query", "finish"]
    row["input_event"]["script_args"] = {"query": "up"}
    store.insert_run(row)
    client = Recorder(error=MCPError("timeout"))
    runner = Runner(_settings(), store, FakeAlertFlow(), ScriptedModel(), mcp=client)
    runner.execute(row["id"])
    saved = store.get(row["id"])
    report = store.latest_report(row["id"])["report"]
    assert saved["status"] == "completed"
    assert report["usage"]["tool_calls"] == 2
    assert "PROMETHEUS_TIMEOUT" in report["coverage"]["gaps"]
    assert any(item["text"] == "Prometheus has no evidence." for item in report["unknowns"])
    assert [name for name, _ in client.calls] == ["query"]
    assert "prometheus" not in {item["source_type"] for item in store.get_evidence(row["id"]).values()}


def test_investigation_without_mcp_still_finishes():
    store = MemoryStore()
    row = event_row("plain")
    row["input_event"]["script"] = ["query", "finish"]
    store.insert_run(row)
    client = Recorder()
    runner = Runner(Settings(), store, FakeAlertFlow(), ScriptedModel(), mcp=client)
    runner.execute(row["id"])
    assert store.get(row["id"])["status"] == "completed"
    assert client.calls == []
    assert sources_from_settings(Settings()) is None


def test_mcp_evidence_is_citable_and_unknown_ids_fail():
    client = Recorder(text='[{"value": "4"}]')
    state = _state()
    result = run_tool("query", {"query": "up"}, state, _settings(), {"mcp": client})
    evidence = state["evidence"]
    ident = result["evidence_ids"][0]
    catalog = build_catalog(evidence)
    query_fact = fact_id(catalog, ident, "query")
    assert query_fact
    draft = {
        "summary": "scoped metric",
        "observations": [{
            "id": "o1",
            "text": "the sent query",
            "evidence_ids": [ident],
            "fact_ids": [query_fact],
        }],
        "hypotheses": [{
            "id": "h1", "text": "probe", "layer": "probe", "status": "possible",
            "supporting_evidence_ids": [], "weakening_evidence_ids": [],
        }],
        "unknowns": [],
        "recommended_next_steps": [],
    }
    checked = verify(draft, evidence, ["prom-secret"])
    assert checked["passed"]
    assert checked["draft"]["observations"][0]["facts"][0]["path"] == "query"
    draft["observations"][0]["fact_ids"] = ["no-such"]
    draft["observations"][0]["evidence_ids"] = ["missing-ev"]
    rejected = verify(draft, evidence, [])
    rules = {item["rule"] for item in rejected["findings"]}
    assert "fact_ref" in rules
    assert "evidence_ref" in rules


def test_lead_context_lists_only_configured_tools():
    quiet = build_blocks("lead", {"event": {}})
    assert all(block.name != "mcp_tools" for block in quiet)
    named = build_blocks("lead", {"event": {}, "mcp_tools": ["loki_query"]})
    block = next(block for block in named if block.name == "mcp_tools")
    listed = block.variants[0].split("only these: ", 1)[1].split(".", 1)[0]
    assert listed == "loki_query"


def test_transport_stops_reading_past_the_byte_cap():
    class _Chunks(httpx.AsyncByteStream):
        def __init__(self):
            self.pulled = 0

        async def __aiter__(self):
            for _ in range(50):
                self.pulled += 100
                yield b"a" * 100

    chunks = _Chunks()

    class _Inner(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            return httpx.Response(200, stream=chunks)

    async def scenario():
        transport = CappedTransport(_Inner(), 100)
        async with httpx.AsyncClient(transport=transport) as client:
            with pytest.raises(MCPError) as caught:
                await client.get("http://example.test/mcp")
        assert caught.value.kind == "too_large"
        assert chunks.pulled == 200

    import asyncio
    asyncio.run(scenario())


def test_summarize_rejects_empty_and_keeps_a_short_value():
    with pytest.raises(MCPError) as caught:
        summarize_result("[]", "prometheus", 10)
    assert caught.value.kind == "empty"
    assert summarize_result('[{"value": "9"}]', "loki", 10)["lines"] == 1


def test_default_compose_has_empty_mcp_urls_and_no_mcp_service():
    document = __import__("yaml").safe_load(Path(__file__).resolve().parents[2].joinpath("docker-compose.yml").read_text())
    names = set(document["services"])
    assert {"prometheus", "loki", "prometheus-mcp", "loki-mcp"}.isdisjoint(names)
    env = document["services"]["agent"]["environment"]
    assert env["PROMETHEUS_MCP_URL"] == "${PROMETHEUS_MCP_URL:-}"
    assert env["PROMETHEUS_MCP_TOKEN"] == "${PROMETHEUS_MCP_TOKEN:-}"
    assert env["LOKI_MCP_URL"] == "${LOKI_MCP_URL:-}"
    assert env["LOKI_MCP_TOKEN"] == "${LOKI_MCP_TOKEN:-}"
    assert env["MCP_TIMEOUT_SECONDS"] == "${MCP_TIMEOUT_SECONDS:-10}"
    assert env["PROMETHEUS_SERVICE_SELECTORS"] == "${PROMETHEUS_SERVICE_SELECTORS:-}"
    assert env["LOKI_SERVICE_SELECTORS"] == "${LOKI_SERVICE_SELECTORS:-}"
