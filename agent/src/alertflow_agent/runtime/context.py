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
from alertflow_agent.schemas.context import (
    BLOCK_ORDER,
    ContextBlock,
    InvestigationScope,
    PromptContextBundle,
    SourceReference,
    default_window,
    utc_now,
)
from alertflow_agent.runtime.window import scope_from_event, window_from_state

_ROOT = Path(__file__).resolve().parents[1]
_PROMPTS = {
    "initial_plan": "planner.md",
    "lead": "lead.md",
    "replan": "replan.md",
    "write_report": "report.md",
    "subagent": "subagent.md",
    "chat": "chat.md",
}
DEFAULT_BUDGET_TOKENS = 24_000
# 原始检查行可能有几百条，只留这几条（key_checks 选出来的）进上下文。
MAX_RAW_CHECKS = 6


def build_context(node: str, state: dict, budget: int = DEFAULT_BUDGET_TOKENS) -> str:
    """向后兼容的入口：只返回文本。"""
    return build_context_report(node, state, budget).text


def build_context_report(node: str, state: dict, budget: int = DEFAULT_BUDGET_TOKENS) -> Fit:
    """装配上下文，并把叶子块重组为**固定六区块**（规格 §7.7.5）。"""
    leaves = build_blocks(node, state)
    fit = assemble(leaves, budget)
    sections = group_sections(leaves, fit.texts)
    text = render_sections(sections)
    return Fit(text=text, tokens=estimate_tokens(text), budget=budget, floor=fit.floor,
               blocks=fit.blocks, texts=[section["content"] for section in sections])


# ------------------------------------------------------------- 六区块 ----

# 固定顺序：current_task / service_timeline / current_evidence /
# historical_experience / expert_knowledge / investigation_state。
SECTION_ORDER = list(BLOCK_ORDER)
_SECTION_PRIORITY = {
    "current_task": "PIN",
    "service_timeline": "HIGH",
    "current_evidence": "PIN",
    "historical_experience": "MEDIUM",
    "expert_knowledge": "LOW",
    "investigation_state": "HIGH",
}
_SECTION_TRUST = {
    "current_task": "model_state",
    "service_timeline": "observed_fact",
    "current_evidence": "observed_fact",
    "historical_experience": "historical_experience",
    "expert_knowledge": "expert_advice",
    "investigation_state": "model_state",
}
_SECTION_REQUIRED = {"current_task": True, "current_evidence": True}
_SECTION_HEADERS = {
    "current_task": "## current_task（任务与当前事件）",
    "service_timeline": "## service_timeline（时间线/事件流）",
    "current_evidence": "## current_evidence（本次调查证据）",
    "historical_experience": "## historical_experience（历史记忆，需复核）",
    "expert_knowledge": "## expert_knowledge（专家知识与技能，非本次证据）",
    "investigation_state": "## investigation_state（计划、消息与调查状态）",
}


def section_of(name: str) -> str:
    """叶子块 -> 六区块之一。未识别的一律落到 investigation_state，不丢内容。"""
    if name in ("system", "node_prompt", "event", "question", "messages"):
        return "current_task"
    if name.startswith("evidence:"):
        ident = name[len("evidence:"):]
        if ident.startswith(("tl:", "k8s:", "inc:", "evt:")):
            return "service_timeline"
        if ident.startswith("ep:"):
            return "historical_experience"
        if ident.startswith("runbook:"):
            return "expert_knowledge"
        return "current_evidence"
    if name in ("digest", "fact_catalog", "verify_findings", "previous_report", "report"):
        return "current_evidence"
    if name in ("prior", "prior_report"):
        return "historical_experience"
    if name in ("semantics", "mcp_tools", "runbook_tool") or name.startswith("skill:"):
        return "expert_knowledge"
    return "investigation_state"


def group_sections(leaves: list[Block], texts: list[str]) -> list[dict]:
    """按固定顺序聚合叶子块被裁剪后的文本；空区块省略，但六区块顺序不变。"""
    buckets: dict[str, dict] = {}
    for index, block in enumerate(leaves):
        name = section_of(block.name)
        content = texts[index] if index < len(texts) else ""
        if not content:
            continue
        bucket = buckets.setdefault(name, {"name": name, "parts": [], "items": 0})
        bucket["parts"].append(content)
        bucket["items"] += 1
    sections = []
    for name in SECTION_ORDER:
        bucket = buckets.get(name)
        if bucket is None:
            continue
        sections.append({
            "name": name,
            "content": "\n\n".join(bucket["parts"]),
            "items": bucket["items"],
            "priority": _SECTION_PRIORITY[name],
            "trust_level": _SECTION_TRUST[name],
            "required": _SECTION_REQUIRED.get(name, False),
        })
    return sections


def render_sections(sections: list[dict]) -> str:
    return "\n\n".join(f"{_SECTION_HEADERS[item['name']]}\n{item['content']}" for item in sections)


def _source_refs(state: dict, sections: list[dict]) -> list[SourceReference]:
    evidence = state.get("evidence") or {}
    joined = "\n".join(item["content"] for item in sections)
    refs: list[SourceReference] = []
    for ident, item in evidence.items():
        if f"evidence:{ident}" not in joined and ident not in joined:
            continue
        refs.append(SourceReference(
            source_type=str((item or {}).get("source_type") or "unknown"),
            source_record_id=str(ident),
            evidence_id=str(ident),
            digest=str((item or {}).get("content_digest") or ""),
        ))
        if len(refs) >= 100:
            break
    return refs


def build_prompt_bundle(node: str, state: dict, budget: int = DEFAULT_BUDGET_TOKENS,
                        reserved_output_tokens: int = 0) -> PromptContextBundle:
    """六区块 PromptContextBundle：动态内容全部由 Pydantic 契约承载。"""
    leaves = build_blocks(node, state)
    fit = assemble(leaves, budget)
    sections = group_sections(leaves, fit.texts)
    event = state.get("event") or {}
    run_id = str(state.get("run_id") or "")
    window = None
    try:
        window = window_from_state(state, None) if state.get("window") else None
    except Exception:
        window = None
    if event and (event.get("occurred_at") or event.get("started_at")):
        scope = scope_from_event(event, run_id, window=window)
    else:
        scope = InvestigationScope(
            run_id=run_id or "run-unknown",
            service_id=str(event.get("service_id") or "unknown"),
            incident_id=event.get("incident_id"),
            trigger_type=str(event.get("event") or "manual"),
            occurred_at=utc_now(),
            observed_at=utc_now(),
            recorded_at=utc_now(),
            time_accuracy="unknown",
            window=window or default_window(utc_now()),
        )
    refs = _source_refs(state, sections)
    blocks = [
        ContextBlock(
            name=item["name"],
            priority=item["priority"],
            trust_level=item["trust_level"],
            required=item["required"],
            content=item["content"],
            source_refs=[ref for ref in refs if ref.evidence_id and ref.evidence_id in item["content"]],
            max_items=100,
        )
        for item in sections
    ]
    return PromptContextBundle(
        run_id=scope.run_id,
        scope=scope,
        blocks=blocks,
        rendered_prompt_sections=render_sections(sections),
        estimated_input_tokens=estimate_tokens(render_sections(sections)),
        reserved_output_tokens=reserved_output_tokens,
        budget_limit=budget,
        truncation_summary=fit.summary(),
        source_refs=refs,
    )


# ------------------------------------------------------------------ 块 ----

def build_blocks(node: str, state: dict) -> list[Block]:
    blocks: list[Block] = [
        Block("system", PIN, [_read("prompts/system.md")], protect=True),
        Block("node_prompt", PIN, [_read(f"prompts/{_PROMPTS[node]}")], protect=True),
        *([Block("mcp_tools", PIN, [
            "Optional evidence tools, only these: " + ", ".join(state["mcp_tools"])
            + ". " + (state.get("mcp_scope") or "")
            + " These sources are configured. Whenever this run has HTTP check data for the service"
            + " (any check sample exists, whatever its status code), query metrics at least once AND logs"
            + " at least once before finish_investigation, even if the checks or the timeline already look"
            + " conclusive; one targeted query per source is enough. Never repeat an identical query. An"
            + " empty or failed answer only proves absence for that source: report it as empty in coverage"
            + " instead of ignoring it."
            + " Start from the configured matcher, for example query"
            + " promql=probe_success{service_id=\"<sid>\"} as an instant query and"
            + " loki_query logql={service_id=\"<sid>\"} for the logs; replace <sid> with the"
            + " service id of this run. If that returns nothing, call metric_metadata or"
            + " label_values(name=\"__name__\") (Prometheus) or label_names (Loki) before giving up,"
            + " and widen the window to the full investigation window."
            + " Do not send credentials.",
        ], protect=True)] if node == "lead" and state.get("mcp_tools") else []),
        *([Block("runbook_tool", PIN, [
            "consult_runbook_agent is available once this run. Call it only when evidence is "
            "insufficient or a runbook is needed. Pass question and hypothesis. Do not pass secrets, logs, or the timeline.",
        ], protect=True)] if node == "lead" and state.get("runbook_available") else []),
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


def build_chat_blocks(state: dict) -> list[Block]:
    """Chat 的叶子块：复用诊断同一套块/层级/降级，六区块仍由 group_sections 归类。"""
    blocks: list[Block] = [
        Block("system", PIN, [_read("prompts/system.md")], protect=True),
        Block("node_prompt", PIN, [_read("prompts/chat.md")], protect=True),
        Block("event", PIN, [_event_block(state.get("event") or {})], protect=True),
    ]
    if state.get("question"):
        blocks.append(Block("question", PIN, ["Engineer's question: " + _clip(state["question"], 1200)],
                            protect=True))
    if state.get("report"):
        blocks.append(Block("report", HIGH, [
            "Report (yours):\n" + _clip(state["report"], 4000),
            "Report (yours, trimmed):\n" + _clip(state["report"], 1500),
        ]))
    blocks.extend(_evidence_blocks(state.get("evidence") or {}))
    if state.get("prior_block"):
        blocks.append(_prior_block(str(state["prior_block"])))
    messages = state.get("messages") or []
    if messages:
        recent = "\n".join(_clip(str(item), 500) for item in messages[-12:])
        shorter = "\n".join(_clip(str(item), 200) for item in messages[-6:])
        blocks.append(Block("messages", MEDIUM, [
            "Conversation so far:\n" + recent,
            "Conversation so far (trimmed):\n" + (shorter or "(none)"),
            "",
        ]))
    return blocks


def build_chat_bundle(state: dict, budget: int = DEFAULT_BUDGET_TOKENS) -> Fit:
    """Chat 的上下文装配：与诊断输出**同一形状**的六区块，而不是另拼一段字符串。"""
    leaves = build_chat_blocks(state)
    fit = assemble(leaves, budget)
    sections = group_sections(leaves, fit.texts)
    text = render_sections(sections)
    return Fit(text=text, tokens=estimate_tokens(text), budget=budget, floor=fit.floor,
               blocks=fit.blocks, texts=[section["content"] for section in sections])


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


def _clip(value, limit: int) -> str:
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    text = str(text).replace("\n", " ")
    return text if len(text) <= limit else text[:limit]


__all__ = ["build_context", "build_context_report", "build_blocks", "build_prompt_bundle",
           "build_chat_blocks", "build_chat_bundle",
           "group_sections", "render_sections", "section_of", "SECTION_ORDER",
           "estimate_tokens", "DEFAULT_BUDGET_TOKENS"]
