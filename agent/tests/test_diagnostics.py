from datetime import datetime, timezone

from alertflow_agent.integrations.project_diagnostics import validate

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
