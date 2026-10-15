from fastapi.testclient import TestClient

from alertflow_agent.api.app import create_app
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore

AUTH = {"Authorization": "Bearer hook"}


def _settings(**overrides):
    base = dict(agent_webhook_token="hook", agent_api_token="api", agent_enable_warning_runs=False)
    base.update(overrides)
    return Settings(**base)


def _event(event="incident_start", event_id="incident_start:1"):
    return {
        "event_id": event_id,
        "event": event,
        "incident_id": 1,
        "service_id": "api",
        "service_name": "API",
        "service_url": "https://example.com/health",
        "status": "down",
        "severity": "outage",
        "occurred_at": "2026-10-11T10:00:00Z",
    }


def _v2(message_id="incident_start:1", message_type="diagnostic.requested", *, event=None, context=None):
    payload = {"event": event or _event()}
    if context is not None:
        payload["diagnostic_context"] = context
    return {
        "schema_version": "v2",
        "message_id": message_id,
        "message_type": message_type,
        "correlation_id": "incident:1",
        "sent_at": "2026-10-11T10:00:01Z",
        "payload": payload,
    }


def test_v2_ingest_stores_context_and_dedupes_on_message_id():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    context = {"incident": {"id": 1}, "coverage": {"complete": True, "truncated": [], "gaps": []}}

    first = client.post("/webhooks/alertflow", json=_v2(context=context), headers=AUTH)
    assert first.status_code == 202
    assert first.json()["action"] == "started"

    again = client.post("/webhooks/alertflow", json=_v2(context=context), headers=AUTH)
    assert again.status_code == 200
    assert again.json()["action"] == "duplicate"
    assert len(store.list_runs()) == 1

    row = store.get(first.json()["run_id"])
    assert row["dedupe_key"] == "incident_start:1"
    assert row["input_event"]["diagnostic_context"]["incident"]["id"] == 1
    assert row["input_event"]["event"] == "incident_start"


def test_v2_requires_token():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    missing = client.post("/webhooks/alertflow", json=_v2(), headers={"Authorization": "Bearer wrong"})
    assert missing.status_code == 401


def test_v2_rejects_unknown_message_type_and_missing_id():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    bad_type = client.post("/webhooks/alertflow", json=_v2(message_type="something.else"), headers=AUTH)
    assert bad_type.status_code == 400
    assert "message_type" in bad_type.json()["error"]

    no_id = _v2()
    no_id["message_id"] = ""
    missing = client.post("/webhooks/alertflow", json=no_id, headers=AUTH)
    assert missing.status_code == 400
    assert "message_id" in missing.json()["error"]


def test_v2_test_message_does_not_create_a_run():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    response = client.post("/webhooks/alertflow", json=_v2(message_type="diagnostic.test"), headers=AUTH)
    assert response.status_code == 200
    assert response.json()["action"] == "ignored"
    assert store.list_runs() == []


def test_v2_warning_respects_the_warning_switch():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    body = _v2(message_id="warning:7", message_type="diagnostic.warning", event=_event("warning", "warning:7"))
    ignored = client.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert ignored.json()["action"] == "ignored"
    assert store.list_runs() == []

    on = TestClient(create_app(_settings(agent_enable_warning_runs=True), store, runner=None))
    started = on.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert started.status_code == 202
    assert store.get(started.json()["run_id"])["mode"] == "baseline"


def test_v2_resolve_records_against_the_existing_run():
    store = MemoryStore()
    # revise 是设置项：只有开着才会把已有 run 推到 revise 阶段。
    client = TestClient(create_app(_settings(agent_revise_on_resolve=True), store, runner=None))
    started = client.post("/webhooks/alertflow", json=_v2(), headers=AUTH)
    resolved_event = _event("incident_resolve", "incident_resolve:1")
    resolved_event["resolved_at"] = "2026-10-11T10:05:00Z"
    body = _v2(message_id="incident_resolve:1", message_type="diagnostic.resolved",
               event=resolved_event, context={"incident": {"id": 1}})

    first = client.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert first.status_code == 202
    # 还没跑完的 run 只记下 resolve（记录），跑完的才会被推到 revise 阶段。
    assert first.json()["action"] in {"recorded", "revise"}
    assert store.get(started.json()["run_id"])["input_event"]["resolved_at"] == "2026-10-11T10:05:00Z"

    repeat = client.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert repeat.json()["action"] == "duplicate"


def test_body_limit_allows_512kib_but_not_more():
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))

    big = _v2(context={"incident": {"id": 1}, "padding": "x" * 300_000})
    accepted = client.post("/webhooks/alertflow", json=big, headers=AUTH)
    assert accepted.status_code == 202, accepted.text

    too_big = _v2(message_id="incident_start:2", context={"incident": {"id": 1}, "padding": "x" * (600 * 1024)})
    rejected = client.post("/webhooks/alertflow", json=too_big, headers=AUTH)
    assert rejected.status_code == 413


def test_v1_payloads_still_work():
    """迁移期间两条协议并存：v1 事件必须继续被接受。"""
    store = MemoryStore()
    client = TestClient(create_app(_settings(), store, runner=None))
    body = {
        "schema_version": "v1",
        "event_id": "incident_start:1",
        "event": "incident_start",
        "incident_id": 1,
        "service_id": "api",
        "service": "API",
        "url": "https://example.com/health",
        "status": "down",
        "occurred_at": "2026-10-11T10:00:00Z",
    }
    first = client.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert first.status_code == 202
    again = client.post("/webhooks/alertflow", json=body, headers=AUTH)
    assert again.json()["action"] == "duplicate"
    row = store.get(first.json()["run_id"])
    assert row["dedupe_key"] == "incident_start:1"
    assert "diagnostic_context" not in row["input_event"]
