from alertflow_agent.config import Settings
from alertflow_agent.runtime.tools import run_tool
from tests.fakes import FakeAlertFlow


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z", "status": "down"},
        "evidence": {"inc:1": {"source_type": "alertflow_incident", "data": {}}},
        "raw_checks": [
            {"id": 10, "ts": "2026-10-11T09:30:00Z", "status": "online", "latencyMs": 20, "statusCode": 200, "error": "", "attempts": 1},
            {"id": 11, "ts": "2026-10-11T09:59:00Z", "status": "offline", "latencyMs": 15, "statusCode": 503, "error": "down", "attempts": 1},
        ],
        "coverage": {"from": 1, "to": 2},
        "series": [],
        "analysis_n": 0,
        "guard": {"subagent_runs": 0},
        "diagnostic_calls": 0,
    }


def test_tool_params_and_new_evidence():
    state = _state()
    clients = {"alertflow": FakeAlertFlow()}
    metrics = run_tool("get_metrics_series", {"range": "24h"}, state, Settings(), clients)
    assert metrics["evidence_ids"] == ["met:24h"]
    assert state["evidence"]["met:24h"]["data"]["series"][0]["avgLatency"] == 20
    from alertflow_agent.runtime.context import build_context
    prompt = build_context("lead", {"event": state["event"], "evidence": state["evidence"], "guard": state["guard"], "messages": []})
    assert "points from" in prompt
    assert "ana:latency" in prompt
    assert "min_avg_latency" not in prompt
    history = run_tool("get_check_history", {"from": 2, "to": 1, "limit": 10}, state, Settings(), clients)
    assert history["status"] == "invalid_request"
    limited = run_tool("get_check_history", {"limit": 501}, state, Settings(), clients)
    assert limited["status"] == "invalid_request"
    client = _CommentClient()
    detail = run_tool("get_incident_detail", {"incident_id": 1}, state, Settings(), {"alertflow": client})
    assert "cmt:1:7" in detail["evidence_ids"]
    first = run_tool("analyze_check_timeline", {"around": "2026-10-11T09:59:00Z", "window_minutes": 30}, state, Settings(), clients)
    assert "ana:status@1" in state["evidence"]
    second = run_tool("analyze_check_timeline", {"around": "2026-10-11T09:59:00Z", "window_minutes": 30}, state, Settings(), clients)
    assert "ana:status@2" in second["evidence_ids"]
    assert "ana:status@1" in state["evidence"]
    skill = run_tool("load_skill", {"name": "service-unavailable"}, state, Settings(), clients)
    assert skill["status"] == "success"
    assert "503" in state["skill_texts"]["service-unavailable"]
    wide = run_tool("analyze_check_timeline", {"around": "2026-10-11T09:59:00Z", "window_minutes": 400}, state, Settings(), clients)
    assert wide["status"] == "invalid_request"
    assert first["status"] == "success"


class _CommentClient(FakeAlertFlow):
    def incident(self, incident_id: int) -> dict:
        body = super().incident(incident_id)
        body["comments"] = [{"id": 7, "body": "checked the host"}]
        return body
