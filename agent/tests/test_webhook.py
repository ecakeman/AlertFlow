import json

from fastapi.responses import JSONResponse
from fastapi.testclient import TestClient

from alertflow_agent.api.app import _ingest_v1, create_app
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore


def _settings():
    return Settings(agent_api_token="api", agent_enable_warning_runs=False, agent_revise_on_resolve=True)


def _event(event="incident_start", event_id="incident_start:1"):
    return {
        "schema_version": "v1",
        "event_id": event_id,
        "event": event,
        "incident_id": 1,
        "service_id": "api",
        "service": "API",
        "url": "https://example.com/health",
        "status": "down",
        "occurred_at": "2026-10-11T10:00:00Z",
        "message": "down",
    }


class _Reply:
    def __init__(self, response) -> None:
        if isinstance(response, JSONResponse):
            self.status_code = response.status_code
            self._body = json.loads(response.body)
        else:
            self.status_code = 200
            self._body = response

    def json(self):
        return self._body


def _post(store, body):
    return _Reply(_ingest_v1(body, store, _settings()))


def test_dedupe_warning_and_resolve():
    store = MemoryStore()
    first = _post(store, _event())
    assert first.status_code == 202
    again = _post(store, _event())
    assert again.status_code == 200
    assert again.json()["action"] == "duplicate"
    assert len(store.list_runs()) == 1
    warning = _post(store, _event("warning", "warning:9"))
    assert warning.json()["action"] == "ignored"
    resolved = _post(store, _event("incident_resolve", "incident_resolve:1"))
    assert resolved.status_code == 202
    assert store.get(first.json()["run_id"])["status"] == "queued"
    repeat = _post(store, _event("incident_resolve", "incident_resolve:1"))
    assert repeat.status_code == 200
    assert repeat.json()["action"] == "duplicate"
    store.save_report(first.json()["run_id"], 2, {"summary": "v2"})
    third = _post(store, _event("incident_resolve", "incident_resolve:1"))
    assert third.json()["action"] == "duplicate"


def test_webhook_route_is_gone_and_run_api_remains():
    client = TestClient(create_app(_settings(), MemoryStore(), runner=None))
    gone = client.post("/webhooks/alertflow", json=_event())
    assert gone.status_code == 404
    assert client.get("/healthz").status_code == 200
    assert client.get("/api/v1/runs", headers={"Authorization": "Bearer api"}).status_code == 200
