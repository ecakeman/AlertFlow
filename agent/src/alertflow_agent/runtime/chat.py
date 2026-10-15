"""与 Agent 的排查会话。

刻意**不做成第二个 agent**：没有工具调用、没有任何写操作——它只是把"这次事件已经掌握
的东西"（事件、报告、证据、长期记忆）摊开，让模型就着人提的问题解释、对照、澄清。
边界与一次性诊断完全一致：只读。
"""

from __future__ import annotations

import json
import logging

log = logging.getLogger("alertflow_agent")

_SYSTEM = (
    "You are the diagnostic agent for one monitoring incident. Answer the engineer's question "
    "about THIS incident using only the material below (the event, the report, the evidence "
    "registry, and prior context). Read-only: you cannot change anything and must not invent "
    "actions. Cite evidence ids you rely on. If the material does not answer the question, say "
    "what is missing instead of guessing. Keep answers short and concrete: 2-5 sentences, no "
    "headings. Answer in Simplified Chinese (简体中文); keep identifiers, evidence ids and "
    "code-like tokens in English."
)


def _clip(value, limit: int) -> str:
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    text = text.replace("\n", " ")
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


def build_prompt(settings, store, repo, incident_id: int, history: list, question: str) -> tuple[str, list[str]]:
    row = _incident_run(store, incident_id)
    cited: list[str] = []
    parts = [_SYSTEM]

    if row:
        event = row.get("input_event") or {}
        parts.append("Event:\n" + _clip(event, 1200))
        saved = store.latest_report(row["id"])
        report = (saved or {}).get("report") or {}
        if report:
            parts.append("Report (yours):\n" + _clip(
                {k: report.get(k) for k in ("summary", "conclusion", "hypotheses", "unknowns", "recommended_next_steps")},
                4000))
            for hypothesis in report.get("hypotheses") or []:
                cited.extend(hypothesis.get("supporting_evidence_ids") or [])
            conclusion = report.get("conclusion") or {}
            if conclusion.get("text"):
                cited.extend(conclusion.get("evidence_ids") or [])
        evidence = store.get_evidence(row["id"]) or {}
        if evidence:
            lines = []
            for ident, item in list(evidence.items())[:24]:
                data = item.get("data") if isinstance(item.get("data"), dict) else {}
                detail = data.get("detail") if isinstance(data, dict) else None
                summary = item.get("summary") or ""
                lines.append(f"[{ident}] {item.get('kind')} — {_clip(summary, 160)}"
                             + (f" detail={_clip(detail, 300)}" if detail else ""))
            parts.append("Evidence registry:\n" + "\n".join(lines))
            cited.extend(list(evidence.keys()))
    else:
        parts.append("No diagnostic run exists for this incident yet.")

    # 长期记忆：既往同类事件与档案（同样是"先验假设"，需与本次证据核对）。
    if repo is not None:
        try:
            service_id = (row or {}).get("service_id")
            profile = repo.get_profile(service_id) if service_id else None
            recent = repo.list_recent(service_id, 30, 3) if service_id else []
            if profile or recent:
                block = ["Prior context (conclusions are past judgements — verify them):"]
                if profile and profile.get("baseline"):
                    block.append("baseline: " + _clip(profile["baseline"], 300))
                for episode in recent:
                    block.append(f"  ep:{episode['id']}｜{(episode.get('started_at') or '?')[:16]}｜"
                                 f"{(episode.get('conclusion') or {}).get('text', '')}")
                parts.append("\n".join(block))
        except Exception as exc:  # noqa: BLE001 - memory must not break chat
            log.warning("chat recall failed: %s", type(exc).__name__)

    if history:
        recent_turns = "\n".join(f"{turn.get('role')}: {_clip(str(turn.get('text') or ''), 400)}" for turn in history[-8:])
        parts.append("Conversation so far:\n" + recent_turns)
    parts.append("Engineer's question: " + question)
    return "\n\n".join(parts), sorted(set(cited))[:40]


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
