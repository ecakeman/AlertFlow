import json
from datetime import datetime, timezone
from pathlib import Path

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now
from alertflow_agent.integrations.project_diagnostics import fetch, load_config
from alertflow_agent.runtime.model import call_model

_SKILLS = Path(__file__).resolve().parents[1] / "skills"
_ALLOWED_SKILLS = {"service-unavailable", "intermittent-failure", "latency-degradation", "tls-evidence"}


def run_tool(name: str, args: dict, state: dict, settings, clients: dict) -> dict:
    args = args or {}
    if name == "finish_investigation":
        state["finished"] = True
        return {"status": "success", "data": {"note": args.get("note", "")}, "evidence_ids": []}
    if name == "request_replan":
        state["replan_requested"] = str(args.get("reason") or "replan")
        return {"status": "success", "data": {"reason": state["replan_requested"]}, "evidence_ids": []}
    try:
        if name == "get_check_history":
            return _history(args, state, settings, clients)
        if name == "analyze_check_timeline":
            return _analyze(args, state)
        if name == "get_metrics_series":
            return _metrics(args, state, settings, clients)
        if name == "get_incident_detail":
            return _incident(args, state, settings, clients)
        if name == "get_project_diagnostics":
            return _diagnostics(args, state, settings)
        if name == "recall_episode":
            return _recall_episode(args, state, clients)
        if name == "load_skill":
            return _skill(args, state)
        if name == "delegate_history_review":
            return _delegate(args, state, settings, clients)
    except Exception:
        return {"status": "error", "note": "tool failed", "evidence_ids": []}
    return {"status": "invalid_request", "note": "unknown tool", "evidence_ids": []}


def _epoch(value) -> int:
    if isinstance(value, (int, float)):
        return int(value)
    return int(datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp())


def _charge_query(state, settings) -> str | None:
    """给「回头找 AlertFlow 要数据」的工具记一次调用，超限就拒。

    默认上下文已经把 Incident、24h 指标、近期事件和默认检查记录下推了，所以这些
    工具是**补充**手段（更长时间范围、更久历史、别的 Incident）。给每次 run 一个
    次数上限，免得模型把预算花在反复翻历史上。返回 None 表示可以继续。
    """
    used = int(state.get("query_calls") or 0)
    if used >= settings.max_query_calls:
        return "query budget exhausted"
    state["query_calls"] = used + 1
    return None


def _history(args, state, settings, clients) -> dict:
    blocked = _charge_query(state, settings)
    if blocked:
        return {"status": "invalid_request", "note": blocked, "evidence_ids": []}
    event = state["event"]
    occurred = _epoch(event["occurred_at"])
    now = int(datetime.now(timezone.utc).timestamp())
    earliest = occurred - 24 * 3600
    start = int(args.get("from") or occurred - 3600)
    end = int(args.get("to") or occurred)
    if start > end:
        return {"status": "invalid_request", "note": "from is after to", "evidence_ids": []}
    start = max(earliest, start)
    end = min(now, end)
    if start > end:
        return {"status": "invalid_request", "note": "window is empty", "evidence_ids": []}
    limit = int(args.get("limit") or 50)
    if limit < 1 or limit > 500:
        return {"status": "invalid_request", "note": "limit", "evidence_ids": []}
    rows = collect_checks(clients["alertflow"], event["service_id"], start, end, min(limit, settings.check_max_rows))
    merged = {row["id"]: row for row in state.get("raw_checks") or []}
    for row in rows:
        merged[row["id"]] = row
    state["raw_checks"] = sorted(merged.values(), key=lambda row: row["ts"])
    ids = _add_checks(state, key_checks(rows))
    return {"status": "success" if rows else "empty", "evidence_ids": ids, "data": {"returned": len(rows)}}


def _analyze(args, state) -> dict:
    minutes = int(args.get("window_minutes") or 0)
    if "around" not in args or minutes < 1 or minutes > 360:
        return {"status": "invalid_request", "note": "window", "evidence_ids": []}
    center = _epoch(args["around"])
    start, end = center - minutes * 60, center + minutes * 60
    rows = [row for row in state.get("raw_checks") or [] if start <= _epoch(row["ts"]) <= end]
    n = int(state.get("analysis_n") or 0) + 1
    state["analysis_n"] = n
    _put_analysis(state, n, rows, start)
    return {"status": "success" if rows else "empty", "evidence_ids": [f"ana:status@{n}"], "data": {"n": n, "rows": len(rows)}}


def _metrics(args, state, settings, clients) -> dict:
    blocked = _charge_query(state, settings)
    if blocked:
        return {"status": "invalid_request", "note": blocked, "evidence_ids": []}
    span = args.get("range") or ""
    if span not in {"1h", "6h", "24h", "7d", "30d"}:
        return {"status": "invalid_request", "note": "range", "evidence_ids": []}
    body = clients["alertflow"].metrics(state["event"]["service_id"], span)
    series = [
        {"ts": point.get("ts"), "avgLatency": point.get("avgLatency")}
        for point in (body.get("series") or [])[:200]
    ]
    values = [point["avgLatency"] for point in series if isinstance(point.get("avgLatency"), (int, float))]
    stats = {
        "range": span,
        "samples": len(series),
        "from": series[0]["ts"] if series else None,
        "to": series[-1]["ts"] if series else None,
        "min_avg_latency": min(values) if values else None,
        "max_avg_latency": max(values) if values else None,
        "last_avg_latency": values[-1] if values else None,
        "preview": series[:20],
    }
    ident = f"met:{span}"
    _add(state, ident, "alertflow_metric", ident, "metric_series", json.dumps(stats), {"series": series, "stats": stats}, replace=True)
    return {"status": "success" if series else "empty", "evidence_ids": [ident], "data": {"points": len(series), "stats": stats}}


def _incident(args, state, settings, clients) -> dict:
    blocked = _charge_query(state, settings)
    if blocked:
        return {"status": "invalid_request", "note": blocked, "evidence_ids": []}
    try:
        ident = int(args.get("incident_id"))
    except (TypeError, ValueError):
        return {"status": "invalid_request", "note": "incident_id", "evidence_ids": []}
    known = f"inc:{ident}"
    if known not in state["evidence"]:
        return {"status": "invalid_request", "note": "incident is not in the registry", "evidence_ids": []}
    detail = clients["alertflow"].incident(ident)
    if detail.get("serviceId") != state["event"]["service_id"]:
        return {"status": "invalid_request", "note": "different service", "evidence_ids": []}
    added = []
    for comment in detail.get("comments") or []:
        cid = f"cmt:{ident}:{comment['id']}"
        if _add(state, cid, "alertflow_comment", cid, "comment", comment.get("body") or "", {"body": comment.get("body")}):
            added.append(cid)
    return {"status": "success", "evidence_ids": added or [known], "data": {"comments": len(detail.get("comments") or [])}}


def _diagnostics(args, state, settings) -> dict:
    calls = int(state.get("diagnostic_calls") or 0)
    if calls >= 2:
        return {"status": "invalid_request", "note": "diagnostic call limit", "evidence_ids": []}
    try:
        since = _epoch(args.get("since"))
        until = _epoch(args.get("until"))
    except (TypeError, ValueError):
        return {"status": "invalid_request", "note": "window", "evidence_ids": []}
    if since >= until or until - since > 6 * 3600:
        return {"status": "invalid_request", "note": "window", "evidence_ids": []}
    cfg = load_config(settings.diagnostics_file)
    if not cfg:
        return {"status": "unavailable", "note": "diagnostics are not configured", "evidence_ids": []}
    state["diagnostic_calls"] = calls + 1
    event_time = datetime.fromisoformat(state["event"]["occurred_at"].replace("Z", "+00:00"))
    result = fetch(
        cfg,
        datetime.fromtimestamp(since, timezone.utc).isoformat(),
        datetime.fromtimestamp(until, timezone.utc).isoformat(),
        event_time,
    )
    ids = []
    for item in result["items"]:
        ident = f"diag:{item['kind']}:{item['name']}"
        if _add(state, ident, "project_diagnostic", ident, item["kind"], item.get("summary") or "", item, item.get("limitations") or [], replace=True):
            ids.append(ident)
    service = result.get("service")
    if service:
        # v2: the target's own metadata (version, env, instance, uptime) is evidence too.
        label = " ".join(str(service.get(key)) for key in ("name", "version") if service.get(key))
        if _add(state, "diag:service", "project_diagnostic", "diag:service", "service_info",
                label or "service", service, [], replace=True):
            ids.append("diag:service")
    status = {"ok": "success", "empty": "empty", "partial": "partial", "stale": "partial"}.get(result["coverage"], "unavailable")
    if result["coverage"] == "invalid":
        status = "error"
    return {"status": status, "evidence_ids": ids, "data": {"coverage": result["coverage"], "dropped": result["dropped"]}}


def _skill(args, state) -> dict:
    name = args.get("name")
    if name not in _ALLOWED_SKILLS:
        return {"status": "invalid_request", "note": "unknown skill", "evidence_ids": []}
    text = (_SKILLS / name / "SKILL.md").read_text(encoding="utf-8")
    state.setdefault("skills_loaded", [])
    state.setdefault("skill_texts", {})
    if name not in state["skills_loaded"]:
        state["skills_loaded"].append(name)
    state["skill_texts"][name] = text
    return {"status": "success", "evidence_ids": [], "data": {"name": name}}


def _delegate(args, state, settings, clients) -> dict:
    incidents = [key for key, item in state["evidence"].items() if item["source_type"] == "alertflow_incident"]
    wanted = args.get("incident_ids") or []
    if len(incidents) < 2 or not wanted or len(wanted) > 5 or any(item not in state["evidence"] for item in wanted):
        return {"status": "invalid_request", "note": "history_review preconditions", "evidence_ids": []}
    if state["guard"]["subagent_runs"] >= settings.max_subagent_runs:
        return {"status": "invalid_request", "note": "subagent budget", "evidence_ids": []}
    child_evidence = {ident: state["evidence"][ident] for ident in wanted}
    child = {
        "run_id": state["run_id"],
        "event": {
            "event": state["event"].get("event"),
            "service_id": state["event"].get("service_id"),
            "occurred_at": state["event"].get("occurred_at"),
            "status": state["event"].get("status"),
        },
        "evidence": child_evidence,
        "messages": [],
        "guard": state["guard"],
        "coverage": {},
        "skills_loaded": [],
        "question": args.get("question") or "",
    }
    state["guard"]["subagent_runs"] += 1
    rounds = 0
    review = None
    while rounds < settings.subagent_max_rounds:
        review = call_model(clients["model"], "subagent", child, settings, clients["store"])
        rounds += 1
        if not review:
            return {"status": "error", "note": "subagent failed", "evidence_ids": []}
        if review.get("tool") != "get_incident_detail":
            break
        _incident({"incident_id": review.get("incident_id")}, child, settings, clients)
    n = state["guard"]["subagent_runs"]
    ident = f"rev:{n}"
    _add(state, ident, "subagent_review", ident, "history_review", review.get("suggestion") or "review", review, ["LLM summary, cite the underlying evidence"])
    return {"status": "success", "evidence_ids": [ident], "data": {"status": review.get("status")}}


def _recall_episode(args, state, clients) -> dict:
    """展开一条**已在既往上下文里出现过**的历史事件。

    只允许展开被展示过的（在 state['prior']['hits'] 里）——边界清晰：模型不能凭一个
    id 去翻任意历史，只能就"眼前这个问题"放大看当时那份判断。
    """
    raw = str(args.get("episode_id") or "").strip()
    ident = raw if raw.startswith("ep:") else f"ep:{raw}"
    prior = state.get("prior") or {}
    allowed = {f"ep:{hit.get('id')}" for hit in (prior.get("hits") or [])}
    if ident not in allowed:
        return {"status": "invalid_request", "note": "episode not in prior context", "evidence_ids": []}
    repo = clients.get("memory")
    if repo is None:
        return {"status": "unavailable", "note": "memory is not configured", "evidence_ids": []}
    episode = repo.get_episode(ident[3:])
    if not episode:
        return {"status": "empty", "note": "episode not found", "evidence_ids": []}
    _add(state, ident, "episodic_memory", ident, "episode", episode.get("card") or "", episode,
         ["historical conclusion — verify against current evidence"])
    return {"status": "success", "evidence_ids": [ident], "data": {"episode_id": ident}}


def _add_checks(state, rows) -> list[str]:
    ids = []
    for row in rows:
        ident = f"chk:{row['id']}"
        if _add(state, ident, "alertflow_check", f"check:{row['id']}", "http_check", row.get("error") or row.get("status"), row):
            ids.append(ident)
    return ids


def _add(state, ident, source, ref, kind, summary, data, limitations=None, replace: bool = False) -> bool:
    if ident in state["evidence"] and not replace:
        return False
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
    return True


def _put_analysis(state, n: int, rows=None, window_from=None) -> None:
    rows = state.get("raw_checks") or [] if rows is None else rows
    series = state.get("series") or []
    window_from = state["coverage"].get("from") or 0 if window_from is None else window_from
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
