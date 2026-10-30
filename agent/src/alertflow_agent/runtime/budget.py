"""预算账本（§7.7.12）：单点记账，避免多处各记一份。

约定：
- reserve：调用 provider 之前先记账；失败/超时同样消耗额度。
- commit：按真实 usage 修正估算差额；provider usage 与估算二选一，不重复累加。
- refund：只用于"已预留、但请求并未真正投递 provider"的路径。
- resume：把崩溃前未确认完成的 attempt 标 indeterminate；不新增计数、不重复记账。
"""

from __future__ import annotations

import threading

_budget_lock = threading.Lock()


def _report_attempt_reserve(settings) -> int:
    """报告次数从现有上限里预留。上限装不下一次完整报告周期时不拆，避免小预算无法调查。"""
    wanted = (int(settings.max_revisions) + 1) * 2
    if wanted >= int(settings.max_llm_calls):
        return 0
    return wanted


def _report_token_reserve(settings) -> int:
    if _report_attempt_reserve(settings) == 0:
        return 0
    wanted = min(
        int(settings.max_total_tokens),
        int(settings.context_budget_tokens) * (int(settings.max_revisions) + 1),
    )
    if wanted >= int(settings.max_total_tokens):
        return 0
    return wanted


def _stop_for_budget(guard: dict, settings, node: str, estimate: int) -> str | None:
    attempts = guard.get("llm_attempts", 0)
    report_attempts = guard.get("report_attempts", 0)
    attempt_reserve = _report_attempt_reserve(settings)
    token_reserve = _report_token_reserve(settings)
    if node == "write_report":
        if attempt_reserve == 0:
            return None
        if report_attempts >= attempt_reserve or attempts >= settings.max_llm_calls:
            return "budget_llm_calls"
        report_tokens = guard.get("report_tokens", 0)
        if token_reserve and report_tokens + estimate > token_reserve:
            return "budget_tokens"
        if guard.get("tokens", 0) + estimate > settings.max_total_tokens:
            return "budget_tokens"
        return None
    if attempts - report_attempts >= settings.max_llm_calls - attempt_reserve:
        return "budget_llm_calls"
    if guard.get("tokens", 0) + estimate > settings.max_total_tokens - token_reserve:
        return "budget_tokens"
    return None


def _reserve(guard: dict, settings, node: str, estimate: int) -> str | None:
    """Count one provider attempt before the call. llm_calls is that attempt count."""
    with _budget_lock:
        reason = _stop_for_budget(guard, settings, node, estimate)
        if reason:
            return reason
        guard["llm_attempts"] = guard.get("llm_attempts", 0) + 1
        guard["llm_calls"] = guard["llm_attempts"]
        guard["tokens"] = guard.get("tokens", 0) + estimate
        if node == "write_report" and _report_attempt_reserve(settings):
            guard["report_attempts"] = guard.get("report_attempts", 0) + 1
            guard["report_tokens"] = guard.get("report_tokens", 0) + estimate
        return None


def _reconcile(guard: dict, estimate: int, usage: dict) -> tuple[int, str]:
    with _budget_lock:
        if usage.get("source") == "provider":
            actual = int(usage.get("total_tokens") or estimate)
            guard["tokens"] = guard.get("tokens", 0) + actual - estimate
            return actual, "provider"
        guard["tokens_estimated"] = True
        return estimate, "estimated"


def begin_call(guard: dict, node: str, estimate: int) -> None:
    """标记"正在等 provider 回复"；崩溃后由 resume 判定为 indeterminate。"""
    with _budget_lock:
        guard["llm_inflight"] = {
            "node": node, "attempt": guard.get("llm_attempts", 0), "estimate": int(estimate),
        }


def end_call(guard: dict) -> None:
    with _budget_lock:
        guard.pop("llm_inflight", None)


def resume_run(guard: dict) -> dict | None:
    """恢复时处理悬空的 attempt：确认已记账，只标 indeterminate，不再加一次。"""
    with _budget_lock:
        inflight = guard.pop("llm_inflight", None)
        if not inflight:
            return None
        guard["indeterminate_attempts"] = guard.get("indeterminate_attempts", 0) + 1
        guard["indeterminate"] = True
        return inflight


def refund(guard: dict, settings, node: str, estimate: int) -> None:
    """预留后未真正投递 provider（预检失败）才退款；已投递的失败/超时一律不退。"""
    with _budget_lock:
        guard["tokens"] = max(0, guard.get("tokens", 0) - int(estimate))
        guard["llm_attempts"] = max(0, guard.get("llm_attempts", 0) - 1)
        guard["llm_calls"] = guard["llm_attempts"]
        if node == "write_report" and _report_attempt_reserve(settings):
            guard["report_attempts"] = max(0, guard.get("report_attempts", 0) - 1)
            guard["report_tokens"] = max(0, guard.get("report_tokens", 0) - int(estimate))
        guard.pop("llm_inflight", None)
