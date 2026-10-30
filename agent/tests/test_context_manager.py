"""C4：ContextManager 唯一检索入口 + 六区块 PromptContextBundle。"""

from datetime import datetime, timezone

from alertflow_agent.config import Settings
from alertflow_agent.memory_store import InMemoryMemoryRepo
from alertflow_agent.runtime.context import build_blocks, build_prompt_bundle
from alertflow_agent.runtime.context_manager import ContextManager
from alertflow_agent.runtime.window import build_investigation_window, scope_from_event
from alertflow_agent.schemas.context import (
    BLOCK_ORDER,
    AlertFlowQueryRequest,
    LokiQueryRequest,
    PrometheusQueryRequest,
    RetrievalRequest,
)
from alertflow_agent.schemas.memory import SourceRef, TimelineAttachment, TimelineRecallRequest
from tests.fakes import FakeAlertFlow

EVENT = {"event": "incident_start", "service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"}


def _request(*sources):
    window = build_investigation_window(EVENT, lookback_minutes=60, max_window_seconds=86400)
    scope = scope_from_event(EVENT, "run-1", window=window)
    return RetrievalRequest(request_id="req-1", scope=scope, purpose="initial_context",
                            source_requests=list(sources))


def _window():
    window = build_investigation_window(EVENT, lookback_minutes=60, max_window_seconds=86400)
    return window.window_start, window.window_end


def test_alertflow_checks_are_normalized():
    start, end = _window()
    request = _request(AlertFlowQueryRequest(request_id="r1", run_id="run-1", service_id="api",
                                             resources=["checks"], window_start=start, window_end=end,
                                             page_size=50))
    batch = ContextManager(alertflow=FakeAlertFlow(), settings=Settings()).retrieve_now(request)
    result = batch.results[0]
    assert result.source_type == "checks" and result.source_system == "alertflow_grpc"
    assert result.status in {"ok", "truncated"}
    record = result.items[0]
    assert record.record_type == "check"
    assert record.data["ts"] and "status" in record.data
    assert record.content_digest


def test_missing_alertflow_client_is_disabled():
    start, end = _window()
    request = _request(AlertFlowQueryRequest(request_id="r1", run_id="run-1", service_id="api",
                                             resources=["checks"], window_start=start, window_end=end))
    batch = ContextManager().retrieve_now(request)
    assert batch.results[0].status == "disabled"
    assert batch.results[0].coverage_gaps[0].code == "SOURCE_DISABLED"


class FakeMCP:
    def __init__(self, payload):
        self.payload = payload
        self.calls = []

    def call(self, tool, sent):
        self.calls.append((tool, sent))
        return self.payload


def test_prometheus_adapter_uses_the_scope_window():
    start, end = _window()
    mcp = FakeMCP('[{"metric": {}, "value": [1791712800, "1"]}]')
    request = _request(PrometheusQueryRequest(request_id="p1", service_id="api",
                                              selector={"service": "api"}, query_mode="range",
                                              promql='up{service="api"}', start=start, end=end,
                                              step_seconds=60))
    result = ContextManager(mcp=mcp).retrieve_now(request).results[0]
    assert mcp.calls[0][0] == "range_query"
    assert mcp.calls[0][1]["start_time"] == start.isoformat().replace("+00:00", "Z")
    assert mcp.calls[0][1]["end_time"] == end.isoformat().replace("+00:00", "Z")
    assert result.items[0].record_type == "prometheus_sample"


def test_loki_empty_is_empty_not_failed():
    start, end = _window()
    mcp = FakeMCP("[]")
    request = _request(LokiQueryRequest(request_id="l1", service_id="api",
                                        selector={"service": "api"}, logql='{service="api"}',
                                        start=start, end=end, limit=10))
    result = ContextManager(mcp=mcp).retrieve_now(request).results[0]
    assert result.status == "empty"
    assert result.coverage_gaps[0].code == "NO_MATCHING_DATA"


def test_one_source_failure_does_not_cancel_another():
    start, end = _window()

    class Broken:
        def checks(self, *args, **kwargs):
            raise RuntimeError("grpc down")

    request = _request(
        AlertFlowQueryRequest(request_id="r1", run_id="run-1", service_id="api",
                              resources=["checks"], window_start=start, window_end=end),
        PrometheusQueryRequest(request_id="p1", service_id="api", selector={"service": "api"},
                               query_mode="instant", promql="up", start=start, end=end),
    )
    batch = ContextManager(alertflow=Broken(), mcp=FakeMCP('[{"value": [1791712800, "1"]}]')).retrieve_now(request)
    statuses = {item.source_type: item.status for item in batch.results}
    assert statuses["checks"] == "failed"
    assert statuses["prometheus"] == "ok"
    assert batch.overall_coverage == "gap"


def test_memory_recall_reads_timeline_attachments():
    repo = InMemoryMemoryRepo()
    repo.save_attachment(TimelineAttachment(
        attachment_id="att:run-0:diagnosis_report", service_id="api",
        anchor_type="incident", anchor_id="7", attachment_type="diagnosis_report",
        source_system="agent_runtime", source_record_id="run-0", run_id="run-0",
        occurred_at=datetime(2026, 10, 10, tzinfo=timezone.utc),
        recorded_at=datetime(2026, 10, 10, tzinfo=timezone.utc),
        summary="上次是进程崩溃", trust_level="validated_agent_report",
        source_refs=[SourceRef(source_type="analysis", source_record_id="ana:status")],
        content_digest="digest",
    ))
    request = _request(TimelineRecallRequest(request_id="m1", service_id="api", top_k=5,
                                             include_attachments=True))
    result = ContextManager(memory=repo).retrieve_now(request).results[0]
    assert result.source_type == "memory_reference"
    assert result.items and result.items[0].record_type == "memory_attachment"
    assert result.items[0].summary == "上次是进程崩溃"


def test_batch_order_and_no_evidence_ids():
    start, end = _window()
    request = _request(
        LokiQueryRequest(request_id="l1", service_id="api", selector={"service": "api"},
                         logql='{service="api"}', start=start, end=end, limit=5),
        AlertFlowQueryRequest(request_id="r1", run_id="run-1", service_id="api",
                              resources=["checks"], window_start=start, window_end=end),
    )
    batch = ContextManager(alertflow=FakeAlertFlow(), mcp=FakeMCP("[]")).retrieve_now(request)
    assert [item.source_type for item in batch.results] == ["checks", "loki"]
    assert not hasattr(batch, "evidence_ids")
    assert not hasattr(batch.results[0].items[0], "evidence_id")


def _bundle_state():
    window = build_investigation_window(EVENT, lookback_minutes=60, max_window_seconds=86400)
    return {
        "run_id": "run-1",
        "event": {"event": "incident_start", "service_id": "api", "status": "ongoing",
                  "severity": "outage", "occurred_at": "2026-10-11T10:00:00Z"},
        "window": window.model_dump(mode="json"),
        "evidence": {
            "chk:1": {"source_type": "alertflow_check", "source_ref": "check:1", "kind": "http_check",
                      "summary": "503", "data": {"status": "offline"}},
            "ana:status@0": {"source_type": "analysis", "source_ref": "analysis", "kind": "status",
                             "summary": "offline", "data": {"counts": {"total": 1}}},
        },
        "raw_checks": [{"id": 1, "ts": "2026-10-11T09:59:00Z", "status": "offline", "latency_ms": 15,
                        "status_code": 503, "error": "Service Unavailable", "attempts": 1}],
        "messages": [],
        "skills": ["intermittent-failure"],
        "skills_loaded": ["intermittent-failure"],
        "skill_texts": {"intermittent-failure": "check flapping"},
        "prior": {"hits": [{"id": "e1", "conclusion": {"text": "crash"}, "started_at": "2026-10-01T00:00:00Z"}]},
        "prior_block": "既往上下文（来自长期记忆）: crash",
        "plan": {"steps": []},
    }


def test_prompt_bundle_has_fixed_six_block_order():
    bundle = build_prompt_bundle("lead", _bundle_state())
    names = [block.name for block in bundle.blocks]
    assert names == [name for name in BLOCK_ORDER if name in names]
    assert "current_task" in names and "current_evidence" in names
    assert bundle.blocks[0].name == "current_task"
    assert bundle.scope.service_id == "api"
    assert bundle.rendered_prompt_sections.startswith("## current_task")
    assert "## current_evidence" in bundle.rendered_prompt_sections
    assert "chk:1" in bundle.rendered_prompt_sections
    assert bundle.estimated_input_tokens > 0
    required = {block.name: block.required for block in bundle.blocks}
    assert required["current_task"] and required["current_evidence"]


def test_prompt_bundle_groups_memory_and_expert_sections():
    state = _bundle_state()
    bundle = build_prompt_bundle("lead", state)
    text = bundle.rendered_prompt_sections
    assert "## historical_experience" in text
    assert "## expert_knowledge" in text
    assert "## service_timeline" not in text  # 没有时间线证据时该区块为空，不硬造
    # 叶子块名字未被改名（既有断言仍成立）
    assert any(block.name == "mcp_tools" for block in build_blocks("lead", {"event": {}, "mcp_tools": ["loki_query"]}))
