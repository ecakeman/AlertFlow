"""端到端测试的结构化结果模型。

测试的输出不是一句 "pass/fail"，而是**一条条有名字的断言结果**——
这样报告能精确指出「哪条期望没满足」，两套运行也能逐条对比。
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field

Mode = Literal["memory_on", "memory_off"]


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


class CheckResult(BaseModel):
    """一条断言的结果：有名字、有结论、有细节（失败时给出实际值）。"""

    model_config = ConfigDict(extra="forbid")

    name: str
    passed: bool
    detail: str = ""


class Observed(BaseModel):
    """一次场景运行观测到的东西——断言只读它，不自己再去打接口。"""

    model_config = ConfigDict(extra="forbid")

    alerted: bool = False
    severity: Optional[str] = None
    detection_latency_s: Optional[float] = None
    incident_id: Optional[int] = None
    agent_status: Optional[str] = None
    agent_tokens: Optional[int] = None
    evidence_kinds: list[str] = Field(default_factory=list)
    conclusion: Optional[dict] = None
    hypotheses: list[dict] = Field(default_factory=list)
    evidence_ids: list[str] = Field(default_factory=list)
    citation_count: int = 0
    unknowns: list[dict] = Field(default_factory=list)
    prior_block_present: bool = False
    prior_hits: int = 0
    # 上下文装配的观测（来自 context.built 事件）
    context_max_tokens: Optional[int] = None
    context_budget: Optional[int] = None
    context_floor: Optional[int] = None
    context_over_floor: bool = False
    context_calls: int = 0
    context_trimmed: bool = False
    context_dropped: list[str] = Field(default_factory=list)
    context_degraded: list[str] = Field(default_factory=list)
    note: str = ""


class ScenarioResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    scenario_id: str
    name: str
    tags: list[str] = Field(default_factory=list)
    expect_alert: bool
    checks: list[CheckResult] = Field(default_factory=list)
    observed: Observed = Field(default_factory=Observed)
    error: str = ""

    @property
    def passed(self) -> bool:
        return bool(self.checks) and all(check.passed for check in self.checks) and not self.error


class SuiteReport(BaseModel):
    """一次完整跑批的结构化报告。两套模式（memory_on/off）产出的报告可直接逐条比对。"""

    model_config = ConfigDict(extra="forbid")

    mode: Mode
    started_at: str = Field(default_factory=_now)
    finished_at: Optional[str] = None
    catalog_size: int = 0
    results: list[ScenarioResult] = Field(default_factory=list)
    metrics: dict = Field(default_factory=dict)

    def summarize(self) -> None:
        total = len(self.results)
        passed = sum(1 for r in self.results if r.passed)
        self.finished_at = _now()
        self.metrics = {
            "scenarios": total,
            "passed": passed,
            "failed": total - passed,
            "checks_total": sum(len(r.checks) for r in self.results),
            "checks_passed": sum(1 for r in self.results for c in r.checks if c.passed),
            "detection_latency_s": _latencies(self.results),
            "agent_tokens_avg": _avg([r.observed.agent_tokens for r in self.results if r.observed.agent_tokens]),
        }


def _latencies(results: list[ScenarioResult]) -> dict:
    values = sorted(r.observed.detection_latency_s for r in results if r.observed.detection_latency_s)
    if not values:
        return {}
    return {
        "p50": values[len(values) // 2],
        "p95": values[min(len(values) - 1, int(len(values) * 0.95))],
        "min": values[0],
        "max": values[-1],
    }


def _avg(values: list) -> Optional[float]:
    return round(sum(values) / len(values)) if values else None


def compare(reports: dict[str, SuiteReport]) -> dict:
    """把多套模式的结果按**断言名**逐条对照——对照不是人工比文案，是比断言。

    两类差异都算数：
    - ``differing``：两模式下都跑了，但结果不同；
    - ``memory_only``：只在记忆模式下评估的断言（关闭记忆时判 N/A）——
      它们**只在一种模式下存在**，这本身就是差异，不能漏报。
    """
    out: dict = {"modes": list(reports), "per_check": {}, "per_scenario": {},
                 "differing": [], "memory_only": []}
    for mode, report in reports.items():
        for result in report.results:
            for check in result.checks:
                key = f"{result.scenario_id}::{check.name}"
                out["per_check"].setdefault(key, {})[mode] = "pass" if check.passed else "fail"
            out["per_scenario"].setdefault(result.scenario_id, {})[mode] = "pass" if result.passed else "fail"

    total_modes = len(reports)
    for key, modes in out["per_check"].items():
        if len(modes) < total_modes:
            out["memory_only"].append(key)
        elif len(set(modes.values())) > 1:
            out["differing"].append(key)
    for key, modes in out["per_scenario"].items():
        if len(modes) < total_modes:
            out["memory_only"].append(f"scenario::{key}")
        elif len(set(modes.values())) > 1:
            out["differing"].append(f"scenario::{key}")

    # 指标对照：token 成本与通过率的直接对比。
    out["metrics"] = {
        mode: {
            "scenarios_passed": report.metrics.get("passed"),
            "scenarios": report.metrics.get("scenarios"),
            "checks_passed": report.metrics.get("checks_passed"),
            "checks_total": report.metrics.get("checks_total"),
            "agent_tokens_avg": report.metrics.get("agent_tokens_avg"),
            "detection_p50_s": (report.metrics.get("detection_latency_s") or {}).get("p50"),
        }
        for mode, report in reports.items()
    }
    return out
