import json


class ScriptedModel:
    """Deterministic stand-in. It proves control flow and is not a model result."""

    def complete(self, node: str, state: dict) -> dict:
        if node == "initial_plan":
            return {
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
        if node == "replan":
            old = state.get("plan") or {"version": 1, "hypotheses": []}
            return {**old, "version": old.get("version", 1) + 1}
        if node == "lead":
            script = state.get("script") or ["finish"]
            step = state["guard"]["lead_rounds"]
            action = script[min(step, len(script) - 1)]
            if action == "finish":
                return {"tool": "finish_investigation", "args": {"note": "evidence is sufficient"}}
            if action == "replan":
                return {"tool": "request_replan", "args": {"reason": "new evidence"}}
            if action == "history":
                ids = [k for k, v in state["evidence"].items() if v["source_type"] == "alertflow_incident"][:2]
                return {"tool": "delegate_history_review", "args": {"question": "compare", "incident_ids": ids}}
            return {"tool": action, "args": state.get("script_args") or {"from": None, "to": None, "limit": 20}}
        if node == "subagent":
            return {"status": "ok", "similarities": [], "differences": [], "comment_claims": [], "unanswered": [], "suggestion": "none"}
        return draft_from_state(state)


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

        kwargs = {"model": settings.llm_model, "api_key": settings.llm_api_key, "temperature": 0}
        if settings.llm_base_url:
            kwargs["base_url"] = settings.llm_base_url
        self.chat = ChatOpenAI(**kwargs)

    def complete(self, node: str, state: dict) -> dict:
        prompt = state.get("prompt") or ""
        message = self.chat.invoke(prompt + "\nReply with one JSON object.")
        text = message.content if isinstance(message.content, str) else json.dumps(message.content)
        start = text.find("{")
        end = text.rfind("}")
        return json.loads(text[start : end + 1])
