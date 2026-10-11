import json
from datetime import datetime, timezone

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now, normalize_check


def run_tool(name: str, args: dict, state: dict, settings, clients: dict) -> dict:
    if name == "finish_investigation":
        state["finished"] = True
        return {"status": "success", "data": {"note": args.get("note", "")}, "evidence_ids": []}
    if name == "request_replan":
        state["replan_requested"] = args.get("reason") or "replan"
        return {"status": "success", "data": {"reason": state["replan_requested"]}, "evidence_ids": []}
    if name == "get_check_history":
        return _history(args, state, settings, clients)
    if name == "analyze_check_timeline":
        return _analyze(state)
    if name == "get_metrics_series":
        return _metrics(args, state, clients)
    if name == "get_incident_detail":
        return _incident(args, state, clients)
    if name == "get_project_diagnostics":
        return {"status": "unavailable", "note": "diagnostics are collected before the lead loop", "evidence_ids": []}
    if name == "load_skill":
        return _skill(args, state)
    if name == "delegate_history_review":
        return _delegate(args, state, settings, clients)
    return {"status": "invalid_request", "note": "unknown tool", "evidence_ids": []}


def _history(args, state, settings, clients) -> dict:
    event = state["event"]
    occurred = int(datetime.fromisoformat(event["occurred_at"].replace("Z", "+00:00")).timestamp())
    start = max(occurred - 24 * 3600, int(args.get("from") or occurred - 3600))
    end = min(int(datetime.now(timezone.utc).timestamp()), int(args.get("to") or occurred))
    limit = min(int(args.get("limit") or 50), 500)
    rows = collect_checks(clients["alertflow"], event["service_id"], start, end, min(limit, settings.check_max_rows))
    state["raw_checks"] = rows
    ids = _add_checks(state, key_checks(rows))
    return {"status": "success" if rows else "empty", "evidence_ids": ids, "data": {"returned": len(rows)}}


def _analyze(state) -> dict:
    n = state.get("analysis_n", 0) + 1
    state["analysis_n"] = n
    _put_analysis(state, n)
    return {"status": "success", "evidence_ids": [f"ana:status@{n}"], "data": {"n": n}}


def _metrics(args, state, clients) -> dict:
    allowed = {"1h", "6h", "24h", "7d", "30d"}
    span = args.get("range") or "24h"
    if span not in allowed:
        return {"status": "invalid_request", "note": "range", "evidence_ids": []}
    body = clients["alertflow"].metrics(state["event"]["service_id"], span)
    ident = f"met:{span}"
    _add(state, ident, "alertflow_metric", ident, "metric_series", f"metrics {span}", {"points": len(body.get("series") or [])})
    return {"status": "success", "evidence_ids": [ident], "data": {"points": len(body.get("series") or [])}}


def _incident(args, state, clients) -> dict:
    ident = int(args.get("incident_id") or 0)
    known = f"inc:{ident}"
    if known not in state["evidence"]:
        return {"status": "invalid_request", "note": "incident is not in the registry", "evidence_ids": []}
    detail = clients["alertflow"].incident(ident)
    if detail.get("serviceId") != state["event"]["service_id"]:
        return {"status": "invalid_request", "note": "different service", "evidence_ids": []}
    return {"status": "success", "evidence_ids": [known], "data": {"comments": len(detail.get("comments") or [])}}


def _skill(args, state) -> dict:
    name = args.get("name")
    if name not in {"service-unavailable", "intermittent-failure", "latency-degradation", "tls-evidence"}:
        return {"status": "invalid_request", "note": "unknown skill", "evidence_ids": []}
    state.setdefault("skills_loaded", [])
    if name not in state["skills_loaded"]:
        state["skills_loaded"].append(name)
    return {"status": "success", "evidence_ids": [], "data": {"name": name}}


def _delegate(args, state, settings, clients) -> dict:
    incidents = [k for k, v in state["evidence"].items() if v["source_type"] == "alertflow_incident"]
    wanted = args.get("incident_ids") or []
    if len(incidents) < 2 or len(wanted) > 5 or any(i not in state["evidence"] for i in wanted):
        return {"status": "invalid_request", "note": "history_review preconditions", "evidence_ids": []}
    if state["guard"]["subagent_runs"] >= settings.max_subagent_runs:
        return {"status": "invalid_request", "note": "subagent budget", "evidence_ids": []}
    state["guard"]["subagent_runs"] += 1
    review = clients["model"].complete("subagent", state)
    n = state["guard"]["subagent_runs"]
    ident = f"rev:{n}"
    _add(state, ident, "subagent_review", ident, "history_review", review.get("suggestion") or "review", review, ["LLM 汇总，引用底层证据"])
    return {"status": "success", "evidence_ids": [ident], "data": {"status": review.get("status")}}


def _add_checks(state, rows) -> list[str]:
    ids = []
    for row in rows:
        ident = f"chk:{row['id']}"
        _add(state, ident, "alertflow_check", f"check:{row['id']}", "http_check", row.get("error") or row.get("status"), row)
        ids.append(ident)
    return ids


def _add(state, ident, source, ref, kind, summary, data, limitations=None) -> None:
    if ident in state["evidence"]:
        return
    state["evidence"][ident] = {
        "id": ident,
        "source_type": source,
        "source_ref": ref,
        "service_id": state["event"]["service_id"],
        "observed_at": state["event"]["occurred_at"],
        "collected_at": iso_now(),
        "kind": kind,
        "summary": (summary or "")[:400],
        "data": data,
        "limitations": limitations or [],
    }


def _put_analysis(state, n: int) -> None:
    rows = state.get("raw_checks") or []
    series = state.get("series") or []
    window_from = state["coverage"].get("from") or 0
    payloads = {
        f"ana:status@{n}": status_counts(rows),
        f"ana:segments@{n}": segments(rows),
        f"ana:latency@{n}": latency_stats(rows, series, window_from),
        f"ana:errors@{n}": error_stats(rows, classify_error),
        f"ana:coverage@{n}": coverage(rows, window_from, state["coverage"].get("to") or window_from, state.get("interval")),
    }
    for ident, data in payloads.items():
        summary = json.dumps(data)[:1500]
        state["evidence"][ident] = {
            "id": ident, "source_type": "analysis", "source_ref": ident,
            "service_id": state["event"]["service_id"], "observed_at": state["event"]["occurred_at"],
            "collected_at": iso_now(), "kind": "analysis", "summary": summary, "data": data, "limitations": [],
        }


def clip(result: dict) -> dict:
    text = json.dumps(result)
    if len(text) <= 4000:
        return result
    return {"status": result.get("status"), "note": "truncated", "evidence_ids": result.get("evidence_ids") or []}
