from fastapi.testclient import TestClient

from alertflow_agent.api.app import create_app
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore


def _settings():
    return Settings(agent_webhook_token="hook", agent_api_token="api", agent_enable_warning_runs=False, agent_revise_on_resolve=True)


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


def test_dedupe_warning_and_resolve():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    first = client.post("/webhooks/alertflow", json=_event(), headers={"Authorization": "Bearer hook"})
    assert first.status_code == 202
    again = client.post("/webhooks/alertflow", json=_event(), headers={"Authorization": "Bearer hook"})
    assert again.status_code == 200
    assert again.json()["action"] == "duplicate"
    assert len(store.list_runs()) == 1
    warning = client.post("/webhooks/alertflow", json=_event("warning", "warning:9"), headers={"Authorization": "Bearer hook"})
    assert warning.json()["action"] == "ignored"
    missing = client.post("/webhooks/alertflow", json=_event("incident_resolve", "incident_resolve:1"), headers={"Authorization": "Bearer wrong"})
    assert missing.status_code == 401
    resolved = client.post("/webhooks/alertflow", json=_event("incident_resolve", "incident_resolve:1"), headers={"Authorization": "Bearer hook"})
    assert resolved.status_code == 202
    assert store.get(first.json()["run_id"])["status"] == "queued"
    repeat = client.post("/webhooks/alertflow", json=_event("incident_resolve", "incident_resolve:1"), headers={"Authorization": "Bearer hook"})
    # no report yet, so a second resolve still queues; a saved v2 would be a duplicate
    store.save_report(first.json()["run_id"], 2, {"summary": "v2"})
    third = client.post("/webhooks/alertflow", json=_event("incident_resolve", "incident_resolve:1"), headers={"Authorization": "Bearer hook"})
    assert third.json()["action"] == "duplicate"
    assert repeat.status_code == 202
