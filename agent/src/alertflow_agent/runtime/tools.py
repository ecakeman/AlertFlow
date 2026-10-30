import hashlib
import json
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now
from alertflow_agent.integrations.mcp import (
    EVIDENCE_TOOLS,
    LOKI_TOOLS,
    MCPError,
    PROMETHEUS_TOOLS,
    SCOPED_TOOLS,
    clamp_window,
    _IDENT,
    _mask_promql,
    fit_promql_window,
    scope_logql,
    scope_promql,
    selector_for,
    summarize_result,
)
from alertflow_agent.integrations.project_diagnostics import fetch, load_config
from alertflow_agent.integrations.runbook_contract import (
    ARTIFACT_SCHEMA,
    InvalidArtifact,
    RunbookFactHint,
    build_query,
    parse_artifact,
    query_payload,
)
from alertflow_agent.runtime.model import call_model
from alertflow_agent.runtime.window import (
    parse_time,
    scope_from_event,
    window_between,
    window_from_state,
)
from alertflow_agent.schemas.context import (
    AlertFlowQueryRequest,
    LokiQueryRequest,
    PrometheusQueryRequest,
    RetrievalRequest,
)

_SKILLS = Path(__file__).resolve().parents[1] / "skills"
_ALLOWED_SKILLS = {"service-unavailable", "intermittent-failure", "latency-degradation", "tls-evidence"}


def run_tool(name: str, args: dict, state: dict, settings, clients: dict) -> dict:
    args = args or {}
    if name == "finish_investigation":
        # 数据源可用、故障又属于 HTTP/运行时类时，没查过的源必须显式记为 empty，
        # 不能让模型“静默跳过”后报告看起来像查过。这里不强制调用次数，只记事实。
        _note_unqueried_sources(state, settings)
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
        if name == "query_service_timeline":
            return _timeline(args, state, settings, clients)
        if name == "recall_episode":
            return _recall_episode(args, state, clients)
        if name == "load_skill":
            return _skill(args, state)
        if name == "delegate_history_review":
            return _delegate(args, state, settings, clients)
        if name in PROMETHEUS_TOOLS or name in LOKI_TOOLS:
            return _mcp(name, args, state, settings, clients)
        if name == "consult_runbook_agent":
            return _consult_runbook(args, state, settings, clients)
    except Exception:
        return {"status": "error", "note": "tool failed", "evidence_ids": []}
    return {"status": "invalid_request", "note": "unknown tool", "evidence_ids": []}


def _epoch(value) -> int:
    if isinstance(value, (int, float)):
        return int(value)
    return int(datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp())


def _step_seconds(value) -> int:
    try:
        return max(1, int(str(value).rstrip("s")))
    except (TypeError, ValueError):
        return 60


def _optional_int(value):
    try:
        return int(value) if value is not None else None
    except (TypeError, ValueError):
        return None


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


_FAILED_STATUSES = {"failed", "unavailable", "timeout", "disabled", "invalid_request"}


def _manager(clients, settings):
    """取进程内唯一的 ContextManager；工具层不直接访问任何检索 adapter。"""
    manager = clients.get("context")
    if manager is None:
        from alertflow_agent.runtime.context_manager import ContextManager

        manager = ContextManager(
            alertflow=clients.get("alertflow"), mcp=clients.get("mcp"),
            memory=clients.get("memory"), settings=settings,
        )
    return manager


def _scope_for(state, settings, start, end, *, kind="root_cause"):
    """工具子窗口仍派生自父级 InvestigationWindow（唯一时间来源）。"""
    base = window_from_state(state, settings)
    win = window_between(base, int(start), int(end), kind=kind)
    return scope_from_event(
        state.get("event") or {}, str(state.get("run_id") or "run"),
        window=win, settings=settings,
    )


def _retrieve(state, settings, clients, scope, sources, *, purpose="targeted_followup"):
    """唯一检索入口；失败返回 None 由调用方映射成本源的 gap/note，不抛出。"""
    try:
        request = RetrievalRequest(
            request_id=f"tool:{scope.run_id}:{time.time_ns()}",
            scope=scope, purpose=purpose, source_requests=list(sources),
        )
        return _manager(clients, settings).retrieve_now(request)
    except Exception as exc:  # 单次检索失败不得打断整个工具调用
        state.setdefault("retrieval_errors", []).append(str(exc))
        return None


def _first(batch, source_type):
    if batch is None:
        return None
    return (batch.by_source(source_type) or [None])[0]


def _history(args, state, settings, clients) -> dict:
    blocked = _charge_query(state, settings)
    if blocked:
        return {"status": "invalid_request", "note": blocked, "evidence_ids": []}
    event = state["event"]
    # 窗口只认父级 state["window"]：不再用 occurred-24h 自己推导
    window = window_from_state(state, settings)
    now = int(datetime.now(timezone.utc).timestamp())
    earliest = window.start_epoch()
    start = int(args.get("from") or window.start_epoch())
    end = int(args.get("to") or window.end_epoch())
    if start > end:
        return {"status": "invalid_request", "note": "from is after to", "evidence_ids": []}
    start = max(earliest, start)
    end = min(now, end)
    if start > end:
        return {"status": "invalid_request", "note": "window is empty", "evidence_ids": []}
    limit = int(args.get("limit") or 50)
    if limit < 1 or limit > 500:
        return {"status": "invalid_request", "note": "limit", "evidence_ids": []}
    scope = _scope_for(state, settings, start, end)
    incident_id = _optional_int(event.get("incident_id"))
    source = AlertFlowQueryRequest(
        request_id=f"checks:{scope.run_id}:{start}:{end}",
        run_id=scope.run_id, service_id=event["service_id"], incident_id=incident_id,
        resources=["checks"],
        window_start=datetime.fromtimestamp(start, timezone.utc),
        window_end=datetime.fromtimestamp(end, timezone.utc),
        page_size=min(limit, settings.check_max_rows),
    )
    result = _first(_retrieve(state, settings, clients, scope, [source]), "checks")
    if result is None:
        return {"status": "error", "note": "checks retrieval failed", "evidence_ids": []}
    if result.status in _FAILED_STATUSES:
        return {"status": "error", "note": f"checks retrieval {result.status}", "evidence_ids": []}
    rows = [item.data for item in result.items if isinstance(item.data, dict)]
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


def _timeline(args, state, settings, clients) -> dict:
    """Read application, kubernetes, incident and agent events from Go.

    所有跨度都经 ``ContextManager.retrieve`` 走统一入口；窗口从 1h 扩到 24h/retention
    只是显式的 historical_recall 探测，不改变父级 InvestigationWindow 的定义。
    """
    if getattr(settings, "agent_diagnostics_mode", "timeline") == "legacy":
        return {"status": "unavailable", "note": "timeline mode is off", "evidence_ids": []}
    blocked = _charge_query(state, settings)
    if blocked:
        return {"status": "invalid_request", "note": blocked, "evidence_ids": []}
    event = state["event"]
    window = window_from_state(state, settings)
    end = window.end_epoch()
    retention_days = int(args.get("retention_days") or 30)
    windows = [max(1, window.duration_seconds), 24 * 3600, retention_days * 24 * 3600]
    sources = args.get("sources") or ["application_diagnostics", "kubernetes", "incident", "agent"]
    limit = int(args.get("limit") or 50)
    if limit < 1 or limit > 200:
        return {"status": "invalid_request", "note": "limit", "evidence_ids": []}
    page = {"events": [], "coverage": []}
    for span in windows:
        kind = "root_cause" if span == windows[0] else "historical_recall"
        if span > window.duration_seconds:
            state.setdefault("window_expansions", []).append(
                {"kind": "historical_recall", "start": end - span, "end": end, "seconds": span}
            )
        start = end - span
        scope = _scope_for(state, settings, start, end, kind=kind)
        source = AlertFlowQueryRequest(
            request_id=f"timeline:{scope.run_id}:{start}:{end}",
            run_id=scope.run_id, service_id=event["service_id"],
            incident_id=_optional_int(event.get("incident_id")),
            resources=["service_timeline"],
            window_start=datetime.fromtimestamp(start, timezone.utc),
            window_end=datetime.fromtimestamp(end, timezone.utc),
            page_size=limit,
        )
        result = _first(_retrieve(state, settings, clients, scope, [source],
                                  purpose="historical_recall" if kind == "historical_recall" else "targeted_followup"), "timeline")
        if result is None:
            return {"status": "error", "note": "timeline retrieval failed", "evidence_ids": []}
        if result.status in _FAILED_STATUSES:
            return {"status": "error", "note": f"timeline retrieval {result.status}", "evidence_ids": []}
        events = [item.data for item in result.items if isinstance(item.data, dict)]
        page = {
            "events": events,
            "coverage": (result.query_metadata or {}).get("coverage") or [],
            "from": start, "to": end,
        }
        if events:
            break
        if int(state.get("query_calls") or 0) >= settings.max_query_calls:
            break
        state["query_calls"] = int(state.get("query_calls") or 0) + 1
    ids = []
    for item in page.get("events") or []:
        ident = item.get("eventKey") or f"tl:{item.get('id')}"
        summary = item.get("summary") or item.get("title") or item.get("eventType") or ""
        data = {
            "source": item.get("source"),
            "event_type": item.get("eventType"),
            "occurred_at": item.get("occurredAt"),
            "observed_at": item.get("observedAt"),
            "recorded_at": item.get("recordedAt"),
            "cluster_id": item.get("clusterId"),
            "namespace": item.get("namespace"),
            "resource_kind": item.get("resourceKind"),
            "resource_name": item.get("resourceName"),
            "resource_uid": item.get("resourceUid"),
            "details": item.get("details") or {},
            "query_from": page.get("from"),
            "query_to": page.get("to"),
        }
        if _add(state, ident, item.get("source") or "timeline", ident, item.get("eventType") or "event", summary, data):
            ids.append(ident)
    coverage_rows = page.get("coverage") or []
    if coverage_rows and _add(state, "tl:coverage", "timeline", "tl:coverage", "coverage", "source coverage", {"sources": coverage_rows}):
        ids.append("tl:coverage")
    if not page.get("events"):
        state.setdefault("coverage", {}).setdefault("gaps", []).append("TIMELINE_EMPTY")
        return {"status": "empty", "evidence_ids": ids, "data": {"coverage": coverage_rows}}
    return {"status": "success", "evidence_ids": ids, "data": {"events": len(page.get("events") or [])}}

def _diagnostics(args, state, settings) -> dict:
    if getattr(settings, "agent_diagnostics_mode", "timeline") != "legacy":
        return {"status": "unavailable", "note": "diagnostics mode is timeline", "evidence_ids": []}
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


def _mcp(name, args, state, settings, clients) -> dict:
    """配置了 URL 才调用。失败只记本源的 coverage gap，不抛出。

    参数整形留在工具层；真正的 MCP tools/call 只发生在 ContextManager.retrieve 内部。
    """
    configured = (settings.prometheus_mcp_url if name in PROMETHEUS_TOOLS else settings.loki_mcp_url) or ""
    if not str(configured).strip():
        return {"status": "invalid_request", "note": "unknown tool", "evidence_ids": []}
    source = "prometheus" if name in PROMETHEUS_TOOLS else "loki"
    try:
        sent, recorded = _mcp_request(name, args, state, settings, source)
    except MCPError as exc:
        _mcp_gap(state, source, exc.kind)
        _note_mcp_activity(state, source, "unavailable", error=f"{source} {exc.kind}")
        return {"status": "error", "note": _mcp_note(source, exc.kind), "evidence_ids": []}
    if sent is None:
        return {"status": "invalid_request", "note": "unknown tool", "evidence_ids": []}
    if int(state.get("mcp_calls") or 0) >= int(settings.mcp_max_calls):
        _mcp_gap(state, source, "budget")
        _note_mcp_activity(state, source, "unavailable", error=f"{source} budget")
        return {"status": "error", "note": _mcp_note(source, "budget"), "evidence_ids": []}
    encoded = json.dumps(sent, sort_keys=True, default=str).encode()
    if len(encoded) > int(settings.mcp_max_response_bytes):
        _mcp_gap(state, source, "too_large")
        _note_mcp_activity(state, source, "unavailable", error=f"{source} too_large")
        return {"status": "error", "note": _mcp_note(source, "too_large"), "evidence_ids": []}
    # 调用前记账：超时/失败同样消耗额度（§7.7.12）
    state["mcp_calls"] = int(state.get("mcp_calls") or 0) + 1
    _note_mcp_activity(state, source, counted=True)
    # instant query 的 recorded 只有 timestamp，没有起止；用 1 秒窗口近似，仍派生自父级窗口
    end_dt = parse_time(recorded.get("end") or recorded.get("timestamp"))
    start_dt = parse_time(recorded.get("start")) or (
        end_dt - timedelta(seconds=1) if end_dt is not None else None
    )
    if start_dt is None or end_dt is None:
        _mcp_gap(state, source, "malformed")
        _note_mcp_activity(state, source, "unavailable", error=f"{source} malformed")
        return {"status": "error", "note": _mcp_note(source, "malformed"), "evidence_ids": []}
    start, end = int(start_dt.timestamp()), int(end_dt.timestamp())
    scope = _scope_for(state, settings, start, end)
    service_id = (state.get("event") or {}).get("service_id") or ""
    cap = int(settings.mcp_max_log_lines if source == "loki" else settings.mcp_max_samples)
    if source == "prometheus":
        request = PrometheusQueryRequest(
            request_id=f"mcp:{name}:{scope.run_id}:{start}:{end}",
            service_id=service_id, query_mode="instant" if name == "query" else "range",
            promql=str(recorded.get("query") or ""), selector={}, start=start_dt, end=end_dt,
            step_seconds=_step_seconds(sent.get("step")), max_samples=cap,
            timeout_seconds=int(settings.mcp_timeout_seconds), tool=name, tool_args=sent,
        )
    else:
        request = LokiQueryRequest(
            request_id=f"mcp:{name}:{scope.run_id}:{start}:{end}",
            service_id=service_id, selector={}, logql=str(recorded.get("query") or ""),
            start=start_dt, end=end_dt, limit=int(recorded.get("limit") or cap),
            timeout_seconds=int(settings.mcp_timeout_seconds), tool=name, tool_args=sent,
        )
    result = _first(_retrieve(state, settings, clients, scope, [request],
                              purpose="historical_recall" if source == "loki" else "targeted_followup"), source)
    if result is None:
        _note_mcp_activity(state, source, "unavailable")
        return {"status": "error", "note": "retrieval failed", "evidence_ids": []}
    if result.status in _FAILED_STATUSES:
        meta = result.query_metadata or {}
        kind = str(meta.get("mcp_kind") or (result.error.category if result.error else "") or "malformed")
        # error 只进 coverage 细节，不改变既有的 gap code 列表（便于真机排查）
        detail = str((result.error.message if result.error else "") or "")[:200]
        _mcp_gap(state, source, kind)
        _note_mcp_activity(state, source, "unavailable", error=detail or f"{source} {kind}")
        return {"status": "error", "note": _mcp_note(source, kind), "evidence_ids": []}
    text = _scrub(str((result.query_metadata or {}).get("text") or ""), settings)
    if name not in EVIDENCE_TOOLS:
        _note_mcp_activity(state, source, "ok")
        return {
            "status": "success",
            "evidence_ids": [],
            "data": {"note": "not stored as service evidence", "text": text[:400]},
        }
    limit = settings.mcp_max_log_lines if source == "loki" else settings.mcp_max_samples
    try:
        summary = summarize_result(text, source, limit)
    except MCPError as exc:
        # empty 与 unavailable 必须区分：空结果是“该来源没数据”，不是“来源不可用”；
        # 上游 API 返回 error 时把错误文本带进 coverage，便于真机排查。
        _mcp_gap(state, source, exc.kind)
        detail = str(getattr(exc, "message", "") or "")
        _note_mcp_activity(
            state, source, "empty" if exc.kind == "empty" else "unavailable",
            error=None if exc.kind == "empty" else (detail or f"{source} {exc.kind}"),
        )
        return {"status": "error", "note": _mcp_note(source, exc.kind), "evidence_ids": []}
    truncated = summary.pop("truncated", False)
    _note_mcp_activity(state, source, "truncated" if truncated else "ok")
    # 实际发给上游的 selector/query：登记层用它证明候选带精确 selector（§7.7.6）
    selector_text = str(sent.get("query") or "")
    if not selector_text and isinstance(sent.get("matches"), list) and sent["matches"]:
        selector_text = str(sent["matches"][0])
    data = {
        "source": source,
        "query": _scrub(str(recorded.get("query") or ""), settings),
        "selector": _scrub(selector_text, settings),
        "limit": recorded.get("limit") or limit,
        **summary,
    }
    for key in ("start", "end", "timestamp", "query_type", "lookback_seconds", "budget_seconds", "window_truncated"):
        if recorded.get(key) is not None:
            data[key] = recorded[key]
    limitations = ["truncated to the configured row cap"] if truncated else []
    if source == "prometheus" and _prom_metric(str(recorded.get("query") or "")) == "up":
        limitations.append("up is Prometheus scrape success, not API health")
    digest = hashlib.sha256(json.dumps(sent, sort_keys=True, default=str).encode()).hexdigest()[:8]
    ident = f"{source}:{name}:{digest}"
    value = data.get("value") or ""
    _add(state, ident, source, name, name, f"{source} {data['query']} => {value}", data, limitations)
    return {"status": "success", "evidence_ids": [ident], "data": {"text": str(value)[:400]}}

def _mcp_request(name, args, state, settings, source):
    """返回发给上游的参数，以及写入 evidence 的时间窗。

    Prometheus 字段对齐 v0.18.0：时间为字符串，截断用 truncation_limit，
    label_values 的服务选择器放在 matches 数组里。
    """
    service_id = (state.get("event") or {}).get("service_id") or ""
    if name in SCOPED_TOOLS:
        raw = settings.prometheus_service_selectors if source == "prometheus" else settings.loki_service_selectors
        selector = selector_for(raw, service_id)
        if not selector:
            raise MCPError("unmapped")
    else:
        selector = ""
    # 时间窗来自父级 state["window"]；用户显式传 start/end 时仍受 MCP 窗口上限约束
    window = window_from_state(state, settings)
    occurred = window.end_epoch()
    start, end = clamp_window(
        occurred,
        _optional_epoch(args.get("start", args.get("from"))) or window.start_epoch(),
        _optional_epoch(args.get("end", args.get("to", args.get("time")))) or window.end_epoch(),
        settings.mcp_max_window_seconds,
    )
    start_text = _rfc3339(start)
    end_text = _rfc3339(end)
    cap = int(settings.mcp_max_samples if source == "prometheus" else settings.mcp_max_log_lines)
    recorded = {"query": "", "start": start_text, "end": end_text, "limit": cap}
    if name in {"label_names", "metric_metadata"}:
        sent = {}
        if name == "metric_metadata":
            sent["metric"] = str(args.get("metric") or args.get("name") or "")
        return sent, recorded
    if name == "label_values":
        match = selector if selector.startswith("{") else "{" + selector + "}"
        recorded["query"] = match
        return {
            "label": str(args.get("label") or "job"),
            "matches": [match],
            "start_time": start_text,
            "end_time": end_text,
            "truncation_limit": cap,
        }, recorded
    query = str(args.get("query") or "")
    budget = int(settings.mcp_max_window_seconds)
    if source == "prometheus" and name in {"query", "range_query"}:
        fitted = fit_promql_window(query, occurred, start, end, budget, name == "query")
        if fitted is None:
            raise MCPError("unscoped")
        start, end, lookback, truncated = fitted
        start_text = _rfc3339(start)
        end_text = _rfc3339(end)
        recorded["lookback_seconds"] = lookback
        recorded["budget_seconds"] = budget
        recorded["window_truncated"] = truncated
    scoped = (
        scope_promql(query, selector, budget)
        if source == "prometheus"
        else scope_logql(query, selector)
    )
    if not scoped:
        raise MCPError("unscoped")
    recorded["query"] = scoped
    if name == "query":
        recorded = {
            "query": scoped,
            "timestamp": end_text,
            "query_type": "instant",
            "limit": cap,
            "lookback_seconds": recorded.get("lookback_seconds", 0),
            "budget_seconds": budget,
            "window_truncated": False,
        }
        return {"query": scoped, "timestamp": end_text, "truncation_limit": cap}, recorded
    if name == "loki_query":
        requested = args.get("limit")
        try:
            requested = int(requested) if requested is not None else cap
        except (TypeError, ValueError):
            requested = cap
        limit = max(1, min(requested, cap))
        recorded["limit"] = limit
        return {"query": scoped, "start": start_text, "end": end_text, "limit": limit}, recorded
    return {
        "query": scoped,
        "start_time": start_text,
        "end_time": end_text,
        "step": _prom_step(start, end, cap),
        "truncation_limit": cap,
    }, {**recorded, "start": start_text, "end": end_text}


def _prom_metric(query: str) -> str:
    """指标名 up 才返回 up。引号残缺时无法判断，同样返回 up。"""
    text = query or ""
    if text.count('"') % 2:
        return "up"
    masked = _mask_promql(text)
    for match in _IDENT.finditer(masked):
        if match.group(0) != "up":
            continue
        rest = masked[match.end():].lstrip()
        if rest.startswith("=~") or rest.startswith("="):
            continue
        return "up"
    return ""


def _rfc3339(epoch: int) -> str:
    return datetime.fromtimestamp(int(epoch), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _prom_step(start: int, end: int, samples: int) -> str:
    span = max(int(end) - int(start), 1)
    seconds = max(1, (span + max(int(samples), 1) - 1) // max(int(samples), 1))
    return f"{seconds}s"


def _optional_epoch(value):
    if value is None or value == "":
        return None
    try:
        return _epoch(value)
    except (TypeError, ValueError):
        return None


def _scrub(text: str, settings) -> str:
    for token in (getattr(settings, "prometheus_mcp_token", ""), getattr(settings, "loki_mcp_token", "")):
        if token:
            text = text.replace(token, "[REDACTED]")
    return text


def _mcp_gap(state, source: str, kind: str) -> None:
    code = ("PROMETHEUS_" if source == "prometheus" else "LOKI_") + kind.upper()
    gaps = state.setdefault("coverage", {}).setdefault("gaps", [])
    if code not in gaps:
        gaps.append(code)


_MCP_NOTES = {
    "timeout": "{source} timed out; this source has no evidence",
    "disconnect": "{source} connection failed; this source has no evidence",
    "auth": "{source} rejected the request; this source has no evidence",
    "rate_limit": "{source} was rate limited; this source has no evidence",
    "empty": "{source} returned no rows; this source has no evidence",
    "malformed": "{source} response could not be read; this source has no evidence",
    "too_large": "{source} response exceeded the size limit; this source has no evidence",
    "missing_tool": "{source} does not provide this tool; this source has no evidence",
    "unmapped": "{source} has no service selector; this source has no evidence",
    "unscoped": "{source} query could not be scoped; this source has no evidence",
    "budget": "{source} call budget is exhausted; this source has no evidence",
    "upstream": "{source} returned an error; this source has no evidence",
}


def _mcp_note(source: str, kind: str) -> str:
    return _MCP_NOTES.get(kind, "{source} response could not be read; this source has no evidence").format(source=source)


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


def _consult_runbook(args, state, settings, clients) -> dict:
    """一次 consult：请求是 RunbookQueryV1，响应按 ArtifactV1 解析。

    失败只记 coverage gap，不打断当前调查；FAILED/CANCELED 的 artifact
    即使带建议文本也不能登记为成功证据。
    """
    url = str(getattr(settings, "a2a_adapter_url", "") or "").strip()
    if not url:
        return {"status": "invalid_request", "note": "runbook agent is not configured", "evidence_ids": []}
    if state.get("runbook_consulted"):
        return {"status": "error", "note": "runbook already consulted", "evidence_ids": []}
    state["runbook_consulted"] = True
    event = state.get("event") or {}
    window = window_from_state(state, settings)
    facts = []
    for entry in list(state.get("fact_catalog") or [])[:8]:
        if not isinstance(entry, dict):
            continue
        value = entry.get("value")
        facts.append(
            RunbookFactHint(
                fact_id=str(entry.get("fact_id") or ""),
                evidence_id=str(entry.get("evidence_id") or ""),
                summary=f"{entry.get('path')} = {value}"[:200],
                value=str(value)[:200],
            )
        )
    question = str(args.get("question") or "").strip()[:500]
    if not question:
        question = "What does the runbook say about this incident?"
    run_id = str(state.get("run_id") or "")
    service_id = str(event.get("service_id") or "")
    incident_id = event.get("incident_id")
    query = build_query(
        request_id=f"runbook:{run_id or service_id}",
        run_id=run_id,
        service_id=service_id,
        question=question,
        window_start=window.window_start.isoformat(),
        window_end=window.window_end.isoformat(),
        incident_id=str(incident_id) if incident_id not in (None, "") else None,
        symptom_summary=str(event.get("title") or event.get("message") or event.get("error") or "")[:500],
        known_facts=facts,
    )
    payload = query_payload(query)
    message_id = f"runbook:{run_id or service_id}"
    try:
        sender = clients.get("a2a")
        if sender is None:
            from alertflow_agent.integrations.runbook import consult
            artifact = consult(
                url,
                payload,
                message_id,
                timeout=float(getattr(settings, "a2a_adapter_timeout_seconds", 30.0) or 30.0),
                token=str(getattr(settings, "a2a_adapter_token", "") or ""),
            )
        else:
            artifact = sender(url, payload, message_id)
    except Exception as exc:
        if "invalid" in str(exc).lower():
            _gap(state, "runbook artifact is invalid")
            return {"status": "error", "note": "runbook artifact is invalid", "evidence_ids": []}
        _gap(state, "runbook agent did not answer")
        return {"status": "error", "note": "runbook lookup failed", "evidence_ids": []}
    try:
        parsed = parse_artifact(artifact)
    except InvalidArtifact:
        _gap(state, "runbook artifact is invalid")
        return {"status": "error", "note": "runbook artifact is invalid", "evidence_ids": []}
    if parsed.status in ("failed", "canceled"):
        _gap(state, "runbook lookup failed")
        return {"status": "error", "note": f"runbook lookup {parsed.status}", "evidence_ids": []}
    ident = f"runbook:{message_id}"
    content = parsed.summary[:8000]
    citations = [citation.model_dump(mode="json") for citation in parsed.citations]
    cited = parsed.citations_available and bool(citations)
    _add(state, ident, "runbook", message_id, "runbook", content[:400], {
        "schema": ARTIFACT_SCHEMA,
        "artifact_status": parsed.status,
        "content": content,
        "summary": content,
        "recommendations": [item.model_dump(mode="json") for item in parsed.recommendations][:8],
        "citations_available": cited,
        "citations": citations if cited else [],
        "warnings": parsed.warnings[:10],
        "source": "ragflow",
    })
    return {
        "status": "success",
        "data": {"citations_available": cited, "citations": len(citations)},
        "evidence_ids": [ident],
    }

_RUNTIME_MARKERS = (
    "timeout", "timed out", "latency", "connection", "reset", "refused",
    "tls", "5xx", "500", "502", "503", "504",
)


def _http_runtime_fault(state) -> bool:
    """本 run 是否持有该服务的 HTTP 检查样本。

    判据优先用“有没有检查数据”，而不是 monitor 事件的形状：只要本 run 拿到过
    任何检查行（窗口内 status_code 非 0 的 HTTP 检查），就按 HTTP/运行时类故障处理，
    必须要求指标/日志来源各查一次。事件状态与错误文本仍作为补充判据（无检查数据时
    由它们兜底）。不向模型提问，也不以工具调用次数作判据。
    """
    if state.get("raw_checks"):
        return True
    event = state.get("event") or {}
    try:
        code = int(event.get("status"))
    except (TypeError, ValueError):
        code = 0
    if 500 <= code < 600:
        return True
    text = " ".join(str(event.get(key) or "") for key in ("error", "message", "title")).lower()
    if any(marker in text for marker in _RUNTIME_MARKERS):
        return True
    for row in state.get("raw_checks") or []:
        try:
            code = int(row.get("status_code") or 0)
        except (TypeError, ValueError):
            code = 0
        if 500 <= code < 600:
            return True
        if any(marker in str(row.get("error") or "").lower() for marker in _RUNTIME_MARKERS):
            return True
    return False


def _mcp_activity(state, source: str) -> dict:
    return state.setdefault("mcp_activity", {}).setdefault(
        source, {"calls": 0, "status": "empty"})


def _note_mcp_activity(state, source: str, status: str | None = None,
                       counted: bool = False, error: str | None = None) -> None:
    entry = _mcp_activity(state, source)
    if counted:
        entry["calls"] = int(entry.get("calls") or 0) + 1
    if status:
        entry["status"] = status
        if status == "ok":
            entry.pop("error", None)
    if error:
        entry["error"] = str(error)[:200]


def _note_unqueried_sources(state, settings) -> None:
    """把“源可用但本次一次没查”记成 coverage 里的 empty + gap。

    只在本 run 有 HTTP 检查样本（或事件本身呈现 HTTP/运行时故障）时记；
    确实没有任何检查数据时跳过历史/时间线查询是合理行为。
    """
    if not _http_runtime_fault(state):
        return
    tools = state.get("mcp_tools") or []
    for source, enabled in (
        ("prometheus", bool(str(settings.prometheus_mcp_url or "").strip())
         and any(name in PROMETHEUS_TOOLS for name in tools)),
        ("loki", bool(str(settings.loki_mcp_url or "").strip())
         and any(name in LOKI_TOOLS for name in tools)),
    ):
        if not enabled:
            continue
        entry = _mcp_activity(state, source)
        if int(entry.get("calls") or 0) > 0:
            continue
        entry["status"] = "empty"
        code = f"{source.upper()}_NOT_QUERIED"
        gaps = state.setdefault("coverage", {}).setdefault("gaps", [])
        if code not in gaps:
            gaps.append(code)


def _gap(state, text: str) -> None:
    state.setdefault("coverage", {}).setdefault("gaps", []).append(text)


def clip(result: dict) -> dict:
    text = json.dumps(result)
    if len(text) <= 4000:
        return result
    return {"status": result.get("status"), "note": "truncated", "evidence_ids": result.get("evidence_ids") or []}
