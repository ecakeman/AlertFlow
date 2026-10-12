"""长期记忆的结构化契约：事件卡、服务档案、检索命中。

这些模型是**契约**，不是自由字典：写入记忆前一定经过校验，读取时按模型解析。
LLM 产出的部分（报告里的 conclusion）通过 ``with_structured_output`` 绑定到同一个
``Conclusion`` 模型，保证「模型产出」与「记忆存储」用的是同一份 schema。
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field

# 与报告共用：一个结论挂在哪一层
Layer = Literal["probe", "network_dns", "tls", "app_internal", "dependency", "unknown"]
# 症状层：**外部观测**到故障发生在哪一层，与「假设层」不同——后者是原因归属。
ObservedLayer = Literal["http", "timeout", "connection_reset", "network_dns", "tls", "unknown"]
Priority = Literal["low", "medium", "high"]
Outcome = Literal["self_healed", "deploy_fixed", "manual", "open", "unknown"]

INTERNAL_LAYERS = ("dependency", "app_internal", "network_dns", "tls")


class Symptom(BaseModel):
    """外部症状的结构化指纹：Agent 判断「像不像」的第一维。"""

    model_config = ConfigDict(extra="forbid")

    status_codes: list[int] = Field(default_factory=list)
    error_classes: list[str] = Field(default_factory=list)
    failing_layer: ObservedLayer = "unknown"
    severity: str = "outage"
    offline_ratio: float = 0.0
    transitions: int = 0


class SignatureItem(BaseModel):
    """一条异常内部证据的极简描述（不带全文，带名字与状态就够检索）。"""

    model_config = ConfigDict(extra="forbid")

    kind: str
    name: str
    status: Optional[str] = None


class EvidenceSignature(BaseModel):
    """内部证据签名：哪些组件/依赖在报警 + 最早偏差时刻。"""

    model_config = ConfigDict(extra="forbid")

    abnormal: list[SignatureItem] = Field(default_factory=list)
    kinds: list[str] = Field(default_factory=list)
    earliest_deviation_at: Optional[datetime] = None


class Conclusion(BaseModel):
    """结论 = 历史记忆里最值钱的东西。**必须带证据，必须可被推翻（recurrence_of）。**"""

    model_config = ConfigDict(extra="forbid")

    text: str
    layer: Layer = "unknown"
    confidence: Priority = "low"
    evidence_ids: list[str] = Field(default_factory=list)
    recurrence_of: Optional[str] = None  # 命中的历史 episode id（ep:<uuid>）


class EpisodeOutcome(BaseModel):
    model_config = ConfigDict(extra="forbid")

    resolved_by: Outcome = "unknown"
    recurred: bool = False
    note: str = ""


class EpisodeCard(BaseModel):
    """一次诊断压成一张卡：既是嵌入的输入，也是给人看的规范化文本。"""

    model_config = ConfigDict(extra="forbid")

    service_id: str
    target_endpoint: str = ""
    profile_key: str = ""
    incident_id: Optional[int] = None
    run_id: Optional[str] = None
    started_at: Optional[datetime] = None
    resolved_at: Optional[datetime] = None
    duration_s: Optional[int] = None
    severity: str = "outage"
    outcome: EpisodeOutcome = Field(default_factory=EpisodeOutcome)
    symptom: Symptom = Field(default_factory=Symptom)
    signature: EvidenceSignature = Field(default_factory=EvidenceSignature)
    conclusion: Conclusion
    evidence_ids: list[str] = Field(default_factory=list)
    version: int = 1

    def render(self) -> str:
        """规范化文本：供嵌入与人工阅读，**只带结构化字段 + 证据 id 引用，不带全文**。"""
        sym = self.symptom
        codes = ",".join(f"HTTP {c}" for c in sym.status_codes[:4]) or sym.failing_layer
        errors = ",".join(sym.error_classes[:4]) or "-"
        inner = ", ".join(f"{item.name}:{item.status or item.kind}" for item in self.signature.abnormal[:6]) or "-"
        when = self.started_at.astimezone(timezone.utc).strftime("%Y-%m-%d %H:%M") if self.started_at else "?"
        dur = f"{self.duration_s}s" if self.duration_s is not None else "?"
        return (
            f"服务 {self.service_id}｜{when}｜{self.severity} {dur}｜"
            f"症状: {codes}；错误类: {errors}；offline 占比 {sym.offline_ratio:.0%}｜"
            f"内部: {inner}｜"
            f"结论: {self.conclusion.text}（{self.conclusion.layer}, {self.conclusion.confidence}）"
        )


class ServiceBaseline(BaseModel):
    """基线由原始检查统计得出，**不靠 LLM**。"""

    model_config = ConfigDict(extra="forbid")

    window_s: int = 0
    samples: int = 0
    p50_ms: Optional[float] = None
    p95_ms: Optional[float] = None
    online_ratio: Optional[float] = None
    check_interval_s: Optional[int] = None


class KnownCause(BaseModel):
    model_config = ConfigDict(extra="forbid")

    layer: Layer
    text: str
    count: int = 1
    last_seen: Optional[datetime] = None


class KnownPattern(BaseModel):
    """跨事件规律。**样本不足 3 只能标疑似**（护栏）。"""

    model_config = ConfigDict(extra="forbid")

    key: str
    text: str
    samples: int = 1
    tentative: bool = True


class ServiceProfile(BaseModel):
    model_config = ConfigDict(extra="forbid")

    service_id: str
    baseline: ServiceBaseline = Field(default_factory=ServiceBaseline)
    known_causes: list[KnownCause] = Field(default_factory=list)
    known_patterns: list[KnownPattern] = Field(default_factory=list)
    episode_count: int = 0


class RecallHit(BaseModel):
    """一条历史命中：先向量召回、后 rerank 精排。"""

    model_config = ConfigDict(extra="forbid")

    episode_id: str
    vector_score: float = 0.0
    rerank_score: Optional[float] = None
    started_at: Optional[datetime] = None
    card: str = ""
    conclusion: Conclusion
