"""跨来源认知契约：调查范围、统一时间窗、检索请求/结果、Prompt 六区块。

设计边界（规格 §7.7.3 / §7.7.5）：
- 这些模型是 **manager 之间的接口**，不是自由 dict；adapter 只消费父级算出的窗口，
  不允许在 adapter 内部重新推导时间窗。
- 时间统一转成 aware UTC 后再参与计算；原始来源时间字段原样保留。
- ``RetrievalRequest`` 里的 ``SourceRequest`` 只能是四种受约束请求之一，
  禁止 ``dict`` 形式的无约束扩展。
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Annotated, Literal, Optional, Union

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from alertflow_agent.schemas.memory import SourceRef as SourceReference
from alertflow_agent.schemas.memory import TimelineRecallRequest

# ---- 通用时间/可信度 ----

TimeAccuracy = Literal["exact", "derived", "reconciled_after_gap", "unknown"]
WindowKind = Literal["root_cause", "post_event_observation", "historical_recall"]
TriggerType = Literal["incident_start", "incident_resolve", "manual", "chat"]
TrustLevel = Literal[
    "observed_fact",
    "validated_fact",
    "historical_experience",
    "expert_advice",
    "model_state",
]


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def ensure_utc(value: datetime) -> datetime:
    """naive 时间一律拒绝：无法判断时区的时间不允许进入调查窗口。"""
    if value.tzinfo is None:
        raise ValueError("datetime must be timezone-aware")
    return value.astimezone(timezone.utc)


def epoch(value: datetime) -> int:
    """转 Unix 秒：Go gRPC / Prometheus / Loki 参数都用这个形式。"""
    return int(ensure_utc(value).timestamp())


class _Model(BaseModel):
    model_config = ConfigDict(extra="forbid")


class TimeRange(_Model):
    start: datetime
    end: datetime

    @field_validator("start", "end")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)

    @model_validator(mode="after")
    def _ordered(self) -> "TimeRange":
        if self.end < self.start:
            raise ValueError("time range end is before start")
        return self


class InvestigationWindow(_Model):
    """**唯一**的时间窗来源：父级算一次，四个 adapter 只消费。"""

    window_start: datetime
    window_end: datetime
    window_kind: WindowKind = "root_cause"
    anchor_field: str = "occurred_at"
    lookback_seconds: int = Field(ge=0)
    time_accuracy: TimeAccuracy = "exact"
    gap_codes: list[str] = Field(default_factory=list)

    @field_validator("window_start", "window_end")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)

    @model_validator(mode="after")
    def _ordered(self) -> "InvestigationWindow":
        if self.window_end < self.window_start:
            raise ValueError("window_end is before window_start")
        return self

    @property
    def duration_seconds(self) -> int:
        return int((self.window_end - self.window_start).total_seconds())

    def start_epoch(self) -> int:
        return epoch(self.window_start)

    def end_epoch(self) -> int:
        return epoch(self.window_end)

    def coverage(self) -> dict:
        """兼容旧 coverage 结构：下游报告/工具仍读 int 秒。"""
        return {"from": self.start_epoch(), "to": self.end_epoch(), "gaps": list(self.gap_codes)}


class InvestigationScope(_Model):
    """一次 run 的调查范围：Service + 事件三时间 + 窗口。"""

    schema_version: str = "1"
    run_id: str
    service_id: str
    incident_id: Optional[int] = None
    trigger_type: TriggerType = "incident_start"
    occurred_at: datetime
    started_at: Optional[datetime] = None
    resolved_at: Optional[datetime] = None
    observed_at: Optional[datetime] = None
    recorded_at: Optional[datetime] = None
    time_accuracy: TimeAccuracy = "exact"
    window: InvestigationWindow

    @field_validator(
        "occurred_at", "started_at", "resolved_at", "observed_at", "recorded_at"
    )
    @classmethod
    def _aware(cls, value: Optional[datetime]) -> Optional[datetime]:
        return ensure_utc(value) if value is not None else None


# ---- 四类来源请求（必须是有约束的联合类型） ----

AlertFlowResource = Literal[
    "incident",
    "related_incidents",
    "checks",
    "diagnostics",
    "service_timeline",
    "kubernetes_state",
    "aggregated_metrics",
]

# Go 的 metrics 端点只接受固定跨度，请求窗口只能映射到其中一个。
MetricRange = Literal["1h", "6h", "24h", "7d", "30d"]


class AlertFlowQueryRequest(_Model):
    request_id: str
    run_id: str
    service_id: str
    incident_id: Optional[int] = None
    resources: list[AlertFlowResource] = Field(min_length=1)
    window_start: datetime
    window_end: datetime
    page_size: int = Field(default=50, gt=0)
    page_token: Optional[str] = None
    include_inactive: bool = False
    # 留空表示由窗口派生（见 ContextManager._metric_range）；派生结果与实际调用都写进
    # ``query_metadata``，窗口与跨度不一致时可核对。
    metrics_range: Optional[MetricRange] = None

    @field_validator("window_start", "window_end")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)

    @model_validator(mode="after")
    def _ordered(self) -> "AlertFlowQueryRequest":
        if self.window_end < self.window_start:
            raise ValueError("window_end is before window_start")
        return self


class PrometheusQueryRequest(_Model):
    request_id: str
    service_id: str
    selector: dict[str, str] = Field(default_factory=dict)
    query_mode: Literal["instant", "range"] = "range"
    promql: str
    start: datetime
    end: datetime
    step_seconds: int = Field(default=60, gt=0)
    max_samples: int = Field(default=100, gt=0)
    timeout_seconds: int = Field(default=10, gt=0)
    # MCP 原生工具名与已整形参数：工具层只做参数整形，调用/状态映射仍归 ContextManager。
    tool: str = ""
    tool_args: dict = Field(default_factory=dict)

    @field_validator("start", "end")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)


class LokiQueryRequest(_Model):
    request_id: str
    service_id: str
    selector: dict[str, str] = Field(default_factory=dict)
    logql: str
    start: datetime
    end: datetime
    limit: int = Field(default=100, gt=0)
    direction: Literal["forward", "backward"] = "backward"
    timeout_seconds: int = Field(default=10, gt=0)
    tool: str = ""
    tool_args: dict = Field(default_factory=dict)

    @field_validator("start", "end")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)


SourceRequest = Union[
    AlertFlowQueryRequest, PrometheusQueryRequest, LokiQueryRequest, TimelineRecallRequest
]

RetrievalPurpose = Literal[
    "initial_context", "targeted_followup", "chat_explanation", "historical_recall"
]


class RetrievalRequest(_Model):
    """唯一检索入口的入参：范围、用途、来源请求、预算/deadline。"""

    request_id: str
    scope: InvestigationScope
    purpose: RetrievalPurpose = "initial_context"
    source_requests: list[SourceRequest] = Field(min_length=1)
    result_limit: int = Field(default=100, gt=0)
    response_byte_limit: int = Field(default=262144, gt=0)
    deadline_at: Optional[datetime] = None
    budget_reservation_id: Optional[str] = None
    dedupe_key: Optional[str] = None

    @field_validator("deadline_at")
    @classmethod
    def _aware(cls, value: Optional[datetime]) -> Optional[datetime]:
        return ensure_utc(value) if value is not None else None

    @model_validator(mode="after")
    def _same_scope(self) -> "RetrievalRequest":
        window = self.scope.window
        for request in self.source_requests:
            if getattr(request, "service_id", self.scope.service_id) != self.scope.service_id:
                # 跨 Service 的请求不得进入同一次检索
                raise ValueError("SERVICE_SCOPE_MISMATCH")
            if isinstance(request, AlertFlowQueryRequest):
                if request.run_id != self.scope.run_id:
                    raise ValueError("RUN_SCOPE_MISMATCH")
                self._same_window(request.window_start, request.window_end, window)
            elif isinstance(request, (PrometheusQueryRequest, LokiQueryRequest)):
                self._same_window(request.start, request.end, window)
            elif isinstance(request, TimelineRecallRequest):
                if request.time_start is not None and request.time_start < window.window_start:
                    raise ValueError("WINDOW_MISMATCH")
                if request.time_end is not None and request.time_end > window.window_end:
                    raise ValueError("WINDOW_MISMATCH")
        return self

    @staticmethod
    def _same_window(start: datetime, end: datetime, window: InvestigationWindow) -> None:
        # adapter 不允许自己重算时间窗：请求窗口必须与 scope.window 完全一致
        if start != window.window_start or end != window.window_end:
            raise ValueError("WINDOW_MISMATCH")


# ---- 检索结果归一化 ----

SourceType = Literal[
    "checks",
    "diagnostic",
    "timeline",
    "kubernetes",
    "go_metric",
    "prometheus",
    "loki",
    "memory_reference",
    "runbook",
]
SourceSystem = Literal[
    "alertflow_grpc", "prometheus_mcp", "loki_mcp", "agent_memory", "ragflow_a2a"
]
RetrievalStatus = Literal[
    "ok",
    "empty",
    "disabled",
    "unavailable",
    "timeout",
    "invalid_request",
    "failed",
    "truncated",
]
RecordType = Literal[
    "check",
    "timeline_event",
    "kubernetes_object_state",
    "prometheus_sample",
    "loki_log_line",
    "memory_attachment",
    # P2：Go Runtime Timeline 统一为时间轴后，节点与人工纠正也需要自己的记录类型。
    "timeline_node",
    "memory_correction",
    # P1：同服务的其它 Incident（历史对比用），与当前 Incident 本身区分开。
    "incident_reference",
]

CoverageGapCode = Literal[
    "SOURCE_DISABLED",
    "SOURCE_UNAVAILABLE",
    "QUERY_TIMEOUT",
    "NO_MATCHING_DATA",
    "SELECTOR_MISSING",
    "SERVICE_SCOPE_MISMATCH",
    "WINDOW_GAP",
    "RESULT_TRUNCATED",
    "PERSISTENCE_FAILED",
    "INVALID_RESPONSE",
]


class CoverageGap(_Model):
    """gap 只做补充说明，**不取代 status**（规格 §7.7.5）。"""

    code: CoverageGapCode
    source_type: Optional[SourceType] = None
    detail: str = ""


class SourceError(_Model):
    category: Literal[
        "config", "transport", "timeout", "auth", "malformed", "budget", "unknown"
    ] = "unknown"
    message: str = ""
    retryable: bool = False


class TruncationInfo(_Model):
    truncated: bool = False
    reason: str = ""
    limit: Optional[int] = None
    returned: Optional[int] = None


class RetrievedItem(_Model):
    source_record_id: Optional[str] = None
    record_type: RecordType
    service_id: str
    occurred_at: Optional[datetime] = None
    observed_at: Optional[datetime] = None
    recorded_at: Optional[datetime] = None
    summary: str = ""
    data: dict = Field(default_factory=dict)
    source_locator: dict = Field(default_factory=dict)
    content_digest: str = ""

    @field_validator("occurred_at", "observed_at", "recorded_at")
    @classmethod
    def _aware(cls, value: Optional[datetime]) -> Optional[datetime]:
        return ensure_utc(value) if value is not None else None


class RetrievalResult(_Model):
    request_id: str
    run_id: str
    service_id: str
    source_type: SourceType
    source_system: SourceSystem
    status: RetrievalStatus = "ok"
    query_metadata: dict = Field(default_factory=dict)
    items: list[RetrievedItem] = Field(default_factory=list)
    collected_at: datetime = Field(default_factory=utc_now)
    observed_range: Optional[TimeRange] = None
    truncation: TruncationInfo = Field(default_factory=TruncationInfo)
    coverage_gaps: list[CoverageGap] = Field(default_factory=list)
    error: Optional[SourceError] = None

    @field_validator("collected_at")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)


class BudgetUsage(_Model):
    reserved_calls: int = 0
    used_calls: int = 0
    reserved_bytes: int = 0
    used_bytes: int = 0


class RetrievalBatch(_Model):
    """一次 retrieve 的批次结果：**不生成 evidence_id**，登记交给 EvidenceManager。"""

    batch_id: str
    run_id: str
    scope: InvestigationScope
    results: list[RetrievalResult] = Field(default_factory=list)
    started_at: datetime = Field(default_factory=utc_now)
    completed_at: Optional[datetime] = None
    budget_usage: BudgetUsage = Field(default_factory=BudgetUsage)
    overall_coverage: Literal["complete", "partial", "gap"] = "complete"

    @model_validator(mode="after")
    def _ordered(self) -> "RetrievalBatch":
        # 按 source_type 固定顺序排序，保证 prompt / 测试稳定
        order = ["checks", "diagnostic", "timeline", "kubernetes", "go_metric",
                 "prometheus", "loki", "memory_reference", "runbook"]
        self.results.sort(key=lambda item: order.index(item.source_type))
        return self

    def by_source(self, source_type: str) -> list[RetrievalResult]:
        return [item for item in self.results if item.source_type == source_type]


# ---- Prompt 六区块 ----

ContextBlockName = Literal[
    "current_task",
    "service_timeline",
    "current_evidence",
    "historical_experience",
    "expert_knowledge",
    "investigation_state",
]

BLOCK_ORDER: tuple[str, ...] = (
    "current_task",
    "service_timeline",
    "current_evidence",
    "historical_experience",
    "expert_knowledge",
    "investigation_state",
)


class ContextBuildRequest(_Model):
    run_id: str
    scope: InvestigationScope
    node: str
    budget_tokens: int = Field(default=24_000, gt=0)
    reserved_output_tokens: int = Field(default=0, ge=0)
    max_items: int = Field(default=100, gt=0)


class ContextBlock(_Model):
    name: ContextBlockName
    priority: Literal["PIN", "HIGH", "MEDIUM", "LOW"] = "MEDIUM"
    trust_level: TrustLevel = "observed_fact"
    required: bool = False
    max_items: int = 100
    content: str = ""
    render_variants: list[str] = Field(default_factory=list)
    source_refs: list[SourceReference] = Field(default_factory=list)
    dropped_item_count: int = 0
    degradation_reason: str = ""


class PromptContextBundle(_Model):
    run_id: str
    scope: InvestigationScope
    blocks: list[ContextBlock] = Field(default_factory=list)
    rendered_prompt_sections: str = ""
    estimated_input_tokens: int = 0
    reserved_output_tokens: int = 0
    budget_limit: int = 24_000
    truncation_summary: dict = Field(default_factory=dict)
    source_refs: list[SourceReference] = Field(default_factory=list)


def default_window(occurred_at: datetime, lookback_seconds: int = 3600) -> InvestigationWindow:
    """父级之外的兜底构造：只在没有事件上下文（如纯 chat）时使用。"""
    end = ensure_utc(occurred_at)
    return InvestigationWindow(
        window_start=end - timedelta(seconds=lookback_seconds),
        window_end=end,
        window_kind="root_cause",
        anchor_field="occurred_at",
        lookback_seconds=lookback_seconds,
    )
