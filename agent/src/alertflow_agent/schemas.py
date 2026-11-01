from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


class DiagnosisReport(BaseModel):
    model_config = ConfigDict(extra="forbid")

    summary: str = Field(description="当前故障的整体判断")
    observations: list[str] = Field(default_factory=list, description="已确认的客观表现")
    hypotheses: list[str] = Field(default_factory=list, description="可能原因及支持或削弱依据，保留确定程度")
    unknowns: list[str] = Field(default_factory=list, description="当前仍无法确认的信息")
    next_steps: list[str] = Field(default_factory=list, description="建议进一步检查或处理的方向")


class FaultSearchIndex(BaseModel):
    model_config = ConfigDict(extra="forbid")

    summary: str = Field(description="整个故障历程的简要概括")
    symptoms: list[str] = Field(default_factory=list, description="服务实际出现的异常表现")
    observations: list[str] = Field(default_factory=list, description="调查中实际获取并确认的关键发现")
    diagnosis: list[str] = Field(default_factory=list, description="最终诊断及有依据的可能原因，保留确定程度")


class QueryTimelineInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    episode_id: str
    start_time: datetime
    end_time: datetime
    monitor_point_ids: list[str] = Field(default_factory=list)
    app_event_ids: list[str] = Field(default_factory=list)

    @model_validator(mode="after")
    def validate_range(self):
        if self.start_time.tzinfo is None or self.end_time.tzinfo is None:
            raise ValueError("查询时间必须包含时区")
        if self.end_time <= self.start_time:
            raise ValueError("end_time 必须晚于 start_time")
        return self


class SearchFaultsInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    query: str = Field(min_length=1)


class TimelineRecord(BaseModel):
    record_type: Literal["MONITOR_POINT", "APP_EVENT"]
    record_id: str
    event_time: datetime
    received_at: datetime
    source: str
    content: dict
