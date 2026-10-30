"""证据与事实目录契约（C2 / 规格 §7.7.6）。

关键不变量：
- ``evidence_id`` 由**父级**（EvidenceManager）根据 run + 来源定位 + 窗口 + payload 摘要
  稳定生成；不接受模型自报，也不允许检索层生成。
- 批量登记逐候选校验：某个来源非法只拒绝该候选，其他合法候选照常入库。
- FactCatalogEntry 只能由已登记的 evidence 派生，``value`` 只允许标量。
- ``verify_report`` 的 ``passed`` 只代表确定性校验通过，**不代表因果被证明**。
"""

from __future__ import annotations

from datetime import datetime
from typing import Literal, Optional, Union

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from alertflow_agent.schemas.context import (
    CoverageGap,
    InvestigationWindow,
    SourceSystem,
    SourceType,
    TimeRange,
    TrustLevel,
    ensure_utc,
    utc_now,
)

RegistrationStatus = Literal["accepted", "rejected", "partial"]
RejectionCode = Literal[
    "SERVICE_MISMATCH",
    "INCIDENT_MISMATCH",
    "WINDOW_MISMATCH",
    "SELECTOR_MISSING",
    "ROWS_EXCEEDED",
    "BYTES_EXCEEDED",
    "SCHEMA_INVALID",
    "PAYLOAD_EMPTY",
    "INVALID_SOURCE",
]
ValueType = Literal["string", "number", "boolean", "null", "datetime"]
FactKind = Literal["state", "transition", "metric", "log", "diagnostic", "expert_advice", "metadata"]


class _Model(BaseModel):
    model_config = ConfigDict(extra="forbid")


class EvidenceCandidate(_Model):
    """检索层交来的原始候选：**还没有 evidence_id**。"""

    candidate_id: str
    run_id: str
    service_id: str
    incident_id: Optional[int] = None
    source_type: SourceType
    source_system: SourceSystem
    source_locator: dict = Field(default_factory=dict)
    query_metadata: dict = Field(default_factory=dict)
    window: InvestigationWindow
    observed_range: Optional[TimeRange] = None
    collected_at: datetime = Field(default_factory=utc_now)
    payload_schema: str = "1"
    payload: dict = Field(default_factory=dict)
    content_digest: str = ""
    truncated: bool = False
    trust_level: TrustLevel = "observed_fact"

    @field_validator("collected_at")
    @classmethod
    def _aware(cls, value: datetime) -> datetime:
        return ensure_utc(value)

    @model_validator(mode="after")
    def _within_window(self) -> "EvidenceCandidate":
        # 观测区间必须落在调查窗口内；越窗数据不得登记（规格 §7.7.5 第 6 步）
        if self.observed_range is not None:
            if self.observed_range.start < self.window.window_start:
                raise ValueError("OBSERVED_RANGE_BEFORE_WINDOW")
            if self.observed_range.end > self.window.window_end:
                raise ValueError("OBSERVED_RANGE_AFTER_WINDOW")
        return self


class EvidenceRecord(EvidenceCandidate):
    """已登记证据：在候选字段上补 evidence_id 与登记状态。"""

    evidence_id: str
    registration_status: RegistrationStatus = "accepted"


class FactCatalogEntry(_Model):
    fact_id: str
    evidence_id: str
    path: str
    value: Union[str, int, float, bool, None]
    value_type: ValueType
    unit: Optional[str] = None
    fact_kind: FactKind = "state"
    occurred_at: Optional[datetime] = None
    observed_at: Optional[datetime] = None
    recorded_at: Optional[datetime] = None
    service_id: str
    trust_level: TrustLevel = "observed_fact"

    @model_validator(mode="after")
    def _scalar(self) -> "FactCatalogEntry":
        if isinstance(self.value, (dict, list)):
            raise ValueError("fact value must be scalar")
        return self


class RejectedCandidate(_Model):
    candidate_id: str
    reason_code: RejectionCode
    summary: str = ""


class EvidenceRegistrationResult(_Model):
    run_id: str
    accepted_evidence_ids: list[str] = Field(default_factory=list)
    duplicate_evidence_ids: list[str] = Field(default_factory=list)
    rejected_candidates: list[RejectedCandidate] = Field(default_factory=list)
    fact_count_by_source_type: dict[str, int] = Field(default_factory=dict)
    coverage_gaps: list[CoverageGap] = Field(default_factory=list)
    catalog_truncated: bool = False


class ReportVerificationRequest(_Model):
    run_id: str
    report: dict
    records: list[EvidenceRecord] = Field(default_factory=list)
    facts: list[FactCatalogEntry] = Field(default_factory=list)
    tokens: list[str] = Field(default_factory=list)


class VerificationResult(_Model):
    """确定性校验结果；``passed`` 只表示结构/引用合法。"""

    passed: bool = False
    schema_errors: list[str] = Field(default_factory=list)
    unknown_evidence_ids: list[str] = Field(default_factory=list)
    invalid_fact_refs: list[str] = Field(default_factory=list)
    invalid_document_citations: list[str] = Field(default_factory=list)
    hypothesis_reference_errors: list[str] = Field(default_factory=list)
    warnings: list[str] = Field(default_factory=list)
