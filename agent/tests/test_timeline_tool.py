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
                "occurredAt": 1_700_000_000, "observedAt": 1_700_000_100, "recordedAt": 1_700_000_120,
                "clusterId": "local", "namespace": "prod", "resourceKind": "Deployment",
                "resourceName": "api", "resourceUid": "uid-1", "details": {"reason": "timeout"},
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
    stored = state["evidence"]["diagnostics:api:recent_error:db:1"]
    assert stored["source_type"] == "application_diagnostics"
    assert stored["data"]["resource_uid"] == "uid-1"
    assert stored["data"]["details"] == {"reason": "timeout"}
    from alertflow_agent.runtime.verify import verify
    draft = {
        "summary": "x",
        "observations": [{"id": "o1", "text": "x", "evidence_ids": ["no-such-evidence"], "fact_ids": []}],
        "hypotheses": [], "unknowns": [], "recommended_next_steps": [],
    }
    result = verify(draft, state["evidence"], [])
    assert not result["passed"]
    assert any(item["rule"] == "evidence_ref" for item in result["findings"])


def test_timeline_mode_registers_events_and_objects():
    from alertflow_agent.runtime.graph import Runner
    from alertflow_agent.runtime.llm import ScriptedModel
    from alertflow_agent.db import MemoryStore

    class Client:
        def timeline(self, service_id, start, end, sources, limit=100, cursor_at=0, cursor_id=0):
            return {"events": [{
                "eventKey": "k8s:pod:1", "source": "kubernetes", "eventType": "kubernetes.pod.changed",
                "summary": "restarted", "occurredAt": 1, "observedAt": 2, "recordedAt": 3,
                "clusterId": "local", "namespace": "prod", "resourceKind": "Pod",
                "resourceName": "api", "resourceUid": "uid-1", "details": {"reason": "OOM"},
            }]}

        def kubernetes_state(self, service_id):
            return {"objects": [{
                "uid": "uid-1", "kind": "Pod", "name": "api", "namespace": "prod", "clusterId": "local",
                "active": True, "resourceVersion": "9", "fingerprint": "fp", "summary": '{"phase":"Running"}',
            }]}

    runner = Runner(Settings(), MemoryStore(), Client(), ScriptedModel())
    state = {"run_id": "r", "event": {"service_id": "api", "occurred_at": "2026-10-28T12:00:00Z"}, "evidence": {}, "coverage": {"gaps": []}}
    runner.collect_diagnostics(state)
    assert state["evidence"]["k8s:pod:1"]["data"]["resource_uid"] == "uid-1"
    assert state["evidence"]["k8s:pod:1"]["data"]["details"]["reason"] == "OOM"
    assert state["evidence"]["k8s:uid-1"]["data"]["fingerprint"] == "fp"
    assert "TIMELINE_EMPTY" not in state["coverage"]["gaps"]

    empty = Runner(Settings(), MemoryStore(), None, ScriptedModel())
    blank = {"run_id": "r", "event": {"service_id": "api", "occurred_at": "2026-10-28T12:00:00Z"}, "evidence": {}, "coverage": {"gaps": []}}
    empty.collect_diagnostics(blank)
    assert "TIMELINE_EMPTY" in blank["coverage"]["gaps"]
    assert blank["evidence"] == {}


def test_empty_timeline_is_not_success():
    class Empty(_Client):
        def timeline(self, service_id, start, end, sources, limit=100, cursor_at=0, cursor_id=0):
            self.calls.append((start, end))
            return {"events": [], "coverage": [], "from": start, "to": end}

    state = _state()
    result = run_tool("query_service_timeline", {}, state, Settings(max_query_calls=1), {"alertflow": Empty()})
    assert result["status"] == "empty"
    assert "TIMELINE_EMPTY" in state["coverage"]["gaps"]
