import logging
import threading
import time

from alertflow_agent.runtime.context import build_context_report
from alertflow_agent.runtime.context_window import estimate_tokens
from alertflow_agent.runtime.llm import draft_from_state
from alertflow_agent.runtime.schemas import SCHEMAS

log = logging.getLogger("alertflow_agent")
_budget_lock = threading.Lock()


class LLMFailed(Exception):
    pass


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


def call_model(model, node: str, state: dict, settings, store):
    """One model call with a rebuilt context, one retry, and a shared budget."""
    guard = state["guard"]
    fit = build_context_report(node, state, settings.context_budget_tokens)
    prompt = fit.text
    estimate = estimate_tokens(prompt)
    # 上下文装配是可观测的：哪块被降级/丢弃、各占多少 token，都落成事件。
    store.append_event(state["run_id"], "context.built", {"node": node, **fit.summary()})
    last = None
    for _ in range(2):
        reason = _reserve(guard, settings, node, estimate)
        if reason:
            state["stop_reason"] = reason
            state["finished"] = True
            if node == "write_report" and _report_attempt_reserve(settings):
                state["failure_reason"] = reason
            if last is None:
                return None
            break
        started = time.perf_counter()
        store.append_event(state["run_id"], "llm.attempt", {"node": node, "attempt": guard["llm_attempts"]})
        try:
            data, usage = model.complete(node, state, prompt)
            parsed = SCHEMAS[node].model_validate(data).model_dump()
            tokens, source = _reconcile(guard, estimate, usage or {})
            with _budget_lock:
                guard["llm_successes"] = guard.get("llm_successes", 0) + 1
                if node == "write_report" and _report_attempt_reserve(settings):
                    guard["report_tokens"] = guard.get("report_tokens", 0) + tokens - estimate
            if node == "lead":
                guard["lead_rounds"] += 1
            store.append_event(state["run_id"], "llm.completed", {
                "node": node, "attempt": guard["llm_attempts"], "tokens": tokens, "source": source,
                "elapsed_ms": int((time.perf_counter() - started) * 1000),
            })
            over_global = guard["tokens"] > settings.max_total_tokens
            over_report = (
                node == "write_report"
                and _report_token_reserve(settings)
                and guard.get("report_tokens", 0) > _report_token_reserve(settings)
            )
            if over_report or (node != "write_report" and over_global):
                state["stop_reason"] = "budget_tokens"
                state["finished"] = True
                if over_report:
                    state["failure_reason"] = "budget_tokens"
            elif node == "write_report" and over_global and _report_attempt_reserve(settings):
                state["stop_reason"] = "budget_tokens"
                state["failure_reason"] = "budget_tokens"
                state["finished"] = True
            return parsed
        except Exception as exc:
            last = exc
            detail = _redact(str(exc), settings)
            log.warning("llm call failed", extra={"node": node})
            store.append_event(state["run_id"], "llm.attempt_failed", {
                "node": node, "attempt": guard["llm_attempts"], "error": type(exc).__name__,
                "detail": detail[:400], "source": "estimated",
                "elapsed_ms": int((time.perf_counter() - started) * 1000),
            })
    if node == "write_report" and state.get("failure_reason"):
        return None
    # lead / subagent 的产出只决定"下一步做什么"，不该因为一次不合规输出就让整次运行失败：
    # 退化为"结束调查"，报告照常产出（少的是额外取证，不是结论）。
    if node in ("lead", "subagent"):
        store.append_event(state["run_id"], "llm.degraded", {"node": node,
                          "reason": type(last).__name__ if last else "error"})
        return {"tool": "finish_investigation", "args": {"note": "model output invalid; finishing"}, "text": ""}
    state["failure_reason"] = "llm_error"
    detail = str(last) if last else "error"
    for secret in (settings.llm_api_key, settings.agent_api_token, settings.agent_webhook_token, settings.alertflow_agent_token):
        if secret:
            detail = detail.replace(secret, "[redacted]")
    store.append_event(state["run_id"], "llm.failed", {
        "node": node,
        "error": type(last).__name__ if last else "error",
        "detail": detail[:400],
    })
    return None


def _redact(detail: str, settings) -> str:
    for secret in (settings.llm_api_key, settings.agent_api_token, settings.agent_webhook_token, settings.alertflow_agent_token):
        if secret:
            detail = detail.replace(secret, "[redacted]")
    return detail


def scripted_report(state: dict) -> dict:
    return draft_from_state(state)
