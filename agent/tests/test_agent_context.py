from alertflow_agent.runtime.graph import Runner, _epoch
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.schemas.events import (
    MAX_CONTEXT_CHECKS,
    MAX_CONTEXT_COMMENTS,
    MAX_CONTEXT_METRIC_POINTS,
    MAX_CONTEXT_RELATED_INCIDENTS,
    MAX_CONTEXT_TEXT,
    sanitize_context,
)
from tests.fakes import FakeAlertFlow


class ExplodingAlertFlow:
    """任何一次调用都失败——用来证明上下文够用时不会再回头找 AlertFlow。"""

    def __getattr__(self, name):
        def boom(*args, **kwargs):
            raise AssertionError(f"AlertFlow.{name}() must not be called when a context is provided")

        return boom


def _context():
    return {
        "incident": {
            "id": 1, "serviceId": "api", "serviceName": "API", "status": "ongoing",
            "severity": "outage", "title": "API down",
            "startedAt": "2026-10-11T09:59:00Z", "resolvedAt": None,
            "comments": [{"id": 7, "username": "ops", "body": "checked the host", "createdAt": "2026-10-11T10:00:00Z"}],
            "externalAlerts": [{"source": "pager", "alertKey": "a", "status": "firing", "title": "firing"}],
        },
        "service": {
            "id": "api", "name": "API", "url": "https://example.com/health", "present": True,
            "check": {"interval": 60, "method": "GET", "timeout": 10, "expectedStatus": [200], "retryAttempts": 1, "retryDelays": []},
        },
        "triggeringCheck": {"id": 11, "ts": "2026-10-11T09:59:00Z", "status": "offline", "latencyMs": 15, "statusCode": 503, "error": "Service Unavailable", "attempts": 1},
        "recentChecks": [
            {"id": 10, "ts": "2026-10-11T09:30:00Z", "status": "online", "latencyMs": 20, "statusCode": 200, "error": "", "attempts": 1},
            {"id": 11, "ts": "2026-10-11T09:59:00Z", "status": "offline", "latencyMs": 15, "statusCode": 503, "error": "Service Unavailable", "attempts": 1},
        ],
        "metrics": {
            "from": "2026-10-10T10:00:00Z", "to": "2026-10-11T10:00:00Z", "bucketSeconds": 900,
            "series": [{"ts": 1791000000, "avgLatency": 20.0, "errors": 0, "warnings": 0}], "tls": None,
        },
        "relatedIncidents": [
            {"id": 9, "serviceId": "api", "status": "resolved", "severity": "outage", "title": "earlier", "startedAt": "2026-10-01T00:00:00Z", "resolvedAt": None},
        ],
        "timeWindow": {"eventTime": "2026-10-11T10:00:00Z", "from": "2026-10-10T10:00:00Z", "to": "2026-10-11T10:00:00Z", "lookbackSeconds": 86400},
        "coverage": {"complete": True, "truncated": [], "gaps": [], "sources": {"service": "ok", "checks": "ok", "metrics": "ok", "comments": "ok"}},
    }


def _state(context):
    return {
        "run_id": "r1",
        "event": {
            "event": "incident_start", "incident_id": 1, "service_id": "api",
            "service_name": "API", "service_url": "https://example.com/health",
            "status": "down", "occurred_at": "2026-10-11T10:00:00Z",
        },
        "evidence": {}, "raw_checks": [], "series": [],
        "coverage": {"from": 0, "to": 0, "gaps": []},
        "source_status": {}, "interval": None,
        "provided_context": context,
    }


def test_context_seeds_evidence_without_calling_alertflow():
    runner = Runner(Settings(), MemoryStore(), ExplodingAlertFlow(), ScriptedModel())
    state = _state(_context())
    out = runner.collect_alertflow(state)

    for ident in ("inc:1", "cmt:1:7", "ext:1:pager:a", "svc:api", "met:24h", "chk:11", "chk:10", "inc:9"):
        assert ident in out["evidence"], ident
    assert out["interval"] == 60
    assert [row["id"] for row in out["raw_checks"]] == [10, 11]
    assert out["source_status"]["provided_context"] == "ok"
    assert out["source_status"]["checks"] == "ok"
    # 窗口取自上下文声明的时间窗（围绕事件），而不是投递那一刻。
    assert out["coverage"]["from"] == _epoch("2026-10-10T10:00:00Z")
    assert out["coverage"]["to"] == _epoch("2026-10-11T10:00:00Z")
    # 上下文本身不进事件字段。
    assert "diagnostic_context" not in out["event"]


def test_empty_context_falls_back_to_live_reads():
    runner = Runner(Settings(), MemoryStore(), FakeAlertFlow(), ScriptedModel())
    state = _state({"coverage": {"sources": {"incident": "unavailable"}}})
    out = runner.collect_alertflow(state)
    assert "CONTEXT_INSUFFICIENT" in out["coverage"]["gaps"]
    # 实时读取回来了，所以 Incident 证据仍然在。
    assert "inc:1" in out["evidence"]
    assert out["source_status"]["incident"] == "ok"


def test_no_context_uses_live_reads():
    runner = Runner(Settings(), MemoryStore(), FakeAlertFlow(), ScriptedModel())
    out = runner.collect_alertflow(_state(None))
    assert "inc:1" in out["evidence"]
    assert "met:24h" in out["evidence"]
    assert out["source_status"]["incident"] == "ok"


def test_context_gaps_and_truncation_are_carried_over():
    context = _context()
    context["coverage"] = {
        "complete": False,
        "truncated": ["CHECKS_TRUNCATED"],
        "gaps": ["NO_TRIGGERING_CHECK"],
        "sources": {"checks": "ok"},
    }
    runner = Runner(Settings(), MemoryStore(), ExplodingAlertFlow(), ScriptedModel())
    out = runner.collect_alertflow(_state(context))
    assert "CHECKS_TRUNCATED" in out["coverage"]["gaps"]
    assert "NO_TRIGGERING_CHECK" in out["coverage"]["gaps"]


def test_sanitize_context_caps_every_list():
    raw = {
        "recentChecks": [{"id": i, "error": "x" * 10} for i in range(MAX_CONTEXT_CHECKS + 40)],
        "metrics": {"series": [{"ts": i} for i in range(MAX_CONTEXT_METRIC_POINTS + 40)]},
        "incident": {
            "id": 1, "title": "t" * (MAX_CONTEXT_TEXT + 500),
            "comments": [{"id": i, "body": "y" * (MAX_CONTEXT_TEXT + 500)} for i in range(MAX_CONTEXT_COMMENTS + 20)],
        },
        "relatedIncidents": [{"id": i} for i in range(MAX_CONTEXT_RELATED_INCIDENTS + 20)],
        "coverage": {"complete": True, "truncated": []},
    }
    out = sanitize_context(raw)
    assert len(out["recentChecks"]) == MAX_CONTEXT_CHECKS
    assert len(out["metrics"]["series"]) == MAX_CONTEXT_METRIC_POINTS
    assert len(out["incident"]["comments"]) == MAX_CONTEXT_COMMENTS
    assert len(out["relatedIncidents"]) == MAX_CONTEXT_RELATED_INCIDENTS
    assert len(out["incident"]["title"]) <= MAX_CONTEXT_TEXT + 1
    # 保留最新的检查，而不是最早的。
    assert out["recentChecks"][-1]["id"] == MAX_CONTEXT_CHECKS + 39
    assert "CHECKS_TRUNCATED_BY_AGENT" in out["coverage"]["truncated"]
    assert out["coverage"]["complete"] is False


def test_sanitize_context_keeps_an_existing_truncation_record():
    out = sanitize_context({"coverage": {"complete": False, "truncated": ["FROM_GO"]}})
    assert "FROM_GO" in out["coverage"]["truncated"]
