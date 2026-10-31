"""与 Agent 的排查会话。

刻意**不做成第二个 agent**：没有工具调用、没有任何写操作——它只是把"这次事件已经掌握
的东西"（事件、报告、证据、长期记忆）摊开，让模型就着人提的问题解释、对照、澄清。
边界与一次性诊断完全一致：只读。

P4：上下文不再另拼字符串，而是复用 ``runtime/context.py`` 的块/层级/降级与**六区块**
（``build_chat_bundle``）——Lead 与 Chat 看到的是同一形状、同一取舍规则的上下文。
"""

from __future__ import annotations

import json
import logging

from alertflow_agent.runtime.context import DEFAULT_BUDGET_TOKENS, build_chat_bundle

log = logging.getLogger("alertflow_agent")

_MAX_CITED = 40


def _clip(value, limit: int) -> str:
    if not isinstance(value, str):
        value = json.dumps(value, ensure_ascii=False)
    text = str(value).replace("\n", " ")
    return text if len(text) <= limit else text[:limit]


def _incident_run(store, incident_id: int) -> dict | None:
    """找到该事件最近一次诊断运行（优先 incident_start）。"""
    row = store.find_incident(incident_id) if hasattr(store, "find_incident") else None
    if row:
        return row
    best = None
    for candidate in store.list_runs():
        if candidate.get("incident_id") == incident_id:
            if best is None or (candidate.get("created_at") or "") > (best.get("created_at") or ""):
                best = candidate
    return best


def _report_view(report: dict) -> dict:
    return {key: report.get(key) for key in
            ("summary", "conclusion", "hypotheses", "unknowns", "recommended_next_steps")}


def _cited_from(report: dict, evidence: dict) -> list[str]:
    cited: list[str] = []
    for hypothesis in report.get("hypotheses") or []:
        cited.extend(hypothesis.get("supporting_evidence_ids") or [])
    conclusion = report.get("conclusion") or {}
    if conclusion.get("text"):
        cited.extend(conclusion.get("evidence_ids") or [])
    cited.extend(list(evidence.keys()))
    return sorted(set(cited))[:_MAX_CITED]


def _prior_block(repo, service_id) -> str:
    """既往上下文：档案 + 最近几条诊断（结论是过去的判断，需与本次证据核对）。"""
    if repo is None or not service_id:
        return ""
    try:
        profile = repo.get_profile(service_id)
        recent = repo.list_recent(service_id, 30, 3)
    except Exception as exc:  # noqa: BLE001 - memory must not break chat
        log.warning("chat recall failed: %s", type(exc).__name__)
        return ""
    if not profile and not recent:
        return ""
    lines = ["Prior context (conclusions are past judgements — verify them):"]
    if profile and profile.get("baseline"):
        lines.append("baseline: " + _clip(profile["baseline"], 300))
    for episode in recent:
        lines.append(f"  ep:{episode['id']}｜{(episode.get('started_at') or '?')[:16]}｜"
                     f"{(episode.get('conclusion') or {}).get('text', '')}")
    return "\n".join(lines)


def build_prompt(settings, store, repo, incident_id: int, history: list, question: str) -> tuple[str, list[str]]:
    """装配一次 Chat 调用的上下文（六区块），返回 (text, cited evidence ids)。"""
    row = _incident_run(store, incident_id)
    event = (row or {}).get("input_event") or {}
    evidence = (store.get_evidence(row["id"]) if row else None) or {}
    report = ((store.latest_report(row["id"]) if row else None) or {}).get("report") or {}
    state: dict = {
        "event": event,
        "evidence": evidence,
        "question": question,
        "messages": [f"{turn.get('role')}: {_clip(str(turn.get('text') or ''), 400)}"
                     for turn in (history or [])[-8:]],
    }
    if report:
        state["report"] = _report_view(report)
    prior = _prior_block(repo, (row or {}).get("service_id"))
    if prior:
        state["prior_block"] = prior
    budget = int(getattr(settings, "context_budget_tokens", DEFAULT_BUDGET_TOKENS) or DEFAULT_BUDGET_TOKENS)
    fit = build_chat_bundle(state, budget)
    return fit.text, _cited_from(report, evidence)


def answer(settings, store, repo, model, incident_id: int, history: list, question: str) -> dict:
    """回答一个问题。答不出来就抛异常——**不要**用一句套话冒充答案。

    调用方（异步 chat worker）据此重试或标记失败，页面因此能看到"没答出来"，
    而不是一句看起来像回答的废话。
    """
    prompt, cited = build_prompt(settings, store, repo, incident_id, history, question)
    if not hasattr(model, "answer"):
        raise RuntimeError("the agent has no model configured")
    try:
        reply = model.answer(prompt)
    except Exception as exc:  # noqa: BLE001 - 记下来再抛给调用方
        log.warning("chat model failed: %s", type(exc).__name__)
        raise
    if not reply or not str(reply).strip():
        raise RuntimeError("the model returned an empty answer")
    return {"reply": str(reply).strip(), "evidence_ids": cited}
