import logging

from alertflow_agent.runtime.context import build_context
from alertflow_agent.runtime.llm import draft_from_state
from alertflow_agent.runtime.schemas import SCHEMAS

log = logging.getLogger("alertflow_agent")


class LLMFailed(Exception):
    pass


def call_model(model, node: str, state: dict, settings, store):
    """One model call with a rebuilt context, one retry, and a shared budget."""
    guard = state["guard"]
    if node != "write_report" and (guard["llm_calls"] >= settings.max_llm_calls or guard["tokens"] >= settings.max_total_tokens):
        state["stop_reason"] = "budget_llm_calls" if guard["llm_calls"] >= settings.max_llm_calls else "budget_tokens"
        state["finished"] = True
        return None
    prompt = build_context(node, state)
    last = None
    for _ in range(2):
        try:
            data, usage = model.complete(node, state, prompt)
            parsed = SCHEMAS[node].model_validate(data).model_dump()
            tokens = int(usage.get("total_tokens") or usage.get("estimated") or max(1, len(prompt) // 4))
            guard["llm_calls"] += 1
            guard["tokens"] += tokens
            if usage.get("source") != "provider":
                guard["tokens_estimated"] = True
            if node == "lead":
                guard["lead_rounds"] += 1
            store.append_event(state["run_id"], "llm.completed", {
                "node": node, "tokens": tokens, "source": usage.get("source") or "estimated",
            })
            if guard["tokens"] > settings.max_total_tokens:
                state["stop_reason"] = "budget_tokens"
                state["finished"] = True
            return parsed
        except Exception as exc:
            last = exc
            log.warning("llm call failed", extra={"node": node})
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


def scripted_report(state: dict) -> dict:
    return draft_from_state(state)
