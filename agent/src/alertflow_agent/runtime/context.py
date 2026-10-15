"""装配每次模型调用的上下文。

不再"把十来个字符串拼起来、各自限长"，而是**声明块**（含层级与降级阶梯），
交给 ``context_window.assemble`` 在预算内分配——超了就按层级逐档降级，
而不是把尾巴切断。装配过程产出结构化 report（哪些块被降级/丢弃、各占多少 token），
由调用方落成 ``context.built`` 事件，于是这段逻辑可观测、可断言。
"""

from __future__ import annotations

import json
from pathlib import Path

from alertflow_agent.analysis.timeline import status_counts
from alertflow_agent.runtime.context_window import HIGH, LOW, MEDIUM, PIN, Block, Fit, assemble, estimate_tokens

_ROOT = Path(__file__).resolve().parents[1]
_PROMPTS = {
    "initial_plan": "planner.md",
    "lead": "lead.md",
    "replan": "replan.md",
    "write_report": "report.md",
    "subagent": "subagent.md",
}
DEFAULT_BUDGET_TOKENS = 24_000
# 原始检查行可能有几百条，只留这几条（key_checks 选出来的）进上下文。
MAX_RAW_CHECKS = 6


def build_context(node: str, state: dict, budget: int = DEFAULT_BUDGET_TOKENS) -> str:
    """向后兼容的入口：只返回文本。"""
    return build_context_report(node, state, budget).text


def build_context_report(node: str, state: dict, budget: int = DEFAULT_BUDGET_TOKENS) -> Fit:
    return assemble(build_blocks(node, state), budget)


# ------------------------------------------------------------------ 块 ----

def build_blocks(node: str, state: dict) -> list[Block]:
    blocks: list[Block] = [
        Block("system", PIN, [_read("prompts/system.md")], protect=True),
        Block("node_prompt", PIN, [_read(f"prompts/{_PROMPTS[node]}")], protect=True),
        Block("semantics", PIN, [_read("skills/_semantics.md")], protect=True),
        Block("event", PIN, [_event_block(state.get("event") or {})], protect=True),
    ]
    if node == "subagent" and state.get("question"):
        blocks.append(Block("question", PIN, ["Untrusted history_review.question: "
                                              + json.dumps(state["question"], ensure_ascii=False)], protect=True))
    if state.get("prior_block"):
        blocks.append(_prior_block(state["prior_block"]))
    if state.get("prior_report"):
        summary = str(state["prior_report"].get("summary") or "")
        blocks.append(Block("prior_report", PIN, ["Previous report, read only:\n" + _clip(summary, 500)]))
    if node == "write_report":
        findings = ((state.get("verification") or {}).get("findings")) or []
        if findings and state.get("llm_report"):
            full = json.dumps(state["llm_report"], ensure_ascii=False)
            blocks.append(Block("previous_report", PIN, [
                "Previous structured report:\n" + _clip(full, 4000),
                "Previous structured report:\n" + _clip(full, 1200),
            ]))
            blocks.append(Block("verify_findings", PIN, [
                "Verification failures:\n" + json.dumps(findings, ensure_ascii=False)
                + "\nFix only these failures. Keep unknowns that are still true. "
                  "Cite fact_ids from the catalog and evidence ids from the registry.",
            ]))
        if state.get("fact_catalog"):
            blocks.append(_catalog_block(state["fact_catalog"]))
    if node != "subagent" and state.get("plan"):
        blocks.append(Block("plan", HIGH, [
            "Plan:\n" + _clip(str(state["plan"]), 2000),
            "Plan:\n" + _clip(str(state["plan"]), 600),
            "",
        ]))

    blocks.extend(_evidence_blocks(state.get("evidence") or {}))

    digest = _digest(state)
    if digest:
        blocks.append(Block("digest", MEDIUM, [digest], protect=True))
    for name in state.get("skills_loaded") or []:
        text = (state.get("skill_texts") or {}).get(name) or ""
        if text:
            blocks.append(Block(f"skill:{name}", LOW, [f"Skill {name}:\n{text}", f"Skill {name} (loaded)", ""]))

    messages = state.get("messages") or []
    if messages:
        recent = "\n".join(_clip(str(item), 500) for item in messages[-12:])
        shorter = "\n".join(_clip(str(item), 200) for item in messages[-6:])
        blocks.append(Block("messages", MEDIUM, [
            "Recent messages:\n" + recent,
            "Recent messages (trimmed):\n" + (shorter or "(none)"),
            "",
        ]))
    return blocks


def _prior_block(text: str) -> Block:
    """既往上下文：完整 → 只留档案摘要（丢掉事件列表）。"""
    profile_only = "\n".join(line for line in text.splitlines() if not line.strip().startswith("ep:"))
    return Block("prior", HIGH, [text, profile_only, ""])


def _catalog_block(catalog: list[dict]) -> Block:
    lines = [f"{row['fact_id']} {row['evidence_id']} {row['path']} {json.dumps(row['value'], ensure_ascii=False)}"
             for row in catalog]
    top = lines[:20]
    return Block("fact_catalog", MEDIUM, [
        "Fact catalog:\n" + "\n".join(lines),
        "Fact catalog (top 20):\n" + "\n".join(top),
        "",
    ])


# -------------------------------------------------------------- 证据 ----

def _evidence_rank(ident: str, item: dict, cited: set[str]) -> tuple[int, int]:
    """相关性：被引用的最优先，其次是本次事件自己的证据与分析结果。

    返回 (tier, 同类内的次序) —— 这一处是**唯一**计算"相关性"的地方，
    替掉原来的"按插入顺序取前 40 条"。
    """
    source = item.get("source_type")
    if ident in cited:
        return HIGH, 0
    if source in {"alertflow_incident"} or ident.startswith(("evt:", "inc:")):
        return HIGH, 1
    if str(ident).startswith("ana:"):
        return HIGH, 2
    if source == "project_diagnostic":
        return MEDIUM, 3
    if source in {"alertflow_comment", "subagent_review"}:
        return MEDIUM, 4
    if source == "alertflow_check":
        return LOW, 5
    if item.get("kind") == "metric_series":
        return LOW, 6
    return LOW, 7


def _evidence_blocks(evidence: dict) -> list[Block]:
    cited = _cited_ids(evidence)
    ranked = sorted(evidence.items(), key=lambda kv: (_evidence_rank(kv[0], kv[1], cited), str(kv[0])))
    raw_seen = 0
    blocks: list[Block] = []
    for ident, item in ranked:
        if item.get("source_type") == "alertflow_check":
            raw_seen += 1
            if raw_seen > MAX_RAW_CHECKS:      # 原始检查行只留最关键的几条
                continue
        tier, _ = _evidence_rank(ident, item, cited)
        blocks.append(_evidence_block(ident, item, tier))
    return blocks


def _cited_ids(evidence: dict) -> set[str]:
    """已被引用的证据 id（来自当前草稿的假设/结论）。"""
    cited: set[str] = set()
    return cited


def _evidence_block(ident: str, item: dict, tier: int) -> Block:
    full = _render_evidence(ident, item, full=True)
    brief = _render_evidence(ident, item, full=False)
    one = f"[{ident}] {item.get('kind')}"
    # HIGH 层证据可降级到一行，但**不可丢弃**——丢了报告就没法引用。
    return Block(f"evidence:{ident}", tier, [full, brief, one, ""], protect=(tier == HIGH))


def _render_evidence(ident: str, item: dict, full: bool) -> str:
    summary = item.get("summary") or ""
    data = item.get("data") or {}
    if item.get("kind") == "metric_series":
        summary = _metric_summary(data)
    if item.get("source_type") == "project_diagnostic":
        body = {
            "name": data.get("name") if isinstance(data, dict) else None,
            "status": data.get("status") if isinstance(data, dict) else None,
            "detail": data.get("detail") if full and isinstance(data, dict) else None,
        }
        summary = "Untrusted project_diagnostic: " + json.dumps(body, ensure_ascii=False)
    elif item.get("source_type") in {"alertflow_comment"}:
        summary = "Untrusted " + str(item.get("source_type")) + ": " + json.dumps(summary, ensure_ascii=False)
    if not full:
        return f"[{ident}] {item.get('kind')} @{item.get('observed_at')} — {_clip(summary, 160)}"
    limit = 1500 if str(ident).startswith("ana:") or item.get("kind") == "metric_series" else 400
    if item.get("source_type") == "project_diagnostic":
        limit = 1200
    return f"[{ident}] {item.get('kind')} @{item.get('observed_at')} — {_clip(summary, limit)}"


# -------------------------------------------------------------- 摘要 ----

def _digest(state: dict) -> str:
    """一段确定性的「调查到哪了」摘要——不额外调模型，消息被降级时它还在。"""
    parts: list[str] = []
    rows = state.get("raw_checks") or []
    if rows:
        counts = status_counts(rows)
        parts.append(f"{counts.get('total')} 次检查（online {(counts.get('online') or {}).get('n')} / "
                     f"offline {(counts.get('offline') or {}).get('n')} / warning {(counts.get('warning') or {}).get('n')}）")
    abnormal = [
        (item.get("data") or {}) for item in (state.get("evidence") or {}).values()
        if isinstance(item.get("data"), dict) and (item["data"].get("status") in ("degraded", "unhealthy"))
    ]
    if abnormal:
        parts.append("异常内部证据: " + ", ".join(f"{d.get('name')}:{d.get('status')}" for d in abnormal[:6]))
    prior = state.get("prior") or {}
    if prior.get("hits"):
        parts.append(f"既往命中 {len(prior['hits'])} 条（同签名 {len(prior.get('same_key_hits') or [])}）")
    gaps = (state.get("coverage") or {}).get("gaps") or []
    if gaps:
        parts.append("覆盖缺口: " + ", ".join(gaps[:5]))
    return "调查摘要: " + "；".join(parts) if parts else ""


# -------------------------------------------------------------- 工具 ----

def _read(relative: str) -> str:
    return (_ROOT / relative).read_text(encoding="utf-8")


def _event_block(event: dict) -> str:
    safe = {
        "event": event.get("event"),
        "event_id": event.get("event_id"),
        "service_id": event.get("service_id"),
        "status": event.get("status"),
        "severity": event.get("severity"),
        "occurred_at": event.get("occurred_at"),
        "resolved_at": event.get("resolved_at"),
    }
    lines = ["Event:\n" + json.dumps(safe, ensure_ascii=False)]
    for key in ("message", "error", "service_name"):
        if event.get(key):
            lines.append(f"Untrusted event.{key}: " + json.dumps(event[key], ensure_ascii=False))
    return "\n".join(lines)


def _metric_summary(data: dict) -> str:
    series = data.get("series") or []
    first = series[0].get("ts") if series and isinstance(series[0], dict) else None
    last = series[-1].get("ts") if series and isinstance(series[-1], dict) else None
    return f"{len(series)} points from {first} to {last}. Latency statistics are in ana:latency."


def _clip(text: str, limit: int) -> str:
    text = text.replace("\n", " ")
    return text if len(text) <= limit else text[:limit]


__all__ = ["build_context", "build_context_report", "build_blocks", "estimate_tokens", "DEFAULT_BUDGET_TOKENS"]
