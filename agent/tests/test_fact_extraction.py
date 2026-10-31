"""P3 契约：Fact Catalog 只收结构化字段，日志原文只作为 evidence 保留。

覆盖：
1. Loki 多行日志里纯文本行不产生事实，JSON 行的白名单字段（level/status/ts）产生事实；
2. FactCatalogEntry 的三个时间继承 payload 自身时间，缺失/非法才回退 collected_at；
3. 配额与顺序：每行最多 5 个键、整体最多 30 条、顺序确定；
4. ``build_catalog`` 与 ``EvidenceManager`` 用同一套提取规则；
5. 原始日志 evidence 仍可被报告按 evidence_id 引用，未登记 fact_id 仍被拒绝。
"""

from __future__ import annotations

from datetime import datetime, timezone

from alertflow_agent.runtime.evidence import EvidenceManager
from alertflow_agent.runtime.verify import build_catalog
from alertflow_agent.schemas.evidence import EvidenceCandidate
from tests.test_evidence_manager import _candidate, _manager, _scope

_LOKI_SELECTOR = {"ledger_key": "loki:1", "selector": '{app="api"}'}


def _loki(scope, window, payload, *, ledger="loki:1", schema="loki_rows"):
    return _candidate(
        scope, window, source_type="loki", source_system="loki_mcp",
        payload=payload, payload_schema=schema,
        source_locator={"ledger_key": ledger, "selector": _LOKI_SELECTOR["selector"]},
    )


def test_loki_plain_text_lines_never_become_facts():
    scope, window = _scope()
    manager = _manager(scope)
    payload = {
        "query": '{app="api"}',
        "lines": [
            "2026-10-11T09:58:00Z pool exhausted: dial tcp 10.0.0.7:5432",
            {"line": "another plain text line"},
            '{"ts":"2026-10-11T09:59:00Z","level":"error","status":503}',
        ],
    }
    result = manager.register_batch([_loki(scope, window, payload)])
    assert result.accepted_evidence_ids
    fact_by_path = {fact.path: fact.value for fact in manager.facts}

    # 结构化 JSON 行的白名单字段成为事实。
    assert fact_by_path["lines.2.level"] == "error"
    assert fact_by_path["lines.2.status"] == 503
    assert fact_by_path["lines.2.ts"] == "2026-10-11T09:59:00Z"

    # 纯文本行一条事实都没有，原文只留在 evidence payload 里。
    assert not any(path.startswith("lines.0.") for path in fact_by_path)
    assert not any(path.startswith("lines.1.") for path in fact_by_path)
    rendered = " ".join(str(value) for value in fact_by_path.values())
    assert "pool exhausted" not in rendered
    assert "another plain text line" not in rendered
    # 原文键（line/message/msg/log）永远不是事实路径。
    assert not any(path.rsplit(".", 1)[-1] in {"line", "message", "msg", "log"}
                   for path in fact_by_path)
    # 原始行仍完整保留在 evidence 里，模型可以通过 evidence_id 回溯。
    assert manager.records[result.accepted_evidence_ids[0]].payload["lines"] == payload["lines"]


def test_loki_count_and_query_are_facts_but_free_text_is_not():
    """MCP summarize_summary 形状：lines 是行数（int），value 是首行原文。"""
    scope, window = _scope()
    manager = _manager(scope)
    payload = {"query": '{job="api"}', "value": "checkout failed", "lines": 1}
    result = manager.register_batch([_loki(scope, window, payload)])
    assert result.accepted_evidence_ids
    fact_by_path = {fact.path: fact.value for fact in manager.facts}
    assert fact_by_path["query"] == '{job="api"}'
    assert fact_by_path["lines"] == 1
    assert "value" not in fact_by_path


def test_fact_times_inherit_payload_and_fall_back_to_collected_at():
    scope, window = _scope()
    manager = _manager(scope)
    candidate = _candidate(
        scope, window, source_type="timeline", source_system="alertflow_grpc",
        payload_schema="timeline_event",
        payload={
            "event_type": "PodRestart",
            "occurred_at": "2026-10-11T09:59:00Z",
            "observed_at": "2026-10-11T09:59:30Z",
        },
    )
    accepted = manager.register_batch([candidate]).accepted_evidence_ids[0]
    record = manager.records[accepted]
    facts = [fact for fact in manager.facts if fact.evidence_id == accepted]
    assert facts
    for fact in facts:
        assert fact.occurred_at == datetime(2026, 10, 11, 9, 59, tzinfo=timezone.utc)
        assert fact.observed_at == datetime(2026, 10, 11, 9, 59, 30, tzinfo=timezone.utc)
        # payload 没有 recorded_at，回退到记录采集时间。
        assert fact.recorded_at == record.collected_at


def test_unparseable_payload_time_falls_back_without_raising():
    scope, window = _scope()
    manager = _manager(scope)
    candidate = _candidate(
        scope, window, source_type="timeline", source_system="alertflow_grpc",
        payload_schema="timeline_event",
        payload={"event_type": "PodRestart", "occurred_at": "not-a-timestamp"},
    )
    accepted = manager.register_batch([candidate]).accepted_evidence_ids[0]
    record = manager.records[accepted]
    facts = [fact for fact in manager.facts if fact.evidence_id == accepted]
    assert facts
    for fact in facts:
        assert fact.occurred_at == record.collected_at
        assert fact.observed_at == record.collected_at
        assert fact.recorded_at == record.collected_at


def test_build_catalog_and_registry_share_extraction_rules():
    """legacy 报告路径（build_catalog）与注册表路径必须一字不差。"""
    checks_payload = {
        "id": 1, "status": "failing", "latency_ms": 1200,
        "note": "free text should not become a fact",
        "detail": {"pool_depth": 4},
    }
    loki_payload = {
        "query": '{app="api"}',
        "lines": ["plain text line", '{"level":"error","status":503}'],
    }
    scope, window = _scope()
    manager = _manager(scope)
    manager.register_batch([
        _candidate(scope, window, payload=checks_payload),
        _loki(scope, window, loki_payload),
    ])
    registry = {(fact.path, fact.value) for fact in manager.facts}

    legacy = build_catalog({
        "chk:1": {"kind": "http_check", "source_type": "checks", "data": checks_payload},
        "loki:1": {"kind": "log_query", "source_type": "loki", "data": loki_payload},
    })
    catalog = {(entry["path"], entry["value"]) for entry in legacy}

    assert registry == catalog
    assert ("note", "free text should not become a fact") not in registry
    assert ("value", "free text should not become a fact") not in registry


def test_per_line_and_total_caps_are_deterministic():
    scope, window = _scope()
    lines = [
        {
            "ts": index, "level": "error", "status": 500, "code": 7,
            "method": "GET", "path": "/api/checkout", "pod": f"api-{index}",
            "namespace": "default",
        }
        for index in range(40)
    ]
    first = _manager(scope).register_batch([_loki(scope, window, {"query": "q", "lines": lines})])
    assert first.accepted_evidence_ids

    def catalog_paths():
        manager = _manager(scope)
        manager.register_batch([_loki(scope, window, {"query": "q", "lines": lines})])
        return [fact.path for fact in manager.facts if fact.path.startswith("lines.")]

    paths = catalog_paths()
    # 每条日志行按白名单顺序取前 5 个键：ts/level/status/code/method。
    # manager.facts 全局按 path 字典序排序，因此这里用集合断言内容。
    expected = {f"lines.{index}.{key}"
                for index in range(6)
                for key in ("ts", "level", "status", "code", "method")}
    assert set(paths) == expected
    assert "lines.0.pod" not in paths
    assert len(paths) == 30
    # 同样的输入两次得到同样的顺序。
    assert catalog_paths() == paths


def test_plain_log_evidence_is_still_citable_by_evidence_id():
    scope, window = _scope()
    manager = _manager(scope)
    loki = _loki(scope, window, {"query": "q", "lines": ["boom: pool exhausted"]})
    accepted = manager.register_batch([loki]).accepted_evidence_ids[0]
    draft = {
        "summary": "pool exhausted",
        "observations": [{"id": "o1", "text": "log line reports pool exhausted",
                          "evidence_ids": [accepted]}],
        "hypotheses": [{
            "id": "h1", "text": "connection pool exhausted", "layer": "app_internal",
            "status": "supported", "supporting_evidence_ids": [accepted],
        }],
        "unknowns": [],
        "recommended_next_steps": [],
    }
    result = manager.verify_report(draft, {accepted: {}}, [])
    assert result.passed is True
    assert result.unknown_evidence_ids == []


def test_unknown_fact_and_evidence_ids_still_fail_verification():
    scope, window = _scope()
    manager = _manager(scope)
    accepted = manager.register_batch([_loki(scope, window, {"query": "q", "lines": []})])
    assert accepted.accepted_evidence_ids
    draft = {
        "summary": "x",
        "observations": [{"id": "o1", "text": "x", "evidence_ids": ["missing:1"]}],
        "hypotheses": [{
            "id": "h1", "text": "x", "layer": "app_internal", "status": "supported",
            "supporting_fact_ids": ["f404"],
        }],
        "unknowns": [],
        "recommended_next_steps": [],
    }
    result = manager.verify_report(draft, {}, [])
    assert result.passed is False
    assert "f404" in result.invalid_fact_refs
    assert "missing:1" in result.unknown_evidence_ids


def test_diagnostic_and_kubernetes_use_explicit_fields_only():
    scope, window = _scope()
    manager = _manager(scope)
    manager.register_batch([
        _candidate(scope, window, candidate_id="diag", source_type="diagnostic",
                   source_system="alertflow_grpc", payload_schema="project_diagnostic",
                   source_locator={"ledger_key": "diag:1"},
                   payload={"name": "pool", "status": "ok",
                            "detail": {"pool_depth": 4, "free_note": "ignore me"},
                            "attributes": {"region": "cn-north"},
                            "random_key": "not whitelisted"}),
        _candidate(scope, window, candidate_id="k8s", source_type="kubernetes",
                   source_system="alertflow_grpc", payload_schema="k8s_object",
                   source_locator={"ledger_key": "k8s:1"},
                   payload={"kind": "Pod", "name": "api-1", "active": True, "restarts": 3,
                            "labels": {"app": "api"}, "unlisted": "ignore me"}),
    ])
    paths = {fact.path for fact in manager.facts}
    assert {"detail.pool_depth", "attributes.region", "kind", "active", "restarts",
            "labels.app"} <= paths
    # 顶层未白名单的键一律不提取；白名单容器（detail/attributes/labels）按有界规则展开。
    assert not any(path.startswith(("random_key", "unlisted")) for path in paths)
    assert "detail.pool_depth" in paths
