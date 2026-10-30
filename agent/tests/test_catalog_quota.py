"""Fact Catalog 截断按来源配额，且 tl:coverage 这类时间线事件不再被当成未知来源。"""

from alertflow_agent.runtime.evidence import EvidenceManager, candidates_from_evidence
from alertflow_agent.runtime.window import build_investigation_window, scope_from_event
from alertflow_agent.schemas.evidence import EvidenceCandidate


def _scope():
    event = {"service_id": "api", "incident_id": 7, "occurred_at": "2026-10-11T10:00:00Z"}
    window = build_investigation_window(event, lookback_minutes=60, max_window_seconds=3600)
    return scope_from_event(event, "run-quota", window=window), window


def _candidate(scope, window, index, *, source_type, source_system, payload):
    return EvidenceCandidate(
        candidate_id=f"c{index}", run_id=scope.run_id, service_id=scope.service_id,
        incident_id=scope.incident_id, source_type=source_type, source_system=source_system,
        source_locator={"ledger_key": f"{source_type}:{index}",
                        "selector": {"service": "api"}}, window=window,
        payload=payload, payload_schema="http_check", content_digest="",
    )


def test_truncation_keeps_a_floor_for_every_source():
    scope, window = _scope()
    manager = EvidenceManager(scope.run_id, scope, max_facts=9)
    candidates = []
    for index in range(12):
        candidates.append(_candidate(scope, window, index, source_type="checks",
                                     source_system="alertflow_grpc",
                                     payload={"id": index, "status": "failing",
                                              "extra": {f"k{n}": n for n in range(5)}}))
    for index in range(12):
        candidates.append(_candidate(scope, window, 100 + index, source_type="prometheus",
                                     source_system="prometheus_mcp",
                                     payload={"value": index, "start": index, "end": index + 1,
                                              "samples": [{"ts": index, "value": index}]}))
    for index in range(12):
        candidates.append(_candidate(scope, window, 200 + index, source_type="loki",
                                     source_system="loki_mcp",
                                     payload={"line": f"boom {index}", "timestamp": index,
                                              "lines": [{"ts": index, "line": f"boom {index}"}]}))
    result = manager.register_batch(candidates)
    assert result.catalog_truncated is True
    kept = result.facts_kept_by_source_type
    # 三个来源都至少保留 1 条 fact：没有任何来源被“先到先得”整类挤掉
    assert set(kept) == {"checks", "prometheus", "loki"}
    assert all(count >= 1 for count in kept.values())
    assert sum(kept.values()) == 9
    assert set(result.catalog_truncated_by_source) == {"checks", "prometheus", "loki"}
    assert any(item.code == "RESULT_TRUNCATED" for item in result.coverage_gaps)
    # fact_id 从 f1 连续编号，且每条都带 evidence_id
    assert [fact.fact_id for fact in manager.facts] == [f"f{i}" for i in range(1, 10)]
    assert all(fact.evidence_id for fact in manager.facts)


def test_timeline_coverage_event_is_registered_not_rejected():
    scope, window = _scope()
    state = {
        "evidence": {
            "tl:coverage": {
                "source_type": "timeline",
                "source_ref": "tl:coverage",
                "kind": "coverage",
                "summary": "timeline source coverage",
                "data": {"source": "application_diagnostics", "status": "ok", "from": 1, "to": 60},
            }
        }
    }
    candidates, rejected = candidates_from_evidence(state, scope, window)
    assert rejected == []
    result = EvidenceManager(scope.run_id, scope).register_batch(candidates)
    assert len(result.accepted_evidence_ids) == 1
    assert result.rejected_candidates == []
    assert result.fact_count_by_source_type == {"timeline": 1}


def test_quota_never_exceeds_the_fact_limit_when_sources_outnumber_it():
    """来源数多于名额时也不能超过 max_facts：按来源优先级只保留前几个来源各 1 条。"""
    scope, window = _scope()
    manager = EvidenceManager(scope.run_id, scope, max_facts=2)
    candidates = []
    for index in range(5):
        candidates.append(_candidate(scope, window, index, source_type="checks",
                                     source_system="alertflow_grpc",
                                     payload={"id": index}))
    for index in range(5):
        candidates.append(_candidate(scope, window, 100 + index, source_type="prometheus",
                                     source_system="prometheus_mcp",
                                     payload={"value": index, "selector": {"service": "api"}}))
    result = manager.register_batch(candidates)
    assert result.catalog_truncated is True
    assert len(manager.facts) == 2
    assert sum(result.facts_kept_by_source_type.values()) == 2
