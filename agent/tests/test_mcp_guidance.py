"""P6：指标/日志工具必须在 HTTP/运行时故障时被要求查询，且没查要如实进 coverage。"""

from pathlib import Path

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.runtime.context import build_blocks
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.runtime.tools import run_tool
from tests.fakes import FakeAlertFlow, event_row

PROMPTS = Path(__file__).resolve().parents[1] / "src" / "alertflow_agent" / "prompts"


def _state(**overrides):
    state = {
        "event": {"event": "incident_start", "service_id": "api", "status": 503,
                  "error": "Service Unavailable"},
        "evidence": {},
        "raw_checks": [{"id": 11, "status_code": 503, "error": "Service Unavailable"}],
        "mcp_tools": ["query", "range_query", "loki_query"],
        "coverage": {"from": 0, "to": 60, "gaps": []},
        "guard": {},
    }
    state.update(overrides)
    return state


def test_lead_prompt_requires_metrics_and_logs_for_runtime_faults():
    text = (PROMPTS / "lead.md").read_text(encoding="utf-8").lower()
    assert "query" in text and "loki_query" in text
    assert "at least once" in text
    assert "coverage" in text


def test_planner_prompt_plans_metrics_and_logs():
    text = (PROMPTS / "planner.md").read_text(encoding="utf-8").lower()
    assert "metrics" in text and "logs" in text


def test_context_mcp_block_states_the_requirement():
    blocks = build_blocks("lead", {
        "event": {},
        "mcp_tools": ["query", "range_query", "loki_query"],
        "mcp_scope": 'Prometheus matcher for this service: {service="api"}.',
    })
    block = next(item for item in blocks if item.name == "mcp_tools")
    text = block.variants[0]
    # 既有“只列已配置工具”的约定保持不变
    assert text.startswith("Optional evidence tools, only these: query, range_query, loki_query.")
    assert "at least once" in text and "finish_investigation" in text
    assert "coverage" in text


def test_finish_marks_enabled_sources_as_empty_when_never_queried():
    settings = Settings(prometheus_mcp_url="http://prom", loki_mcp_url="http://loki")
    state = _state()
    result = run_tool("finish_investigation", {"note": "done"}, state, settings, {})
    assert result["status"] == "success"
    assert state["coverage"]["gaps"] == ["PROMETHEUS_NOT_QUERIED", "LOKI_NOT_QUERIED"]
    assert state["mcp_activity"]["prometheus"] == {"calls": 0, "status": "empty"}
    assert state["mcp_activity"]["loki"] == {"calls": 0, "status": "empty"}


def test_finish_requires_queries_when_checks_exist_but_event_is_not_5xx():
    """判据是“本 run 有 HTTP 检查样本”，不是 monitor 事件的 5xx 形状。"""
    settings = Settings(prometheus_mcp_url="http://prom", loki_mcp_url="http://loki")
    state = _state(
        event={"event": "incident_start", "service_id": "api", "status": "firing",
               "error": ""},
        raw_checks=[{"id": 10, "status_code": 503, "error": "Service Unavailable"}],
    )
    run_tool("finish_investigation", {}, state, settings, {})
    assert state["coverage"]["gaps"] == ["PROMETHEUS_NOT_QUERIED", "LOKI_NOT_QUERIED"]


def test_finish_does_not_require_queries_without_http_check_data():
    """确实没有任何 HTTP 检查数据时，允许跳过指标/日志查询。"""
    settings = Settings(prometheus_mcp_url="http://prom", loki_mcp_url="http://loki")
    state = _state(
        event={"event": "incident_start", "service_id": "api", "status": "up"},
        raw_checks=[],
    )
    run_tool("finish_investigation", {}, state, settings, {})
    assert state["coverage"]["gaps"] == []


def test_finish_does_not_mark_disabled_sources():
    state = _state()
    run_tool("finish_investigation", {}, state, Settings(), {})
    assert state["coverage"]["gaps"] == []
    assert not state.get("mcp_activity")


def test_report_coverage_lists_each_source():
    store = MemoryStore()
    row = event_row("agent")
    store.insert_run(row)
    settings = Settings(prometheus_mcp_url="http://prom", loki_mcp_url="http://loki")
    Runner(settings, store, FakeAlertFlow(), ScriptedModel()).execute(row["id"])
    report = store.latest_report(row["id"])["report"]
    sources = report["coverage"]["sources"]
    assert sources["prometheus"] == {"enabled": True, "calls": 0, "status": "empty"}
    assert sources["loki"] == {"enabled": True, "calls": 0, "status": "empty"}
    assert sources["alertflow"]["enabled"] is True
    assert sources["alertflow"]["status"] == "ok"
    assert sources["runbook"] == {"enabled": False, "status": "disabled"}
    # FakeAlertFlow 的检查里有 503，属于 HTTP/运行时类故障：一次没查要记进 coverage
    assert "PROMETHEUS_NOT_QUERIED" in report["coverage"]["gaps"]
    assert "LOKI_NOT_QUERIED" in report["coverage"]["gaps"]


def test_finish_still_requires_queries_for_an_event_level_5xx():
    """没有检查样本时，事件本身的 5xx 仍作为补充判据（保持原行为）。"""
    settings = Settings(prometheus_mcp_url="http://prom", loki_mcp_url="http://loki")
    state = _state(raw_checks=[])
    run_tool("finish_investigation", {}, state, settings, {})
    assert state["coverage"]["gaps"] == ["PROMETHEUS_NOT_QUERIED", "LOKI_NOT_QUERIED"]
