import json

from fastapi.responses import JSONResponse

from alertflow_agent.api.app import _ingest_v1, _ingest_v2
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore


def _settings(**overrides):
    base = dict(agent_enable_warning_runs=False)
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


def _post(store, body, **settings):
    fn = _ingest_v2 if body.get("schema_version") == "v2" else _ingest_v1
    return _Reply(fn(body, store, _settings(**settings)))


def test_v2_ingest_stores_context_and_dedupes_on_message_id():
    store = MemoryStore()
    context = {"incident": {"id": 1}, "coverage": {"complete": True, "truncated": [], "gaps": []}}
    first = _post(store, _v2(context=context))
    assert first.status_code == 202
    assert first.json()["action"] == "started"
    again = _post(store, _v2(context=context))
    assert again.status_code == 200
    assert again.json()["action"] == "duplicate"
    assert len(store.list_runs()) == 1
    row = store.get(first.json()["run_id"])
    assert row["dedupe_key"] == "incident_start:1"
    assert row["input_event"]["diagnostic_context"]["incident"]["id"] == 1
    assert row["input_event"]["event"] == "incident_start"


def test_v2_rejects_unknown_message_type_and_missing_id():
    store = MemoryStore()
    bad_type = _post(store, _v2(message_type="something.else"))
    assert bad_type.status_code == 400
    assert "message_type" in bad_type.json()["error"]
    no_id = _v2()
    no_id["message_id"] = ""
    missing = _post(store, no_id)
    assert missing.status_code == 400
    assert "message_id" in missing.json()["error"]


def test_v2_test_message_does_not_create_a_run():
    store = MemoryStore()
    response = _post(store, _v2(message_type="diagnostic.test"))
    assert response.status_code == 200
    assert response.json()["action"] == "ignored"
    assert store.list_runs() == []


def test_v2_warning_respects_the_warning_switch():
    store = MemoryStore()
    body = _v2(message_id="warning:7", message_type="diagnostic.warning", event=_event("warning", "warning:7"))
    ignored = _post(store, body)
    assert ignored.json()["action"] == "ignored"
    assert store.list_runs() == []
    started = _post(store, body, agent_enable_warning_runs=True)
    assert started.status_code == 202
    assert store.get(started.json()["run_id"])["mode"] == "baseline"


def test_v2_resolve_records_against_the_existing_run():
    store = MemoryStore()
    started = _post(store, _v2(), agent_revise_on_resolve=True)
    resolved_event = _event("incident_resolve", "incident_resolve:1")
    resolved_event["resolved_at"] = "2026-10-11T10:05:00Z"
    body = _v2(message_id="incident_resolve:1", message_type="diagnostic.resolved",
               event=resolved_event, context={"incident": {"id": 1}})
    first = _post(store, body, agent_revise_on_resolve=True)
    assert first.status_code == 202
    assert first.json()["action"] in {"recorded", "revise"}
    assert store.get(started.json()["run_id"])["input_event"]["resolved_at"] == "2026-10-11T10:05:00Z"
    repeat = _post(store, body, agent_revise_on_resolve=True)
    assert repeat.json()["action"] == "duplicate"


def test_v1_payloads_still_work():
    store = MemoryStore()
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
    first = _post(store, body)
    assert first.status_code == 202
    again = _post(store, body)
    assert again.json()["action"] == "duplicate"
    row = store.get(first.json()["run_id"])
    assert row["dedupe_key"] == "incident_start:1"
    assert "diagnostic_context" not in row["input_event"]
