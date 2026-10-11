from datetime import datetime, timezone

import httpx

from alertflow_agent.config import Settings
from alertflow_agent.integrations.project_diagnostics import fetch, load_config, validate
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.db import MemoryStore

EVENT = datetime(2026, 10, 11, 10, 0, tzinfo=timezone.utc)


def _item(**extra):
    item = {
        "kind": "component_health",
        "name": "api",
        "status": "healthy",
        "observed_at": "2026-10-11T09:58:00Z",
        "summary": "ok",
        "attributes": {},
    }
    item.update(extra)
    return item


def test_invalid_json_naive_time_and_attribute_limits():
    assert validate(["not", "object"], EVENT)["coverage"] == "invalid"
    naive = {"schema_version": "v1", "items": [_item(observed_at="2026-10-11T09:58:00")]}
    assert validate(naive, EVENT)["coverage"] == "invalid"
    wide = _item(attributes={str(i): "x" for i in range(21)})
    assert validate({"schema_version": "v1", "items": [wide]}, EVENT)["coverage"] == "invalid"
    long_value = _item(attributes={"note": "x" * 201})
    assert validate({"schema_version": "v1", "items": [long_value]}, EVENT)["coverage"] == "invalid"
    mixed = {"schema_version": "v1", "items": [_item(), "bad"]}
    partial = validate(mixed, EVENT)
    assert partial["coverage"] == "partial"
    assert len(partial["items"]) == 1
    stale = validate({"schema_version": "v1", "items": [_item(observed_at="2026-10-11T09:00:00Z")]}, EVENT)
    assert stale["coverage"] == "stale"
    assert stale["items"][0]["limitations"]


def test_config_and_transport_failures_stay_on_the_diagnostic_source(tmp_path, monkeypatch):
    missing = load_config("")
    assert fetch(missing, "a", "b")["coverage"] == "not_configured"
    broken = tmp_path / "bad.yaml"
    broken.write_text(":\n  - [", encoding="utf-8")
    assert fetch(load_config(str(broken)), "a", "b")["coverage"] == "invalid"
    bad_url = tmp_path / "url.yaml"
    bad_url.write_text("url: ftp://files.example/diag\n", encoding="utf-8")
    assert fetch(load_config(str(bad_url)), "a", "b")["coverage"] == "invalid"

    def redirect(*_args, **_kwargs):
        return httpx.Response(302, headers={"location": "https://elsewhere.example"})

    monkeypatch.setattr(httpx, "get", redirect)
    assert fetch({"url": "https://diag.example"}, "2026-10-11T09:00:00+00:00", "2026-10-11T10:00:00+00:00")["coverage"] == "invalid"

    def timeout(*_args, **_kwargs):
        raise httpx.TimeoutException("slow")

    monkeypatch.setattr(httpx, "get", timeout)
    assert fetch({"url": "https://diag.example"}, "2026-10-11T09:00:00+00:00", "2026-10-11T10:00:00+00:00")["coverage"] == "unavailable"

    def huge(*_args, **_kwargs):
        return httpx.Response(200, content=b"{" + b"x" * (512 * 1024))

    monkeypatch.setattr(httpx, "get", huge)
    assert fetch({"url": "https://diag.example"}, "2026-10-11T09:00:00+00:00", "2026-10-11T10:00:00+00:00")["coverage"] == "invalid"

    def bad_json(*_args, **_kwargs):
        return httpx.Response(200, content=b"not-json")

    monkeypatch.setattr(httpx, "get", bad_json)
    assert fetch({"url": "https://diag.example"}, "2026-10-11T09:00:00+00:00", "2026-10-11T10:00:00+00:00")["coverage"] == "invalid"

    runner = Runner(Settings(diagnostics_file=str(broken)), MemoryStore(), None, ScriptedModel())
    state = {
        "run_id": "1",
        "coverage": {"from": 1_791_709_200, "to": 1_791_712_800, "gaps": []},
        "event": {"occurred_at": "2026-10-11T10:00:00Z", "service_id": "api"},
        "evidence": {},
    }
    assert runner.collect_diagnostics(state)["diagnostic_source"] == "invalid"
    assert "DIAGNOSTICS_INVALID" in state["coverage"]["gaps"]
