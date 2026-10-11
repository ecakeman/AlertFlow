import json
import logging
from datetime import datetime, timezone
from typing import TypedDict

from langgraph.graph import END, StateGraph

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now
from alertflow_agent.integrations.project_diagnostics import fetch, load_config
from alertflow_agent.runtime.guards import before_lead, fresh_guard, note_tool
from alertflow_agent.runtime.model import call_model
from alertflow_agent.runtime.planner import validate_plan_diff
from alertflow_agent.runtime.tools import clip, run_tool
from alertflow_agent.runtime.verify import cited, verify

log = logging.getLogger("alertflow_agent")


class GraphState(TypedDict, total=False):
    run_id: str
    mode: str
    event: dict
    evidence: dict
    raw_checks: list
    series: list
    coverage: dict
    skills: list
    skills_loaded: list
    plan: dict
    messages: list
    report: dict
    verification: dict
    revisions: int
    guard: dict
    finished: bool
    replan_requested: str
    failure_reason: str
    stop_reason: str
    script: list
    analysis_n: int
    interval: int
    started: float
    diagnostic_source: str
    pending_tool: dict
    source_error: str
    source_status: dict
    skill_texts: dict
    diagnostic_calls: int
    prior_report: dict
    replan_bad: bool
    cancelled: bool
    script_args: dict


def _epoch(value: str) -> int:
    return int(datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp())


def _evidence(state, ident, source, kind, summary, data, limitations=None):
    state["evidence"][ident] = {
        "id": ident,
        "source_type": source,
        "source_ref": ident,
        "service_id": state["event"]["service_id"],
        "observed_at": state["event"]["occurred_at"],
        "collected_at": iso_now(),
        "kind": kind,
        "summary": summary[:1500],
        "data": data,
        "limitations": limitations or [],
    }


class Runner:
    def __init__(self, settings, store, alertflow, model, checkpointer=None) -> None:
        self.settings = settings
        self.store = store
        self.alertflow = alertflow
        self.model = model
        self.graph = self._graph(checkpointer)

    def execute(self, run_id: str, resume: bool = False) -> dict:
        row = self.store.get(run_id)
        config = {"configurable": {"thread_id": run_id}}
        if resume:
            restored = None
            reason = "empty_checkpoint"
            try:
                restored = self.graph.invoke(None, config)
            except Exception as exc:
                reason = type(exc).__name__
            if restored:
                return restored
            self.store.append_event(run_id, "run.resumed_without_checkpoint", {"reason": reason})
        occurred = row["input_event"]["occurred_at"]
        end_at = row["input_event"].get("resolved_at") or occurred
        return self.graph.invoke({
                "run_id": run_id,
                "mode": row["mode"],
                "event": row["input_event"],
                "evidence": {},
                "raw_checks": [],
                "series": [],
                "coverage": {"from": _epoch(occurred) - self.settings.check_lookback_minutes * 60, "to": _epoch(end_at), "gaps": []},
                "skills": [],
                "skills_loaded": [],
                "plan": None,
                "messages": [],
                "report": None,
                "verification": None,
                "revisions": 0,
                "guard": fresh_guard(),
                "finished": False,
                "replan_requested": "",
                "failure_reason": "",
                "stop_reason": "",
                "script": row["input_event"].get("script") or ["finish"],
                "analysis_n": 0,
                "interval": None,
                "started": datetime.now(timezone.utc).timestamp(),
                "source_status": {},
                "skill_texts": {},
                "diagnostic_calls": 0,
                "prior_report": self._prior_report(row),
                "replan_bad": row["input_event"].get("replan_bad"),
        }, config)

    def _graph(self, checkpointer):
        graph = StateGraph(GraphState)
        graph.add_node("bootstrap", lambda s: s)
        graph.add_node("collect_alertflow", self.collect_alertflow)
        graph.add_node("collect_diagnostics", self.collect_diagnostics)
        graph.add_node("analyze", self.analyze)
        graph.add_node("select_skills", self.select_skills)
        graph.add_node("initial_plan", self.initial_plan)
        graph.add_node("lead_model", self.lead_model)
        graph.add_node("lead_tools", self.lead_tools)
        graph.add_node("replan", self.replan)
        graph.add_node("write_report", self.write_report)
        graph.add_node("verify", self.verify_node)
        graph.add_node("persist", self.persist)
        graph.set_entry_point("bootstrap")
        graph.add_edge("bootstrap", "collect_alertflow")
        graph.add_edge("collect_alertflow", "collect_diagnostics")
        graph.add_edge("collect_diagnostics", "analyze")
        graph.add_edge("analyze", "select_skills")
        graph.add_conditional_edges("select_skills", self.after_skills, {"plan": "initial_plan", "report": "write_report", "persist": "persist"})
        graph.add_conditional_edges("initial_plan", self.after_plan, {"lead": "lead_model", "persist": "persist"})
        graph.add_conditional_edges("lead_model", self.after_lead, {"tools": "lead_tools", "report": "write_report", "persist": "persist", "lead": "lead_model"})
        graph.add_conditional_edges("lead_tools", self.after_tools, {"lead": "lead_model", "replan": "replan", "report": "write_report", "persist": "persist"})
        graph.add_conditional_edges("replan", self.after_replan, {"lead": "lead_model", "report": "write_report", "persist": "persist"})
        graph.add_edge("write_report", "verify")
        graph.add_conditional_edges("verify", self.after_verify, {"revise": "write_report", "persist": "persist"})
        graph.add_edge("persist", END)
        if checkpointer is None:
            from langgraph.checkpoint.memory import MemorySaver

            checkpointer = MemorySaver()
        return graph.compile(checkpointer=checkpointer)

    def collect_alertflow(self, state):
        if self._stop_if_cancelled(state):
            return state
        event = state["event"]
        gaps = list(state["coverage"].get("gaps") or [])
        status = {}
        detail = self._source(status, "incident", lambda: self.alertflow.incident(event["incident_id"]) if event.get("incident_id") else None)
        listing = self._source(status, "incidents", lambda: self.alertflow.incidents(event["service_id"], 10))
        metrics = self._source(status, "metrics", lambda: self.alertflow.metrics(event["service_id"], "24h"))
        rows = self._source(status, "checks", lambda: collect_checks(
            self.alertflow, event["service_id"], state["coverage"]["from"], state["coverage"]["to"], self.settings.check_max_rows,
        ))
        state["source_status"] = status
        if rows is None and detail is None:
            state["failure_reason"] = "collect_failed"
            gaps.append("ALERTFLOW_PARTIAL")
            state["coverage"]["gaps"] = gaps
            return state
        if any(value == "unavailable" for value in status.values()):
            gaps.append("ALERTFLOW_PARTIAL")
        state["raw_checks"] = rows or []
        if metrics:
            state["series"] = [{"ts": point.get("ts"), "avg_latency": point.get("avgLatency")} for point in (metrics.get("series") or [])]
            state["interval"] = ((metrics.get("check") or {}).get("interval"))
            if metrics.get("tls"):
                _evidence(state, f"tls:{event['service_id']}", "alertflow_tls", "tls", metrics["tls"].get("error") or "tls snapshot", metrics["tls"], ["TLS_SNAPSHOT_ONLY"])
                gaps.append("TLS_SNAPSHOT_ONLY")
            _evidence(state, f"svc:{event['service_id']}", "alertflow_service", "service", "check configuration from the metrics payload", metrics.get("check") or {})
            _evidence(state, "met:24h", "alertflow_metric", "metric_series", "24h series", {"series": state["series"][:200]})
        if detail:
            _evidence(state, f"inc:{detail['id']}", "alertflow_incident", "incident", detail.get("title") or detail.get("status") or "incident", {"status": detail.get("status"), "severity": detail.get("severity")})
            for comment in detail.get("comments") or []:
                _evidence(state, f"cmt:{detail['id']}:{comment['id']}", "alertflow_comment", "comment", comment.get("body") or "", {"body": comment.get("body")})
            for alert in detail.get("externalAlerts") or []:
                _evidence(state, f"ext:{detail['id']}:{alert.get('source')}:{alert.get('alertKey')}", "alertflow_external_alert", "external_alert", alert.get("title") or alert.get("status") or "", alert)
        if listing:
            for inc in listing.get("incidents") or []:
                _evidence(state, f"inc:{inc['id']}", "alertflow_incident", "incident", inc.get("title") or inc.get("status") or "incident", {"status": inc.get("status"), "severity": inc.get("severity")})
        for row in key_checks(state["raw_checks"]):
            _evidence(state, f"chk:{row['id']}", "alertflow_check", "http_check", row.get("error") or row["status"], row)
        state["coverage"]["gaps"] = gaps
        return state

    def _source(self, status: dict, name: str, fn):
        try:
            value = fn()
        except Exception:
            log.warning("alertflow source unavailable", extra={"source": name})
            status[name] = "unavailable"
            return None
        status[name] = "empty" if value in (None, [], {}) else "ok"
        return value

    def collect_diagnostics(self, state):
        if self._stop_if_cancelled(state):
            return state
        cfg = load_config(self.settings.diagnostics_file)
        start = datetime.fromtimestamp(state["coverage"]["from"], timezone.utc).isoformat()
        end = datetime.fromtimestamp(state["coverage"]["to"], timezone.utc).isoformat()
        event_time = datetime.fromisoformat(state["event"]["occurred_at"].replace("Z", "+00:00"))
        result = fetch(cfg, start, end, event_time)
        mapping = {
            "not_configured": "NO_PROJECT_DIAGNOSTICS",
            "unavailable": "DIAGNOSTICS_UNAVAILABLE",
            "invalid": "DIAGNOSTICS_INVALID",
            "partial": "DIAGNOSTICS_PARTIAL",
            "stale": "DIAGNOSTICS_STALE",
            "empty": "DIAGNOSTICS_EMPTY",
        }
        code = mapping.get(result["coverage"])
        if code:
            state["coverage"].setdefault("gaps", []).append(code)
        state["diagnostic_source"] = result["coverage"]
        for item in result["items"]:
            ident = f"diag:{item['kind']}:{item['name']}"
            _evidence(state, ident, "project_diagnostic", item["kind"], item.get("summary") or item.get("status") or item["name"], item, item.get("limitations") or [])
        return state

    def analyze(self, state):
        if state.get("failure_reason"):
            return state
        rows = state["raw_checks"]
        n = 0
        window_from = state["coverage"]["from"]
        payloads = {
            "ana:status@0": status_counts(rows),
            "ana:segments@0": segments(rows),
            "ana:latency@0": latency_stats(rows, state.get("series") or [], window_from),
            "ana:errors@0": error_stats(rows, classify_error),
            "ana:coverage@0": coverage(rows, window_from, state["coverage"]["to"], state.get("interval")),
        }
        gaps = list(state["coverage"].get("gaps") or [])
        if payloads["ana:latency@0"]["low_samples"]:
            gaps.append("LOW_SAMPLES")
        if payloads["ana:latency@0"]["no_baseline"]:
            gaps.append("NO_BASELINE")
        if any(r.get("status") == "warning" for r in rows):
            gaps.append("RETRY_SEMANTICS")
        gaps.extend(payloads["ana:coverage@0"]["gaps"])
        state["coverage"]["gaps"] = list(dict.fromkeys(gaps))
        state["analysis_n"] = n
        for ident, data in payloads.items():
            _evidence(state, ident, "analysis", "analysis", json.dumps(data)[:1500], data)
        return state

    def select_skills(self, state):
        if state.get("failure_reason"):
            return state
        chosen = []
        segments_data = state["evidence"].get("ana:segments@0", {}).get("data") or {}
        latency = state["evidence"].get("ana:latency@0", {}).get("data") or {}
        errors = state["evidence"].get("ana:errors@0", {}).get("data") or {}
        offline = any(seg.get("status") == "offline" for seg in segments_data.get("segments") or [])
        external = any(v["source_type"] == "alertflow_external_alert" for v in state["evidence"].values())
        if state["event"]["event"] == "incident_start" and (offline or external):
            chosen.append("service-unavailable")
        warnings = (state["evidence"].get("ana:status@0", {}).get("data") or {}).get("warning", {}).get("n", 0)
        if warnings or segments_data.get("transitions", 0) >= 3 or state["event"]["event"] == "warning":
            chosen.append("intermittent-failure")
        recent_n = (latency.get("recent") or {}).get("n") or 0
        if (latency.get("median_ratio") or 0) >= 1.5 and recent_n >= 20:
            chosen.append("latency-degradation")
        tls = next((v for v in state["evidence"].values() if v["source_type"] == "alertflow_tls"), None)
        if tls and ((tls["data"].get("error")) or (tls["data"].get("daysLeft") is not None and tls["data"]["daysLeft"] <= 14)):
            chosen.append("tls-evidence")
        if (errors.get("classes") or {}).get("tls"):
            chosen.append("tls-evidence")
        state["skills"] = list(dict.fromkeys(chosen))
        return state

    def after_skills(self, state):
        if state.get("failure_reason"):
            return "persist"
        if state.get("mode") == "baseline":
            return "report"
        return "plan"

    def initial_plan(self, state):
        if self._stop_if_cancelled(state):
            return state
        plan = call_model(self.model, "initial_plan", state, self.settings, self.store)
        if not plan:
            return state
        state["plan"] = plan
        self.store.append_event(state["run_id"], "plan.created", {"version": plan.get("version")})
        return state

    def after_plan(self, state):
        if state.get("cancelled"):
            return "persist"
        if state.get("failure_reason"):
            return "persist"
        return "lead"

    def lead_model(self, state):
        if self._stop_if_cancelled(state):
            return state
        now = datetime.now(timezone.utc).timestamp()
        reason = before_lead(state["guard"], self.settings, state["started"], now)
        if reason or state.get("finished") or state.get("failure_reason"):
            state["stop_reason"] = state.get("stop_reason") or reason or ""
            state["finished"] = True
            return state
        decision = call_model(self.model, "lead", state, self.settings, self.store)
        if decision is None:
            return state
        state["pending_tool"] = decision
        state["messages"].append({"role": "ai", "content": json.dumps({"tool": decision.get("tool"), "text": decision.get("text")})})
        if not decision.get("tool"):
            state["guard"]["plain_text_streak"] += 1
            if state["guard"]["plain_text_streak"] >= 2:
                state["stop_reason"] = "no_progress"
                state["finished"] = True
        else:
            state["guard"]["plain_text_streak"] = 0
        return state

    def after_lead(self, state):
        if state.get("cancelled") or state.get("failure_reason"):
            return "persist"
        if (state.get("pending_tool") or {}).get("tool"):
            return "tools"
        if state.get("finished"):
            return "report"
        return "lead"

    def lead_tools(self, state):
        if self._stop_if_cancelled(state):
            return state
        decision = state.get("pending_tool") or {}
        name = decision.get("tool")
        args = decision.get("args") or {}
        signature = name + json.dumps(args, sort_keys=True, default=str)
        cache = state["guard"].setdefault("tool_cache", {})
        if signature in cache:
            result = cache[signature]
            result = {**result, "note": "cached"}
        else:
            result = clip(run_tool(name, args, state, self.settings, {
                "alertflow": self.alertflow, "model": self.model, "store": self.store,
            }))
            cache[signature] = result
        stop = note_tool(state["guard"], signature)
        if stop:
            state["stop_reason"] = stop
            state["finished"] = True
        state["messages"].append({"role": "tool", "name": name, "content": json.dumps(result)})
        self.store.append_event(state["run_id"], "tool.completed", {"name": name, "status": result.get("status"), "evidence_ids": result.get("evidence_ids") or []})
        return state

    def after_tools(self, state):
        if state.get("cancelled") or state.get("failure_reason") == "llm_error":
            return "persist"
        name = (state.get("pending_tool") or {}).get("tool")
        if name == "finish_investigation":
            return "report"
        if name == "request_replan":
            return "replan"
        if state.get("stop_reason"):
            return "report"
        return "lead"

    def replan(self, state):
        if self._stop_if_cancelled(state):
            return state
        if state["guard"]["replans"] >= self.settings.max_replans:
            state["stop_reason"] = "budget_replans"
            state["finished"] = True
            return state
        state["guard"]["replans"] += 1
        accepted = None
        for _ in range(2):
            new = call_model(self.model, "replan", state, self.settings, self.store)
            if state.get("failure_reason"):
                return state
            if new and validate_plan_diff(state.get("plan"), new, state["evidence"]):
                accepted = new
                break
        if accepted:
            state["plan"] = accepted
            self.store.append_event(state["run_id"], "plan.revised", {"version": accepted.get("version")})
        else:
            self.store.append_event(state["run_id"], "plan.rejected", {})
        state["replan_requested"] = ""
        return state

    def after_replan(self, state):
        if state.get("cancelled") or state.get("failure_reason"):
            return "persist"
        if state.get("stop_reason"):
            return "report"
        return "lead"

    def write_report(self, state):
        if self._stop_if_cancelled(state) or state.get("failure_reason") in {"collect_failed", "llm_error"}:
            return state
        now = datetime.now(timezone.utc).timestamp()
        if now - state["started"] > self.settings.run_timeout_seconds + self.settings.report_grace_seconds:
            state["failure_reason"] = "report_timeout"
            return state
        draft = call_model(self.model, "write_report", state, self.settings, self.store)
        if not draft:
            if not state.get("failure_reason"):
                state["failure_reason"] = "llm_error"
            return state
        state["report"] = draft
        return state

    def verify_node(self, state):
        if state.get("failure_reason") or state.get("cancelled") or not state.get("report"):
            return state
        tokens = [self.settings.agent_webhook_token, self.settings.agent_api_token, self.settings.alertflow_agent_token, self.settings.llm_base_url, self.settings.llm_api_key]
        result = verify(state["report"], state["evidence"], tokens, state["coverage"].get("gaps") or [])
        state["report"] = result["draft"]
        state["verification"] = {"passed": result["passed"], "findings": result["findings"], "warnings": result["warnings"]}
        if not result["passed"]:
            state["revisions"] += 1
        return state

    def after_verify(self, state):
        if state.get("failure_reason") or state.get("cancelled") or not state.get("verification"):
            return "persist"
        if state["verification"]["passed"]:
            return "persist"
        if state["revisions"] <= self.settings.max_revisions:
            return "revise"
        return "persist"

    def persist(self, state):
        run_id = state["run_id"]
        if state.get("cancelled") or self._stop_if_cancelled(state):
            self.store.save_evidence(run_id, state.get("evidence") or {})
            return state
        self.store.save_evidence(run_id, state.get("evidence") or {})
        if state.get("failure_reason"):
            self.store.finish(run_id, status="failed", failure_reason=state["failure_reason"], stage="persist", finished_at=iso_now())
            self.store.append_event(run_id, "run.failed", {"reason": state["failure_reason"]})
            return state
        passed = (state.get("verification") or {}).get("passed")
        status = "completed" if passed and not state.get("stop_reason") else "partial"
        report = dict(state.get("report") or {})
        version = 2 if state["event"].get("event") == "incident_resolve" else 1
        report.update({
            "schema_version": "v1",
            "run_id": run_id,
            "incident_id": state["event"].get("incident_id"),
            "service_id": state["event"]["service_id"],
            "version": version,
            "status": status,
            "coverage": {
                "from": state["coverage"].get("from"),
                "to": state["coverage"].get("to"),
                "check_samples": len(state.get("raw_checks") or []),
                "diagnostic_source": state.get("diagnostic_source") or "not_configured",
                "gaps": state["coverage"].get("gaps") or [],
            },
            "verification": state.get("verification"),
            "usage": {
                "llm_calls": state["guard"]["llm_calls"],
                "tool_calls": state["guard"]["tool_calls"],
                "tokens": state["guard"]["tokens"],
                "estimated": bool(state["guard"].get("tokens_estimated")),
                "mode": state["mode"],
            },
            "completed_at": iso_now(),
        })
        if state.get("stop_reason"):
            report.setdefault("unknowns", []).append({"text": "The investigation stopped because of an agent limit.", "code": state["stop_reason"], "evidence_ids": []})
        if not report.get("summary"):
            self.store.finish(run_id, status="failed", failure_reason="report_missing", stage="persist", finished_at=iso_now())
            return state
        self.store.save_report(run_id, version, report)
        self.store.finish(run_id, status=status, stop_reason=state.get("stop_reason") or None, stage="persist", finished_at=iso_now(), usage=report["usage"])
        self.store.append_event(run_id, "report.saved", {"version": version, "status": status, "cited": sorted(cited(report))})
        fresh = self.store.get(run_id) or {}
        pending = (fresh.get("input_event") or {}).get("pending_revise")
        if pending and self.settings.agent_revise_on_resolve and version == 1:
            current = dict(fresh.get("input_event") or {})
            current["event"] = "incident_resolve"
            self.store.update(run_id, input_event=current)
            self.store.transition(run_id, {"completed", "partial"}, status="queued", stage="revise", mode="baseline")
        state["report"] = report
        return state

    def _prior_report(self, row: dict):
        if row.get("stage") != "revise" and row["input_event"].get("event") != "incident_resolve":
            return None
        saved = self.store.latest_report(row["id"])
        if not saved or saved["version"] != 1:
            return None
        return {"summary": saved["report"].get("summary"), "version": 1}

    def _stop_if_cancelled(self, state) -> bool:
        row = self.store.get(state["run_id"])
        if row and row.get("status") == "cancelled":
            state["cancelled"] = True
            return True
        return False
