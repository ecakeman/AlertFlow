from datetime import datetime

from pydantic import BaseModel, Field


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
        if self.event not in {"incident_start", "incident_resolve", "warning"}:
            return "unknown event"
        if not self.event_id or not self.service_id or self.occurred_at is None:
            return "missing event_id, service_id, or occurred_at"
        return None


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
