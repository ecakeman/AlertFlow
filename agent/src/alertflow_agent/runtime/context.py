import json
from pathlib import Path

_ROOT = Path(__file__).resolve().parents[1]
_PROMPTS = {
    "initial_plan": "planner.md",
    "lead": "lead.md",
    "replan": "replan.md",
    "write_report": "report.md",
    "subagent": "subagent.md",
}


def build_context(node: str, state: dict) -> str:
    """Rebuild the model input for this call. The result is not stored on the run."""
    parts = [
        _read("prompts/system.md"),
        _read(f"prompts/{_PROMPTS[node]}"),
        _read("skills/_semantics.md"),
        _event_block(state.get("event") or {}),
    ]
    if node == "subagent" and state.get("question"):
        parts.append("Untrusted history_review.question: " + json.dumps(state["question"], ensure_ascii=False))
    if node != "subagent" and state.get("plan"):
        parts.append("Plan:\n" + _clip(str(state["plan"]), 2000))
    if node == "write_report" and state.get("prior_report"):
        parts.append("Previous report, read only:\n" + _clip(str(state["prior_report"].get("summary")), 500))
    if node == "write_report":
        parts.append(_catalog_block(state.get("fact_catalog") or []))
        findings = ((state.get("verification") or {}).get("findings")) or []
        if findings and state.get("llm_report"):
            parts.append("Previous structured report:\n" + _clip(json.dumps(state["llm_report"], ensure_ascii=False), 4000))
            parts.append("Verification failures:\n" + json.dumps(findings, ensure_ascii=False))
            parts.append("Fix only these failures. Keep unknowns that are still true. Cite fact_ids from the catalog and evidence ids from the registry.")
    parts.append(_evidence_block(state.get("evidence") or {}))
    for name in state.get("skills_loaded") or []:
        text = (state.get("skill_texts") or {}).get(name) or ""
        if text:
            parts.append(f"Skill {name}:\n{text}")
    messages = state.get("messages") or []
    if messages:
        parts.append("Recent messages:\n" + "\n".join(_clip(str(item), 500) for item in messages[-12:]))
    return "\n\n".join(parts)


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


def _catalog_block(catalog: list[dict]) -> str:
    lines = []
    for row in catalog:
        lines.append(f"{row['fact_id']} {row['evidence_id']} {row['path']} {json.dumps(row['value'], ensure_ascii=False)}")
    return "Fact catalog:\n" + "\n".join(lines)


def _evidence_block(evidence: dict) -> str:
    lines = []
    for ident, item in list(evidence.items())[:40]:
        summary = item.get("summary") or ""
        if item.get("source_type") in {"alertflow_comment", "alertflow_external_alert", "project_diagnostic"}:
            summary = "Untrusted " + str(item.get("source_type")) + ": " + json.dumps(summary, ensure_ascii=False)
        if item.get("kind") == "metric_series":
            summary = _metric_summary(item.get("data") or {})
        limit = 1500 if str(ident).startswith("ana:") or item.get("kind") == "metric_series" else 400
        lines.append(f"[{ident}] {item.get('kind')} @{item.get('observed_at')} — {_clip(summary, limit)}")
    return "Evidence:\n" + "\n".join(lines)


def _metric_summary(data: dict) -> str:
    series = data.get("series") or []
    first = series[0].get("ts") if series and isinstance(series[0], dict) else None
    last = series[-1].get("ts") if series and isinstance(series[-1], dict) else None
    return f"{len(series)} points from {first} to {last}. Latency statistics are in ana:latency."


def _clip(text: str, limit: int) -> str:
    text = text.replace("\n", " ")
    return text if len(text) <= limit else text[:limit]
