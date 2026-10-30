"""批次5-2 契约：报告 fact 引用必须落到本 run 的 fact catalog。

旧报告（没有任何 fact 字段）保持可 verify，不得因为新字段而失败。
"""

from __future__ import annotations

from alertflow_agent.runtime.evidence import EvidenceManager
from alertflow_agent.runtime.window import build_investigation_window, scope_from_event
from alertflow_agent.schemas.evidence import EvidenceCandidate


def _manager():
    event = {"service_id": "api", "incident_id": 7, "occurred_at": "2026-10-11T10:00:00Z"}
    window = build_investigation_window(event, lookback_minutes=60, max_window_seconds=3600)
    scope = scope_from_event(event, "run-1", window=window)
    manager = EvidenceManager(scope.run_id, scope)
    manager.register_batch([EvidenceCandidate(
        candidate_id="c1",
        run_id=scope.run_id,
        service_id=scope.service_id,
        incident_id=scope.incident_id,
        source_type="checks",
        source_system="alertflow_grpc",
        source_locator={"ledger_key": "chk:1"},
        window=window,
        payload={"id": 1, "status": "failing"},
        payload_schema="http_check",
        content_digest="",
    )])
    return manager


def _report(**extra):
    body = {
        "summary": "probe failing",
        "observations": [{"id": "o1", "text": "check 1 failing"}],
        "hypotheses": [{
            "id": "h1",
            "text": "process died",
            "layer": "app_internal",
            "status": "supported",
            "supporting_fact_ids": ["f1"],
        }],
        "unknowns": [],
        "recommended_next_steps": [],
    }
    body.update(extra)
    return body


def test_unknown_fact_ids_are_reported_not_silently_passed():
    manager = _manager()
    draft = _report()
    draft["hypotheses"][0]["supporting_fact_ids"] = ["f1", "f404"]
    draft["fact_ids"] = ["f999"]
    result = manager.verify_report(draft, {"chk:1": {}}, [])
    assert result.passed is False
    assert "f404" in result.invalid_fact_refs
    assert "f999" in result.invalid_fact_refs


def test_known_fact_ids_are_resolved_into_the_report_draft():
    manager = _manager()
    result = manager.verify_report(_report(), {"chk:1": {}}, [])
    assert result.passed is True
    assert result.invalid_fact_refs == []
    hypothesis = (manager.last_draft or {})["hypotheses"][0]
    assert hypothesis["supporting_fact_ids"] == ["f1"]
    fact = hypothesis["supporting_facts"][0]
    assert set(fact) == {"evidence_id", "path", "value"}
    assert fact["evidence_id"] == "chk:1"
    assert fact["path"]
    assert fact["value"] in {1, "failing"}


def test_legacy_report_without_fact_fields_still_passes():
    manager = _manager()
    legacy = {
        "summary": "probe failing",
        "observations": [{"id": "o1", "text": "check 1 failing", "evidence_ids": ["chk:1"]}],
        "hypotheses": [{
            "id": "h1", "text": "process died", "layer": "app_internal",
            "status": "supported", "supporting_evidence_ids": ["chk:1"],
        }],
    }
    result = manager.verify_report(legacy, {"chk:1": {}}, [])
    assert result.passed is True
    assert result.invalid_fact_refs == []


def test_conclusion_basis_must_be_an_existing_hypothesis():
    manager = _manager()
    result = manager.verify_report(_report(conclusion_basis_hypothesis_id="missing"),
                                   {"chk:1": {}}, [])
    assert result.passed is False
    assert result.hypothesis_reference_errors == ["missing"]
