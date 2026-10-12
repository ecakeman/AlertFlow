"""类型化断言注册表。

每条断言是一个纯函数：读「场景期望 + 观测结果」→ 一条 ``CheckResult``（或不适用时 None）。
Runner 不认识任何具体场景，只按注册表逐条跑——**引擎里没有"如果是场景 X 就……"**。
"""

from __future__ import annotations

from typing import Callable, Optional

from .models import CheckResult, Observed

Check = Callable[[dict, Observed, str], Optional[CheckResult]]
REGISTRY: list[Check] = []


def check(fn: Check) -> Check:
    REGISTRY.append(fn)
    return fn


def run_checks(scenario: dict, observed: Observed, mode: str = "memory_off") -> list[CheckResult]:
    """按注册表逐条跑；返回 None 表示"本场景/本模式下不适用"，不计入通过率。"""
    results = []
    for fn in REGISTRY:
        result = fn(scenario, observed, mode)
        if result is not None:
            results.append(result)
    return results


def _expect(scenario: dict) -> dict:
    return scenario.get("expect") or {}


# --------------------------------------------------------------- 告警 ----

@check
def alert_fires(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    want = bool((_expect(scenario).get("alert") or {}).get("should_fire"))
    if not want:
        return None
    return CheckResult(name="alert_fires", passed=observed.alerted,
                       detail="" if observed.alerted else "期望产生告警，实际没有")


@check
def no_alert(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    if (_expect(scenario).get("alert") or {}).get("should_fire"):
        return None
    return CheckResult(name="no_alert", passed=not observed.alerted,
                       detail="" if not observed.alerted else "期望无误报，实际产生了告警")


@check
def severity_is(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    severity = (_expect(scenario).get("alert") or {}).get("severity")
    if not severity or not observed.alerted:
        return None
    return CheckResult(name="severity_is", passed=observed.severity == severity,
                       detail=f"期望 {severity}，实际 {observed.severity}")


@check
def detection_within(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    limit = (_expect(scenario).get("alert") or {}).get("max_detection_latency_s")
    if not limit or not observed.alerted:
        return None
    actual = observed.detection_latency_s
    return CheckResult(name="detection_within", passed=actual is not None and actual <= limit,
                       detail=f"上限 {limit}s，实际 {actual}s")


# --------------------------------------------------------------- 证据 ----

@check
def evidence_kind_present(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    wanted = (_expect(scenario).get("evidence") or {}).get("present_kinds") or []
    if not wanted:
        return None
    missing = [kind for kind in wanted if kind not in observed.evidence_kinds]
    return CheckResult(name="evidence_kind_present", passed=not missing,
                       detail=f"缺少证据种类: {missing}" if missing else f"实际 {observed.evidence_kinds}")


@check
def evidence_kind_absent(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    banned = (_expect(scenario).get("evidence") or {}).get("absent_kinds") or []
    if not banned:
        return None
    present = [kind for kind in banned if kind in observed.evidence_kinds]
    return CheckResult(name="evidence_kind_absent", passed=not present,
                       detail=f"不应出现却出现: {present}" if present else "")


# --------------------------------------------------------------- 报告 ----

@check
def report_has_conclusion(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    if not (_expect(scenario).get("report") or {}).get("require_conclusion", True):
        return None
    if not observed.agent_status:
        return None
    text = (observed.conclusion or {}).get("text") if observed.conclusion else None
    return CheckResult(name="report_has_conclusion", passed=bool(text),
                       detail="" if text else "报告没有 conclusion")


@check
def hypotheses_at_least(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    minimum = (_expect(scenario).get("report") or {}).get("min_hypotheses") or 0
    if not minimum or not observed.agent_status:
        return None
    actual = len(observed.hypotheses)
    return CheckResult(name="hypotheses_at_least", passed=actual >= minimum,
                       detail=f"要求 ≥{minimum} 条，实际 {actual} 条")


@check
def report_layers_include(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    wanted = (_expect(scenario).get("report") or {}).get("layers_include") or []
    if not wanted or not observed.agent_status:
        return None
    layers = {h.get("layer") for h in observed.hypotheses}
    missing = [layer for layer in wanted if layer not in layers]
    return CheckResult(name="report_layers_include", passed=not missing,
                       detail=f"缺少假设层: {missing}（实际 {sorted(l for l in layers if l)}）" if missing else "")


@check
def cites_evidence(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    if not (_expect(scenario).get("report") or {}).get("must_cite_evidence", True) or not observed.agent_status:
        return None
    return CheckResult(name="cites_evidence", passed=observed.citation_count > 0,
                       detail="" if observed.citation_count else "结论/假设未引用任何证据")


@check
def unknowns_exclude(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    banned = (_expect(scenario).get("report") or {}).get("unknowns_exclude") or []
    if not banned or not observed.agent_status:
        return None
    codes = {str(u.get("code") or "") for u in observed.unknowns}
    hit = [code for code in banned if code in codes]
    return CheckResult(name="unknowns_exclude", passed=not hit,
                       detail=f"仍出现: {hit}" if hit else "")


# --------------------------------------------------------------- 记忆 ----

@check
def context_within_budget(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    """上下文不超预算——但**不可再压缩的地板**（指令 + 受保护证据）优先于预算。"""
    if not observed.context_calls or observed.context_max_tokens is None:
        return None
    ceiling = max(observed.context_budget or 0, observed.context_floor or 0)
    ok = observed.context_max_tokens <= ceiling
    note = "（预算低于地板，以地板为准）" if observed.context_over_floor else ""
    return CheckResult(name="context_within_budget", passed=ok,
                       detail=f"{observed.context_calls} 次调用，峰值 {observed.context_max_tokens} / 上限 {ceiling}{note}")


@check
def context_trim_reports_digest(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    """一旦发生降级/丢弃，必须留下了「调查摘要」——信息不能断崖丢失。"""
    if not observed.context_trimmed:
        return None
    has_digest = any(name == "digest" for name in observed.context_degraded) or \
        not any(name == "digest" for name in observed.context_dropped)
    return CheckResult(name="context_trim_reports_digest", passed=has_digest,
                       detail=f"已降级 {observed.context_degraded[:4]} · 已丢弃 {observed.context_dropped[:4]}")


@check
def recalls_prior(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    memory = _expect(scenario).get("memory") or {}
    if mode != "memory_on" or not memory.get("should_recall_prior"):
        return None  # 记忆关闭时，"应命中既往事件"不适用，不计入通过率
    if not observed.agent_status:
        return None
    return CheckResult(name="recalls_prior", passed=observed.prior_hits > 0,
                       detail=f"既往命中 {observed.prior_hits} 条" if observed.prior_hits else "未命中任何既往事件")


@check
def baseline_available(scenario: dict, observed: Observed, mode: str = "memory_off") -> Optional[CheckResult]:
    """基线可用 = 报告不再抱怨 NO_BASELINE，或上下文里带了基线。"""
    memory = _expect(scenario).get("memory") or {}
    if mode != "memory_on" or not memory.get("baseline_expected") or not observed.agent_status:
        return None  # 同上：记忆关闭时不评估基线的可得性
    codes = {str(u.get("code") or "") for u in observed.unknowns}
    have_baseline = observed.prior_block_present or "NO_BASELINE" not in codes
    return CheckResult(name="baseline_available", passed=have_baseline,
                       detail="" if have_baseline else "仍报告 NO_BASELINE 且上下文无基线")
