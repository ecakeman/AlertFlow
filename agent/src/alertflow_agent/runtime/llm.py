from alertflow_agent.runtime.schemas import SCHEMAS


class ScriptedModel:
    """Deterministic stand-in. It proves control flow and is not a model result."""

    def __init__(self) -> None:
        self.prompts: list[tuple[str, str]] = []

    def complete(self, node: str, state: dict, prompt: str = "") -> tuple[dict, dict]:
        self.prompts.append((node, prompt))
        if node == "initial_plan":
            data = {
                "version": 1,
                "goal": "Explain the monitoring event with the collected evidence.",
                "known_facts": [{"text": "Analysis evidence is present.", "evidence_ids": ["ana:status@0"]}],
                "hypotheses": [{
                    "id": "h1", "text": "The probe observed the recorded status mix.", "layer": "probe",
                    "status": "open", "evidence_for": ["ana:status@0"], "evidence_against": [],
                    "to_test": "finish_investigation once the status counts are cited",
                }],
                "gaps": state.get("coverage", {}).get("gaps") or [],
            }
        elif node == "replan":
            old = state.get("plan") or {"version": 1, "hypotheses": []}
            data = {**old, "version": int(old.get("version") or 1) + 1}
            if state.get("replan_bad"):
                data = {"version": old.get("version"), "goal": "bad", "hypotheses": []}
        elif node == "lead":
            data = _lead(state)
        elif node == "subagent":
            data = {
                "status": "ok", "similarities": [], "differences": [], "comment_claims": [],
                "unanswered": [], "suggestion": "none", "evidence_ids": list(state.get("evidence") or [])[:2],
            }
        else:
            data = draft_from_state(state)
        return data, {"estimated": max(1, len(prompt) // 4), "source": "estimated"}


def _lead(state: dict) -> dict:
    script = state.get("script") or ["finish"]
    step = state["guard"]["lead_rounds"]
    action = script[min(step, len(script) - 1)]
    if action == "finish":
        return {"tool": "finish_investigation", "args": {"note": "evidence is sufficient"}}
    if action == "text":
        return {"tool": None, "text": "still looking"}
    if action == "replan":
        return {"tool": "request_replan", "args": {"reason": "new evidence"}}
    if action == "history":
        ids = [key for key, item in state["evidence"].items() if item["source_type"] == "alertflow_incident"][:2]
        return {"tool": "delegate_history_review", "args": {"question": "compare", "incident_ids": ids}}
    return {"tool": action, "args": state.get("script_args") or {"from": None, "to": None, "limit": 20, "range": "24h", "around": 1, "window_minutes": 30, "name": "service-unavailable"}}


def draft_from_state(state: dict) -> dict:
    status = (state["evidence"].get("ana:status@0") or {}).get("data") or {}
    offline = (status.get("offline") or {}).get("n", 0)
    return {
        "summary": "Collected checks were analyzed without claiming an internal root cause.",
        "observations": [{
            "id": "o1",
            "text": "The status analysis counts the checks in the window.",
            "evidence_ids": ["ana:status@0"],
            "facts": [{"evidence_id": "ana:status@0", "path": "offline.n", "value": offline}],
        }],
        "hypotheses": [{
            "id": "h1",
            "text": "The external probe recorded the status mix in the analysis.",
            "layer": "probe",
            "status": "possible",
            "confidence": "low",
            "supporting_evidence_ids": ["ana:status@0"],
            "weakening_evidence_ids": [],
            "rationale": "Only probe evidence is available.",
            "missing_info": [],
        }],
        "unknowns": [],
        "recommended_next_steps": [{"text": "Read the service logs for the same window.", "rationale": "The probe cannot see inside the process."}],
    }


class OpenAIModel:
    def __init__(self, settings) -> None:
        from langchain_openai import ChatOpenAI

        if not settings.llm_model or not settings.llm_base_url:
            raise RuntimeError("LLM_MODEL and LLM_BASE_URL are required when LLM_API_KEY is set")
        self.chat = ChatOpenAI(
            model=settings.llm_model,
            api_key=settings.llm_api_key,
            base_url=settings.llm_base_url,
            temperature=0,
        )

    def complete(self, node: str, state: dict, prompt: str = "") -> tuple[dict, dict]:
        schema = SCHEMAS[node]
        runnable = self.chat.with_structured_output(schema, include_raw=True)
        raw = runnable.invoke(prompt)
        parsed = raw["parsed"] if isinstance(raw, dict) else raw
        if parsed is None:
            raise ValueError("model output did not match the schema")
        message = raw.get("raw") if isinstance(raw, dict) else None
        meta = getattr(message, "usage_metadata", None) or {}
        if meta.get("total_tokens"):
            usage = {"total_tokens": int(meta["total_tokens"]), "source": "provider"}
        else:
            usage = {"estimated": max(1, len(prompt) // 4), "source": "estimated"}
        return parsed.model_dump(), usage
