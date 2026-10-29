from fastapi.testclient import TestClient

from alertflow_agent.api.app import create_app
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from tests.test_webhook import _event


def _client(**kwargs):
    settings = Settings(agent_webhook_token="hook", agent_api_token="api", **kwargs)
    return TestClient(create_app(settings, MemoryStore(), runner=None))


def test_run_api_still_requires_its_own_token():
    client = _client()
    assert client.get("/api/v1/runs", headers={"Authorization": "Bearer api"}).status_code == 200
    assert client.get("/api/v1/runs").status_code == 401
    assert client.get("/api/v1/runs", headers={"Authorization": "Bearer nope"}).status_code == 401
    assert client.post("/webhooks/alertflow", json=_event()).status_code == 404

    open_client = TestClient(create_app(Settings(), MemoryStore(), runner=None))
    assert open_client.get("/api/v1/runs", headers={"Authorization": "Bearer api"}).status_code == 401
