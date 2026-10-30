import logging
import time

from alertflow_agent.runtime.context import build_context_report
from alertflow_agent.runtime.context_window import estimate_tokens
from alertflow_agent.runtime.llm import draft_from_state
from alertflow_agent.runtime.schemas import SCHEMAS

log = logging.getLogger("alertflow_agent")
from alertflow_agent.runtime.budget import (  # noqa: E402  (预算账本单点记账)
    _budget_lock,
    _reconcile,
    _report_attempt_reserve,
    _report_token_reserve,
    _reserve,
    begin_call,
    end_call,
    resume_run,
)


def call_model(model, node: str, state: dict, settings, store):
    """One model call with a rebuilt context, one retry, and a shared budget."""
    guard = state["guard"]
    # 上次运行若在等 provider 时崩溃，先收敛为 indeterminate，且不重复记账
    resume_run(guard)
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
        begin_call(guard, node, estimate)
        store.append_event(state["run_id"], "llm.attempt", {"node": node, "attempt": guard["llm_attempts"]})
        try:
            data, usage = model.complete(node, state, prompt)
            parsed = SCHEMAS[node].model_validate(data).model_dump()
            tokens, source = _reconcile(guard, estimate, usage or {})
            end_call(guard)
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
            # 超时/异常发生在投递之后：attempt 与估算 token 已消耗，不退
            end_call(guard)
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
