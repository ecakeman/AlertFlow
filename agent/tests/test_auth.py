from fastapi.testclient import TestClient

from alertflow_agent.api.app import create_app
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from tests.test_webhook import _event


def _client(**kwargs):
    settings = Settings(agent_webhook_token="hook", agent_api_token="api", **kwargs)
    return TestClient(create_app(settings, MemoryStore(), runner=None))


def test_bearer_six_ways():
    client = _client()
    ok = client.post("/webhooks/alertflow", json=_event(), headers={"Authorization": "Bearer hook"})
    assert ok.status_code == 202
    assert client.post("/webhooks/alertflow", json=_event("incident_start", "incident_start:2")).status_code == 401
    assert client.post("/webhooks/alertflow", json=_event("incident_start", "incident_start:3"), headers={"Authorization": "hook"}).status_code == 401
    assert client.post("/webhooks/alertflow", json=_event("incident_start", "incident_start:4"), headers={"Authorization": "Basic hook"}).status_code == 401
    assert client.post("/webhooks/alertflow", json=_event("incident_start", "incident_start:5"), headers={"Authorization": "Bearer nope"}).status_code == 401
    assert client.get("/api/v1/runs", headers={"Authorization": "Bearer api"}).status_code == 200

    open_client = TestClient(create_app(Settings(), MemoryStore(), runner=None))
    assert open_client.post("/webhooks/alertflow", json=_event()).status_code == 503
    assert open_client.get("/api/v1/runs", headers={"Authorization": "Bearer api"}).status_code == 401
