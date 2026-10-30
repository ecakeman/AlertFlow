def fresh_guard() -> dict:
    return {
        "lead_rounds": 0,
        "llm_calls": 0,
        "llm_attempts": 0,
        "llm_successes": 0,
        "tool_calls": 0,
        "replans": 0,
        "subagent_runs": 0,
        "tokens": 0,
        "plain_text_streak": 0,
        "tool_sigs": [],
        "stop_reason": "",
    }


def charge_llm(guard: dict, settings, text: str) -> str | None:
    guard["llm_calls"] += 1
    guard["tokens"] += max(1, len(text) // 4)
    if guard["llm_calls"] > settings.max_llm_calls:
        return "budget_llm_calls"
    if guard["tokens"] > settings.max_total_tokens:
        return "budget_tokens"
    return None


def before_lead(guard: dict, settings, started: float, now: float) -> str | None:
    if now - started > settings.run_timeout_seconds:
        return "timeout"
    if guard["lead_rounds"] >= settings.max_lead_rounds:
        return "budget_lead_rounds"
    if guard["tool_calls"] >= settings.max_tool_calls:
        return "budget_tool_calls"
    return guard.get("stop_reason") or None


def note_tool(guard: dict, signature: str) -> str | None:
    guard["tool_calls"] += 1
    guard["tool_sigs"].append(signature)
    tail = guard["tool_sigs"][-3:]
    if len(tail) == 3 and len(set(tail)) == 1:
        return "repeated_tool_call"
    return None
