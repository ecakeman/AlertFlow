"""C1 契约测试：唯一时间窗 + scope/retrieval/evidence schema。

这些测试先于实现存在（失败测试），覆盖规格 §7.7.3 的时间规则与 §7.7.14 第 4/6 条：
incident_start / incident_resolve / 缺 resolved_at / window cap / 跨 Service / 越窗。
"""

from datetime import datetime, timedelta, timezone

import pytest
from pydantic import ValidationError

from alertflow_agent.runtime.window import (
    build_investigation_window,
    scope_from_event,
    window_from_context,
    window_from_state,
)
from alertflow_agent.schemas.context import (
    AlertFlowQueryRequest,
    InvestigationScope,
    InvestigationWindow,
    LokiQueryRequest,
    PrometheusQueryRequest,
    RetrievalRequest,
)
from alertflow_agent.schemas.evidence import EvidenceCandidate, FactCatalogEntry
from alertflow_agent.schemas.memory import TimelineAttachment, TimelineRecallRequest

UTC = timezone.utc


def _dt(text: str) -> datetime:
    return datetime.fromisoformat(text.replace("Z", "+00:00"))


def test_incident_start_window_uses_occurred_at_as_upper_bound():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"},
        lookback_minutes=60,
        max_window_seconds=3600,
    )
    assert window.window_kind == "root_cause"
    assert window.anchor_field == "occurred_at"
    assert window.window_end == _dt("2026-10-11T10:00:00Z")
    assert window.window_start == _dt("2026-10-11T09:00:00Z")
    assert window.lookback_seconds == 3600
    assert window.time_accuracy == "exact"
    assert window.gap_codes == []


def test_incident_resolve_uses_resolved_at_when_present():
    window = build_investigation_window(
        {
            "event": "incident_resolve",
            "occurred_at": "2026-10-11T10:00:00Z",
            "resolved_at": "2026-10-11T10:20:00Z",
        },
        lookback_minutes=60,
        max_window_seconds=3600,
    )
    assert window.window_end == _dt("2026-10-11T10:20:00Z")
    assert window.anchor_field == "resolved_at"
    assert window.window_start == _dt("2026-10-11T09:20:00Z")


def test_incident_resolve_without_resolved_at_falls_back_and_marks_gap():
    window = build_investigation_window(
        {"event": "incident_resolve", "occurred_at": "2026-10-11T10:00:00Z"},
        lookback_minutes=60,
        max_window_seconds=3600,
    )
    assert window.window_end == _dt("2026-10-11T10:00:00Z")
    assert window.anchor_field == "occurred_at"
    assert "RESOLVE_TIME_MISSING" in window.gap_codes
    assert window.time_accuracy == "derived"


def test_verified_started_at_shrinks_the_window():
    window = build_investigation_window(
        {
            "event": "incident_start",
            "occurred_at": "2026-10-11T10:00:00Z",
            "started_at": "2026-10-11T09:30:00Z",
        },
        lookback_minutes=60,
        max_window_seconds=3600,
    )
    assert window.window_start == _dt("2026-10-11T09:30:00Z")
    assert window.lookback_seconds == 1800


def test_lookback_is_capped_by_mcp_window_limit():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"},
        lookback_minutes=24 * 60,
        max_window_seconds=3600,
    )
    assert window.lookback_seconds == 3600
    assert window.window_start == _dt("2026-10-11T09:00:00Z")


def test_missing_occurred_at_is_unknown_not_guessed():
    now = _dt("2026-10-11T12:00:00Z")
    window = build_investigation_window({"event": "incident_start"}, now=now)
    assert window.window_end == now
    assert window.time_accuracy == "unknown"
    assert "OCCURRED_AT_MISSING" in window.gap_codes


def test_post_event_window_kind_is_preserved():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"},
        window_kind="post_event_observation",
    )
    assert window.window_kind == "post_event_observation"


def test_window_is_consumed_from_state_and_context():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"}
    )
    state = {"window": window.model_dump(mode="json")}
    assert window_from_state(state).window_end == window.window_end

    context_window = window_from_context(
        {"timeWindow": {"from": "2026-10-10T10:00:00Z", "to": "2026-10-11T10:00:00Z"}}
    )
    assert context_window is not None
    assert context_window.start_epoch() == int(_dt("2026-10-10T10:00:00Z").timestamp())


def test_scope_records_trigger_and_times():
    scope = scope_from_event(
        {
            "event": "incident_resolve",
            "service_id": "api",
            "incident_id": 7,
            "occurred_at": "2026-10-11T10:00:00Z",
            "resolved_at": "2026-10-11T10:20:00Z",
        },
        "run-1",
    )
    assert scope.trigger_type == "incident_resolve"
    assert scope.incident_id == 7
    assert scope.window.window_end == _dt("2026-10-11T10:20:00Z")


def _request(
    window: InvestigationWindow,
    service_id: str = "api",
    scope_window: InvestigationWindow | None = None,
    **overrides,
) -> RetrievalRequest:
    alertflow = AlertFlowQueryRequest(
        request_id="r-alertflow",
        run_id="run-1",
        service_id=service_id,
        resources=["checks", "service_timeline"],
        window_start=window.window_start,
        window_end=window.window_end,
    )
    prom = PrometheusQueryRequest(
        request_id="r-prom",
        service_id=service_id,
        selector={"service": service_id},
        promql="up",
        start=window.window_start,
        end=window.window_end,
    )
    loki = LokiQueryRequest(
        request_id="r-loki",
        service_id=service_id,
        selector={"service": service_id},
        logql="{service=\"api\"}",
        start=window.window_start,
        end=window.window_end,
    )
    scope = InvestigationScope(
        run_id="run-1",
        service_id="api",
        trigger_type="incident_start",
        occurred_at=window.window_end,
        window=scope_window or window,
    )
    return RetrievalRequest(
        request_id="req-1",
        scope=scope,
        source_requests=[alertflow, prom, loki],
        **overrides,
    )


def test_retrieval_request_accepts_union_of_constrained_sources():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"}
    )
    request = _request(window)
    assert [type(item).__name__ for item in request.source_requests] == [
        "AlertFlowQueryRequest",
        "PrometheusQueryRequest",
        "LokiQueryRequest",
    ]


def test_retrieval_request_rejects_adapter_derived_window():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"}
    )
    shifted = InvestigationWindow(
        window_start=window.window_start - timedelta(minutes=5),
        window_end=window.window_end,
        lookback_seconds=3900,
    )
    # 父级窗口没变，adapter 自己把请求窗口改宽了 -> 必须拒绝
    with pytest.raises(ValidationError, match="WINDOW_MISMATCH"):
        _request(shifted, scope_window=window)


def test_retrieval_request_rejects_cross_service_query():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"}
    )
    with pytest.raises(ValidationError, match="SERVICE_SCOPE_MISMATCH"):
        _request(window, service_id="other")


def test_retrieval_request_rejects_naive_datetimes():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"}
    )
    scope = InvestigationScope(
        run_id="run-1",
        service_id="api",
        occurred_at=window.window_end,
        window=window,
    )
    with pytest.raises(ValidationError):
        AlertFlowQueryRequest(
            request_id="r",
            run_id="run-1",
            service_id="api",
            resources=["checks"],
            window_start=datetime(2026, 10, 11, 9, 0),
            window_end=datetime(2026, 10, 11, 10, 0),
        )
    assert scope.window.window_kind == "root_cause"


def test_evidence_candidate_rejects_data_outside_window():
    window = build_investigation_window(
        {"event": "incident_start", "occurred_at": "2026-10-11T10:00:00Z"},
        lookback_minutes=60,
    )
    with pytest.raises(ValidationError):
        EvidenceCandidate(
            candidate_id="c1",
            run_id="run-1",
            service_id="api",
            source_type="checks",
            source_system="alertflow_grpc",
            window=window,
            observed_range={
                "start": "2026-10-11T08:00:00Z",
                "end": "2026-10-11T08:10:00Z",
            },
            payload={"rows": []},
        )


def test_fact_value_must_be_scalar():
    with pytest.raises(ValidationError):
        FactCatalogEntry(
            fact_id="f1",
            evidence_id="ev-1",
            path="status",
            value={"nested": 1},
            value_type="string",
            service_id="api",
        )


def test_timeline_attachment_unique_key_is_stable_and_recall_request_is_constrained():
    attachment = TimelineAttachment(
        attachment_id="att-1",
        service_id="api",
        anchor_type="incident",
        anchor_id="inc-1",
        attachment_type="runbook_advice",
        source_system="ragflow_a2a",
        source_record_id="runbook:run-1",
        summary="restart",
    )
    assert attachment.unique_key() == (
        "incident",
        "inc-1",
        "runbook_advice",
        "ragflow_a2a",
        "runbook:run-1",
    )
    recall = TimelineRecallRequest(request_id="r", service_id="api", top_k=5)
    assert recall.source_type == "memory_reference"
