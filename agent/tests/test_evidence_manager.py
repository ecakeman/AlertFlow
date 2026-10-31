"""C2 EvidenceManager 契约测试：登记校验 / 稳定 ID / 幂等 / 提取器 / 确定性校验。"""

from __future__ import annotations

import pytest

from alertflow_agent.runtime.evidence import (
    EvidenceManager,
    candidates_from_evidence,
    make_evidence_id,
)
from alertflow_agent.runtime.window import build_investigation_window, scope_from_event
from alertflow_agent.schemas.evidence import EvidenceCandidate


def _scope(**kwargs):
    event = {"service_id": "api", "incident_id": 7, "occurred_at": "2026-10-11T10:00:00Z",
             **kwargs.pop("event", {})}
    window = build_investigation_window(event, lookback_minutes=60, max_window_seconds=3600)
    return scope_from_event(event, "run-1", window=window), window


def _candidate(scope, window, **kwargs):
    base = dict(
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
    )
    base.update(kwargs)
    return EvidenceCandidate(**base)


def _manager(scope):
    return EvidenceManager(scope.run_id, scope)


def test_six_validations_reject_only_the_bad_candidate():
    scope, window = _scope()
    manager = _manager(scope)
    good = _candidate(scope, window)
    bad_service = _candidate(scope, window, candidate_id="bad-service", service_id="other")
    bad_incident = _candidate(scope, window, candidate_id="bad-incident", incident_id=99)
    other_window = build_investigation_window(
        {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"},
        lookback_minutes=30, max_window_seconds=3600,
    )
    bad_window = _candidate(scope, other_window, candidate_id="bad-window")
    bad_selector = _candidate(scope, window, candidate_id="bad-selector",
                              source_type="prometheus", source_system="prometheus_mcp")
    bad_rows = _candidate(scope, window, candidate_id="bad-rows",
                          payload={"rows": list(range(600))})
    bad_bytes = _candidate(scope, window, candidate_id="bad-bytes",
                           payload={"text": "x" * 70000})
    bad_schema = _candidate(scope, window, candidate_id="bad-schema", payload_schema="")
    bad_empty = _candidate(scope, window, candidate_id="bad-empty", payload={})

    result = manager.register_batch([
        good, bad_service, bad_incident, bad_window, bad_selector,
        bad_rows, bad_bytes, bad_schema, bad_empty,
    ])
    assert len(result.accepted_evidence_ids) == 1
    reasons = {item.reason_code for item in result.rejected_candidates}
    assert reasons == {
        "SERVICE_MISMATCH", "INCIDENT_MISMATCH", "WINDOW_MISMATCH",
        "SELECTOR_MISSING", "ROWS_EXCEEDED", "BYTES_EXCEEDED",
        "SCHEMA_INVALID", "PAYLOAD_EMPTY",
    }
    # 合法候选照常入库并派生事实
    assert manager.facts
    assert result.fact_count_by_source_type == {"checks": 1}


def test_evidence_id_is_stable_idempotent_and_run_scoped():
    scope, window = _scope()
    candidate = _candidate(scope, window)
    first = _manager(scope).register_batch([candidate])
    second = _manager(scope).register_batch([candidate])
    assert first.accepted_evidence_ids == second.accepted_evidence_ids
    assert second.duplicate_evidence_ids == []
    # 同一 manager 内重复登记 -> duplicate
    manager = _manager(scope)
    result = manager.register_batch([candidate, candidate])
    assert len(result.accepted_evidence_ids) == 1
    assert result.duplicate_evidence_ids == result.accepted_evidence_ids
    # 不同 run -> 不同身份
    other_scope = scope_from_event({"service_id": "api", "incident_id": 7,
                                    "occurred_at": "2026-10-11T10:00:00Z"},
                                   "run-2", window=window)
    other_id = make_evidence_id(other_scope.run_id, "checks", candidate.source_locator,
                                window, candidate.content_digest)
    assert other_id != first.accepted_evidence_ids[0]


def test_candidate_cannot_report_its_own_evidence_id():
    scope, window = _scope()
    with pytest.raises(ValueError):
        EvidenceCandidate(**_candidate(scope, window).model_dump(), evidence_id="ev:forged")


@pytest.mark.parametrize("source_type,source_system,payload,path", [
    ("checks", "alertflow_grpc", {"status": "failing", "latency_ms": 1200}, "latency_ms"),
    ("diagnostic", "alertflow_grpc", {"status": "ok", "name": "pool", "detail": {"pool_depth": 4}}, "detail.pool_depth"),
    ("timeline", "alertflow_grpc", {"event_type": "PodRestart", "summary": "restarted"}, "event_type"),
    ("kubernetes", "alertflow_grpc", {"kind": "Pod", "name": "api-1", "active": True}, "active"),
    ("go_metric", "alertflow_grpc", {"stats": {"p95": 900}}, "stats.p95"),
    ("prometheus", "prometheus_mcp", {"query": "up", "value": "0"}, "value"),
    ("loki", "loki_mcp", {"query": "{app=\"api\"}", "lines": [{"ts": 1, "line": "boom"}]}, "lines.0.ts"),
    ("runbook", "ragflow_a2a", {"schema": "runbook-research.v1", "content": "restart",
                                "citations_available": False, "source": "ragflow"}, "content"),
])
def test_catalog_extractors_per_source(source_type, source_system, payload, path):
    scope, window = _scope()
    manager = _manager(scope)
    candidate = _candidate(
        scope, window, source_type=source_type, source_system=source_system,
        payload=payload, payload_schema="1",
        source_locator={"ledger_key": "x:1", "selector": "job=\"api\""},
    )
    result = manager.register_batch([candidate])
    assert result.accepted_evidence_ids
    paths = {fact.path for fact in manager.facts}
    assert path in paths


def test_verify_report_maps_findings_and_citation_ids():
    scope, window = _scope()
    manager = _manager(scope)
    manager.register_batch([
        _candidate(scope, window, payload={"status": "failing"}, payload_schema="http_check"),
        _candidate(scope, window, candidate_id="rb", source_type="runbook",
                   source_system="ragflow_a2a", payload_schema="runbook-research.v1",
                   source_locator={"ledger_key": "runbook:run-1"},
                   payload={"schema": "runbook-research.v1", "content": "restart",
                            "citations_available": True,
                            "citations": [{"document_id": "doc-1", "id": "chunk-1"}],
                            "source": "ragflow"}),
    ])
    draft = {
        "summary": "s",
        "observations": [{"id": "o1", "text": "t", "fact_ids": ["no-such"],
                          "evidence_ids": ["nope"]}],
        "hypotheses": [{"id": "h1", "text": "t", "layer": "probe", "status": "supported",
                        "supporting_evidence_ids": [], "weakening_evidence_ids": []}],
        "unknowns": [],
        "recommended_next_steps": [],
    }
    result = manager.verify_report(draft, {"x": {"id": "x", "source_type": "analysis"}})
    assert result.passed is False
    assert result.invalid_fact_refs == ["no-such"]
    assert result.unknown_evidence_ids == ["nope"]
    assert result.hypothesis_reference_errors == ["h1"]
    # runbook 引用缺 chunk_id -> 不可追溯
    assert result.invalid_document_citations


def test_legacy_projection_keeps_ledger_keys_and_rejects_unknown_sources():
    scope, window = _scope()
    state = {"evidence": {
        "chk:1": {"id": "chk:1", "source_type": "alertflow_check", "source_ref": "check:1",
                  "service_id": "api", "kind": "http_check", "data": {"status": "failing"}},
        "mystery": {"id": "mystery", "source_type": "unknown_source", "service_id": "api",
                    "data": {"x": 1}},
    }}
    candidates, rejected = candidates_from_evidence(state, scope, window)
    assert candidates[0].source_locator["ledger_key"] == "chk:1"
    assert candidates[0].source_type == "checks"
    assert rejected[0].reason_code == "INVALID_SOURCE"


def test_graph_run_registers_evidence_and_reports_summary():
    """集成：一次基线 run 的登记摘要写进报告，catalog 仍用旧 ledger id 供模型引用。"""
    from alertflow_agent.config import Settings
    from alertflow_agent.db import MemoryStore
    from alertflow_agent.runtime.graph import Runner
    from alertflow_agent.runtime.llm import ScriptedModel
    from tests.fakes import FakeAlertFlow, event_row

    store = MemoryStore()
    row = event_row("baseline")
    store.insert_run(row)
    Runner(Settings(max_revisions=2), store, FakeAlertFlow(), ScriptedModel()).execute(row["id"])
    report = store.latest_report(row["id"])["report"]
    registration = report["evidence_registration"]
    assert registration["accepted"] > 0
    assert registration["rejected"] == 0
    assert report["verification"]["passed"] is True
    assert report["observations"][0]["facts"]
