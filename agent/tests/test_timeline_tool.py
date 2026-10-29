from alertflow_agent.config import Settings
from alertflow_agent.runtime.tools import run_tool


class _Client:
    def __init__(self):
        self.calls = []

    def timeline(self, service_id, start, end, sources, limit=100, cursor_at=0, cursor_id=0):
        self.calls.append((start, end))
        if end - start < 24 * 3600:
            return {"events": [], "coverage": [{"source": "application_diagnostics", "status": "empty"}], "from": start, "to": end}
        return {
            "events": [{
                "id": 1, "eventKey": "diagnostics:api:recent_error:db:1", "source": "application_diagnostics",
                "eventType": "diagnostics.recent_error", "title": "db", "summary": "timeout",
                "occurredAt": 1_700_000_000, "observedAt": 1_700_000_100,
            }],
            "coverage": [{"source": "application_diagnostics", "status": "ok"}],
            "from": start, "to": end,
        }


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-28T12:00:00Z"},
        "evidence": {},
        "query_calls": 0,
        "coverage": {"gaps": []},
    }


def test_timeline_expands_until_an_event_and_registers_evidence():
    client = _Client()
    state = _state()
    result = run_tool("query_service_timeline", {}, state, Settings(), {"alertflow": client})
    assert result["status"] == "success"
    assert len(client.calls) == 2
    assert "diagnostics:api:recent_error:db:1" in state["evidence"]
    assert state["evidence"]["diagnostics:api:recent_error:db:1"]["source_type"] == "application_diagnostics"


def test_empty_timeline_is_not_success():
    class Empty(_Client):
        def timeline(self, service_id, start, end, sources, limit=100, cursor_at=0, cursor_id=0):
            self.calls.append((start, end))
            return {"events": [], "coverage": [], "from": start, "to": end}

    state = _state()
    result = run_tool("query_service_timeline", {}, state, Settings(max_query_calls=1), {"alertflow": Empty()})
    assert result["status"] == "empty"
    assert "TIMELINE_EMPTY" in state["coverage"]["gaps"]
