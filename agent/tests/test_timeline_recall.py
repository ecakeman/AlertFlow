"""P2：Memory Timeline 以 Go Runtime Timeline 为主轴，且有真实的窗口/类型/附件过滤。"""

from datetime import datetime, timezone

from alertflow_agent.memory_store import InMemoryMemoryRepo
from alertflow_agent.runtime.context_manager import ContextManager
from alertflow_agent.runtime.window import build_investigation_window, scope_from_event
from alertflow_agent.schemas.context import RetrievalRequest
from alertflow_agent.schemas.memory import TimelineAttachment, TimelineRecallRequest
from tests.fakes import FakeAlertFlow

EVENT = {
    "schema_version": "v1",
    "event_id": "incident_start:1",
    "event": "incident_start",
    "incident_id": 1,
    "service_id": "api",
    "service_name": "API",
    "status": "down",
    "occurred_at": "2026-10-11T10:00:00Z",
}


def _request(*sources) -> RetrievalRequest:
    window = build_investigation_window(EVENT, lookback_minutes=60, max_window_seconds=86400)
    scope = scope_from_event(EVENT, "run-1", window=window)
    return RetrievalRequest(request_id="req-1", scope=scope, purpose="targeted_followup",
                            source_requests=list(sources))


def _attachment(attachment_id: str = "att:run-0:diagnosis_report") -> TimelineAttachment:
    return TimelineAttachment(
        attachment_id=attachment_id, service_id="api",
        anchor_type="incident", anchor_id="1", attachment_type="diagnosis_report",
        source_system="agent_runtime", source_record_id="run-0", run_id="run-0",
        incident_id=1,
        occurred_at=datetime(2026, 10, 11, 9, 55, tzinfo=timezone.utc),
        recorded_at=datetime(2026, 10, 11, 9, 55, tzinfo=timezone.utc),
        summary="上次是进程崩溃", trust_level="validated_agent_report",
    )


def test_runtime_timeline_nodes_come_from_go_and_sort_desc():
    manager = ContextManager(alertflow=FakeAlertFlow())
    result = manager.retrieve_now(
        _request(TimelineRecallRequest(request_id="m1", service_id="api", top_k=10))
    ).results[0]
    assert result.status == "ok"
    assert [item.record_type for item in result.items] == ["timeline_node", "timeline_node"]
    # 事件时间倒序：incident.opened(09:59:30) 在 kubernetes.pod.changed(09:58) 之前
    assert result.items[0].data["node_type"] == "incident_transition"
    assert result.items[1].data["node_type"] == "kubernetes_change"
    assert result.items[1].data["source_system"] == "kubernetes"
    assert result.items[1].data["source_locator"]["event_type"] == "kubernetes.pod.changed"
    assert result.query_metadata["runtime_nodes"] == 2
    assert result.query_metadata["diagnosis_nodes"] == 0


def test_time_window_filters_nodes():
    manager = ContextManager(alertflow=FakeAlertFlow())
    result = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10,
        time_start=datetime(2026, 10, 11, 9, 56, tzinfo=timezone.utc),
        time_end=datetime(2026, 10, 11, 9, 58, 30, tzinfo=timezone.utc),
    ))).results[0]
    assert [item.data["source_locator"]["event_type"] for item in result.items] == ["kubernetes.pod.changed"]


def test_event_types_filter_is_applied_not_ignored():
    manager = ContextManager(alertflow=FakeAlertFlow())
    only_pods = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, event_types=["kubernetes.pod.changed"],
    ))).results[0]
    assert len(only_pods.items) == 1
    assert only_pods.items[0].data["source_locator"]["event_type"] == "kubernetes.pod.changed"
    none = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, event_types=["not_a_real_type"],
    ))).results[0]
    assert none.items == []
    assert none.status == "empty"


def test_include_attachments_toggle_is_real():
    repo = InMemoryMemoryRepo()
    repo.save_attachment(_attachment())
    manager = ContextManager(alertflow=FakeAlertFlow(), memory=repo)
    with_attachments = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, incident_id=1, include_attachments=True,
    ))).results[0]
    assert any(item.record_type == "memory_attachment" for item in with_attachments.items)
    without = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, incident_id=1, include_attachments=False,
    ))).results[0]
    assert all(item.record_type != "memory_attachment" for item in without.items)


def test_legacy_episode_is_a_diagnosis_node_no_longer_the_only_axis():
    repo = InMemoryMemoryRepo()
    repo.episodes.append({
        "id": "11111111-1111-1111-1111-111111111111", "service_id": "api",
        "incident_id": 1, "started_at": "2026-10-11T09:52:00+00:00", "card": "上次同样是 503",
    })
    manager = ContextManager(alertflow=FakeAlertFlow(), memory=repo)
    result = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10,
    ))).results[0]
    kinds = {item.data["node_type"] for item in result.items}
    assert {"agent_diagnosis", "kubernetes_change"} <= kinds
    legacy = [item for item in result.items if item.data["node_type"] == "agent_diagnosis"]
    assert legacy and legacy[0].data["source_system"] == "agent_memory"
    assert result.query_metadata["diagnosis_nodes"] == 1


def test_corrected_history_needs_flag_and_reads_real_rows():
    repo = InMemoryMemoryRepo()
    episode_id = "22222222-2222-2222-2222-222222222222"
    repo.episodes.append({"id": episode_id, "service_id": "api", "incident_id": 1,
                          "started_at": "2026-10-11T09:52:00+00:00", "card": "旧结论"})
    repo.record_correction(episode_id, "conclusion", {"root_cause": "db"}, {"root_cause": "pod"}, "人工纠正")
    manager = ContextManager(alertflow=FakeAlertFlow(), memory=repo)
    with_flag = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, include_corrected_history=True,
    ))).results[0]
    assert any(item.record_type == "memory_correction" for item in with_flag.items)
    without = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10, include_corrected_history=False,
    ))).results[0]
    assert all(item.record_type != "memory_correction" for item in without.items)


def test_runtime_timeline_failure_is_isolated():
    class BrokenTimeline(FakeAlertFlow):
        def timeline(self, *args, **kwargs):
            raise RuntimeError("go grpc down")

    manager = ContextManager(alertflow=BrokenTimeline())
    result = manager.retrieve_now(_request(TimelineRecallRequest(
        request_id="m1", service_id="api", top_k=10,
    ))).results[0]
    assert result.status == "empty"
    codes = [gap.code for gap in result.coverage_gaps]
    assert "SOURCE_UNAVAILABLE" in codes  # Go Runtime Timeline 失败被隔离为单来源不可用
    assert "NO_MATCHING_DATA" in codes
