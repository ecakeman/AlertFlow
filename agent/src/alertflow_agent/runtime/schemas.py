from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator


Layer = Literal["probe", "network_dns", "tls", "app_internal", "dependency", "unknown"]


class PlanHypothesis(BaseModel):
    id: str
    text: str
    layer: Layer
    status: Literal["open", "supported", "possible", "weakened", "ruled_out"]
    evidence_for: list[str] = Field(default_factory=list)
    evidence_against: list[str] = Field(default_factory=list)
    to_test: str = ""


class PlanDraft(BaseModel):
    version: int
    goal: str
    known_facts: list[dict[str, Any]] = Field(default_factory=list)
    hypotheses: list[PlanHypothesis]
    gaps: list[str] = Field(default_factory=list)


class LeadDecision(BaseModel):
    tool: str | None = None
    args: dict[str, Any] = Field(default_factory=dict)
    text: str = ""


class Fact(BaseModel):
    evidence_id: str
    path: str
    value: Any


class Observation(BaseModel):
    id: str
    text: str
    evidence_ids: list[str] = Field(default_factory=list)
    facts: list[Fact] = Field(default_factory=list)


class ReportHypothesis(BaseModel):
    id: str
    text: str
    layer: Layer
    status: Literal["supported", "possible", "weakened", "ruled_out", "undetermined"]
    confidence: Literal["low", "medium", "high"] | None = None
    supporting_evidence_ids: list[str] = Field(default_factory=list)
    weakening_evidence_ids: list[str] = Field(default_factory=list)
    rationale: str = ""
    missing_info: list[str] = Field(default_factory=list)


class UnknownItem(BaseModel):
    text: str
    code: str | None = None
    evidence_ids: list[str] = Field(default_factory=list)


class NextStep(BaseModel):
    text: str
    rationale: str | None = None


class ReportDraft(BaseModel):
    model_config = ConfigDict(extra="ignore")
    summary: str
    observations: list[Observation]
    hypotheses: list[ReportHypothesis]
    unknowns: list[UnknownItem] = Field(default_factory=list)
    recommended_next_steps: list[NextStep] = Field(default_factory=list)

    @field_validator("summary")
    @classmethod
    def clip_summary(cls, value: str) -> str:
        return value[:300]


class SubagentResult(BaseModel):
    tool: str | None = None
    incident_id: int | None = None
    status: Literal["ok", "blocked", "failed"] = "ok"
    similarities: list[str] = Field(default_factory=list)
    differences: list[str] = Field(default_factory=list)
    comment_claims: list[str] = Field(default_factory=list)
    unanswered: list[str] = Field(default_factory=list)
    suggestion: str = ""
    evidence_ids: list[str] = Field(default_factory=list)


SCHEMAS = {
    "initial_plan": PlanDraft,
    "replan": PlanDraft,
    "lead": LeadDecision,
    "write_report": ReportDraft,
    "subagent": SubagentResult,
}
