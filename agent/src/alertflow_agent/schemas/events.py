from datetime import datetime

from pydantic import BaseModel, Field

# --- 入站边界 ---------------------------------------------------------------
# 信封和上下文都有硬上限。Go 侧已经做过一次有界化，这里再兜一层：对端放宽了，
# Agent 也不该跟着无界地吃数据。任何被裁掉的部分都要写进 coverage.truncated，
# 让报告能区分「没有这个数据」和「这个数据被裁掉了」。
MAX_WEBHOOK_BYTES = 512 * 1024
MAX_CONTEXT_CHECKS = 200
MAX_CONTEXT_METRIC_POINTS = 200
MAX_CONTEXT_COMMENTS = 50
MAX_CONTEXT_RELATED_INCIDENTS = 10
MAX_CONTEXT_TEXT = 2000

_AGENT_EVENTS = {"incident_start", "incident_resolve", "warning"}

# v2 的消息类型 → 内部事件名。内部 event 沿用 v1 的取值，下游逻辑因此不必分叉。
MESSAGE_TYPES = {
    "diagnostic.requested": "incident_start",
    "diagnostic.resolved": "incident_resolve",
    "diagnostic.warning": "warning",
}

TEST_MESSAGE_TYPE = "diagnostic.test"


class InboundEvent(BaseModel):
    schema_version: str
    event_id: str
    event: str
    incident_id: int | None = None
    check_id: int | None = None
    service_id: str
    service_name: str = ""
    service_url: str = ""
    status: str = ""
    severity: str = ""
    occurred_at: datetime
    started_at: datetime | None = None
    resolved_at: datetime | None = None
    attempts: int | None = None
    error: str = ""
    message: str = ""
    service: str = ""
    url: str = ""

    def normalized(self) -> dict:
        data = self.model_dump(mode="json")
        if not data["service_url"]:
            data["service_url"] = data.get("url") or ""
        if not data["service_name"]:
            data["service_name"] = data.get("service") or data["service_id"]
        return data


class WebhookBody(BaseModel):
    """v1 载荷：与 AlertFlow 早期默认 webhook 一一对应。"""

    schema_version: str = ""
    event_id: str = ""
    event: str = ""
    incident_id: int | None = None
    check_id: int | None = None
    service_id: str = ""
    service_name: str = ""
    service_url: str = ""
    status: str = ""
    severity: str = ""
    occurred_at: datetime | None = None
    started_at: datetime | None = None
    resolved_at: datetime | None = None
    attempts: int | None = None
    error: str = ""
    message: str = ""
    service: str = ""
    url: str = ""

    def problem(self) -> str | None:
        if self.schema_version != "v1":
            return "schema_version must be v1"
        if self.event not in _AGENT_EVENTS:
            return "unknown event"
        if not self.event_id or not self.service_id or self.occurred_at is None:
            return "missing event_id, service_id, or occurred_at"
        return None


class AgentInbound(BaseModel):
    """v2 信封：AlertFlow → Agent 的版本化契约。

    message_id 对同一次逻辑投递稳定，用作 run 的幂等键；correlation_id 把同一个
    事件的所有消息串在一起。payload.diagnostic_context 是**下推的默认上下文**，
    Agent 用它建 Evidence Registry，而不是再回头把同样的数据拉一遍。
    """

    schema_version: str = ""
    message_id: str = ""
    message_type: str = ""
    correlation_id: str = ""
    sent_at: datetime | None = None
    payload: dict = Field(default_factory=dict)

    def problem(self) -> str | None:
        if self.schema_version != "v2":
            return "schema_version must be v2"
        if not self.message_id:
            return "missing message_id"
        if self.message_type == TEST_MESSAGE_TYPE:
            return None
        if self.message_type not in MESSAGE_TYPES:
            return "unknown message_type"
        event = self.payload.get("event")
        if not isinstance(event, dict):
            return "payload.event is required"
        if event.get("event") not in _AGENT_EVENTS:
            return "unknown event"
        if not event.get("service_id") or not event.get("occurred_at"):
            return "missing service_id or occurred_at"
        return None


def normalize_event(raw: dict) -> dict:
    """补上 v1 历史上就有的两个兜底字段，两条入站路径共用。"""
    event = dict(raw)
    if not event.get("service_url"):
        event["service_url"] = event.get("url") or ""
    if not event.get("service_name"):
        event["service_name"] = event.get("service") or event.get("service_id") or ""
    return event


def _clip(value, limit: int = MAX_CONTEXT_TEXT) -> str:
    text = "" if value is None else str(value)
    return text if len(text) <= limit else text[:limit] + "…"


def _sanitize_incident(raw, truncated: list[str], *, with_notes: bool) -> dict:
    if not isinstance(raw, dict):
        return {}
    out = dict(raw)
    out["title"] = _clip(out.get("title"))
    if with_notes:
        comments = [c for c in (out.get("comments") or []) if isinstance(c, dict)]
        if len(comments) > MAX_CONTEXT_COMMENTS:
            comments = comments[-MAX_CONTEXT_COMMENTS:]
            truncated.append("COMMENTS_TRUNCATED_BY_AGENT")
        out["comments"] = [{**c, "body": _clip(c.get("body"))} for c in comments]
        out["externalAlerts"] = [a for a in (out.get("externalAlerts") or []) if isinstance(a, dict)]
    return out


def sanitize_context(raw: dict) -> dict:
    """把下推的默认上下文裁到 Agent 侧的边界内。

    Go 已经按同样的量级做过一次；这里是 Agent 自己的防线，而不是依赖对端守规矩。
    """
    if not isinstance(raw, dict):
        return {}
    truncated: list[str] = []
    out = dict(raw)

    incident = _sanitize_incident(raw.get("incident"), truncated, with_notes=True)
    if incident:
        out["incident"] = incident

    checks = [c for c in (raw.get("recentChecks") or []) if isinstance(c, dict)]
    if len(checks) > MAX_CONTEXT_CHECKS:
        checks = checks[-MAX_CONTEXT_CHECKS:]
        truncated.append("CHECKS_TRUNCATED_BY_AGENT")
    out["recentChecks"] = [{**c, "error": _clip(c.get("error"))} for c in checks]

    trigger = raw.get("triggeringCheck")
    if isinstance(trigger, dict):
        out["triggeringCheck"] = {**trigger, "error": _clip(trigger.get("error"))}

    metrics = raw.get("metrics")
    if isinstance(metrics, dict):
        points = [p for p in (metrics.get("series") or []) if isinstance(p, dict)]
        if len(points) > MAX_CONTEXT_METRIC_POINTS:
            points = points[-MAX_CONTEXT_METRIC_POINTS:]
            truncated.append("METRICS_TRUNCATED_BY_AGENT")
        out["metrics"] = {**metrics, "series": points}

    related = [i for i in (raw.get("relatedIncidents") or []) if isinstance(i, dict)]
    if len(related) > MAX_CONTEXT_RELATED_INCIDENTS:
        related = related[:MAX_CONTEXT_RELATED_INCIDENTS]
        truncated.append("RELATED_INCIDENTS_TRUNCATED_BY_AGENT")
    out["relatedIncidents"] = [_sanitize_incident(i, truncated, with_notes=False) for i in related]

    service = raw.get("service")
    if isinstance(service, dict):
        out["service"] = {**service, "name": _clip(service.get("name")), "url": _clip(service.get("url"))}

    coverage = dict(raw.get("coverage") or {}) if isinstance(raw.get("coverage"), dict) else {}
    coverage["truncated"] = list(coverage.get("truncated") or []) + truncated
    if truncated:
        coverage["complete"] = False
    out["coverage"] = coverage
    return out


class Evidence(BaseModel):
    id: str
    source_type: str
    source_ref: str
    service_id: str
    observed_at: str
    collected_at: str
    kind: str
    summary: str
    data: dict = Field(default_factory=dict)
    limitations: list[str] = Field(default_factory=list)
