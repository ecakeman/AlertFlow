from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.integrations.mcp import HTTPSource, MCPError, sources_from_settings
from alertflow_agent.runtime.context import build_blocks
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.runtime.tools import run_tool
from tests.fakes import FakeAlertFlow, event_row


class Recorder:
    def __init__(self, text="up", error=None):
        self.calls = []
        self.text = text
        self.error = error

    def call(self, name, args):
        self.calls.append((name, args))
        if self.error:
            raise self.error
        return self.text


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"},
        "evidence": {},
        "coverage": {"gaps": []},
    }


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
    result = run_tool("loki_query", {"query": "{job=\"a\"}"}, _state(), Settings(), {"mcp": client})
    assert result["note"] == "unknown tool"
    assert client.calls == []


def test_configured_query_writes_evidence_once():
    client = Recorder(text="value 1")
    state = _state()
    settings = Settings(prometheus_mcp_url="http://prometheus.example/mcp")
    result = run_tool("query", {"query": "up"}, state, settings, {"mcp": client})
    assert result["status"] == "success"
    assert client.calls == [("query", {"query": "up"})]
    item = state["evidence"][result["evidence_ids"][0]]
    assert item["source_type"] == "prometheus"
    assert item["summary"] == "value 1"
    assert run_tool("series", {}, state, settings, {"mcp": client})["note"] == "unknown tool"
    assert len(client.calls) == 1


def test_loki_failure_records_a_gap_without_evidence():
    client = Recorder(error=MCPError("auth"))
    state = _state()
    settings = Settings(loki_mcp_url="http://loki.example/stream")
    result = run_tool("loki_query", {"query": "{job=\"a\"}"}, state, settings, {"mcp": client})
    assert result["status"] == "error"
    assert "no evidence" in result["note"]
    assert state["evidence"] == {}
    assert state["coverage"]["gaps"] == ["LOKI_NO_EVIDENCE"]


def test_failed_prometheus_read_still_finishes_the_run():
    store = MemoryStore()
    row = event_row("mcp")
    row["input_event"]["script"] = ["query", "finish"]
    store.insert_run(row)
    settings = Settings(prometheus_mcp_url="http://prometheus.example/mcp")
    client = Recorder(error=MCPError("timeout"))
    runner = Runner(settings, store, FakeAlertFlow(), ScriptedModel(), mcp=client)
    runner.execute(row["id"])
    assert [name for name, _ in client.calls] == ["query"]
    saved = store.get(row["id"])
    report = store.latest_report(row["id"])["report"]
    assert saved["status"] == "completed"
    assert report["usage"]["tool_calls"] == 2
    assert "PROMETHEUS_NO_EVIDENCE" in report["coverage"]["gaps"]
    assert any(item["text"] == "Prometheus has no evidence." for item in report["unknowns"])
    assert store.get_evidence(row["id"]) == {} or "prometheus" not in {
        item["source_type"] for item in store.get_evidence(row["id"]).values()
    }


def test_lead_context_lists_only_configured_tools():
    quiet = build_blocks("lead", {"event": {}})
    assert all(block.name != "mcp_tools" for block in quiet)
    named = build_blocks("lead", {"event": {}, "mcp_tools": ["loki_query"]})
    block = next(block for block in named if block.name == "mcp_tools")
    listed = block.variants[0].split("only these: ", 1)[1].split(".", 1)[0]
    assert listed == "loki_query"
