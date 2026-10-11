import json
from datetime import datetime, timezone
from typing import TypedDict

from langgraph.graph import END, StateGraph

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now
from alertflow_agent.integrations.project_diagnostics import fetch, load_config, mark_stale
from alertflow_agent.runtime.guards import before_lead, charge_llm, fresh_guard, note_tool
from alertflow_agent.runtime.llm import draft_from_state
from alertflow_agent.runtime.tools import clip, run_tool
from alertflow_agent.runtime.verify import cited, verify


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
            try:
                return self.graph.invoke(None, config)
            except Exception:
                # A process restart can miss the last checkpoint. The tools are read-only, so the run starts again.
                pass
        occurred = row["input_event"]["occurred_at"]
        return self.graph.invoke({
                "run_id": run_id,
                "mode": row["mode"],
                "event": row["input_event"],
                "evidence": {},
                "raw_checks": [],
                "series": [],
                "coverage": {"from": _epoch(occurred) - self.settings.check_lookback_minutes * 60, "to": _epoch(occurred), "gaps": []},
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
        graph.add_edge("initial_plan", "lead_model")
        graph.add_conditional_edges("lead_model", self.after_lead, {"tools": "lead_tools", "replan": "replan", "report": "write_report"})
        graph.add_edge("lead_tools", "lead_model")
        graph.add_edge("replan", "lead_model")
        graph.add_edge("write_report", "verify")
        graph.add_conditional_edges("verify", self.after_verify, {"revise": "write_report", "persist": "persist"})
        graph.add_edge("persist", END)
        if checkpointer is None:
            from langgraph.checkpoint.memory import MemorySaver

            checkpointer = MemorySaver()
        return graph.compile(checkpointer=checkpointer)

    def collect_alertflow(self, state):
        event = state["event"]
        gaps = list(state["coverage"].get("gaps") or [])
        try:
            detail = self.alertflow.incident(event["incident_id"]) if event.get("incident_id") else None
            listing = self.alertflow.incidents(event["service_id"], 10)
            metrics = self.alertflow.metrics(event["service_id"], "24h")
            rows = collect_checks(self.alertflow, event["service_id"], state["coverage"]["from"], state["coverage"]["to"], self.settings.check_max_rows)
        except Exception as exc:
            state["failure_reason"] = "collect_failed" if not state.get("raw_checks") else ""
            gaps.append("ALERTFLOW_PARTIAL")
            state["coverage"]["gaps"] = gaps
            state["source_error"] = str(exc)
            return state
        if detail is None and not rows:
            state["failure_reason"] = "collect_failed"
            return state
        state["raw_checks"] = rows
        state["series"] = [
            {"ts": p.get("ts"), "avg_latency": p.get("avgLatency")}
            for p in (metrics.get("series") or [])
        ]
        state["interval"] = ((metrics.get("check") or {}).get("interval"))
        if detail:
            _evidence(state, f"inc:{detail['id']}", "alertflow_incident", "incident", detail.get("title") or detail.get("status") or "incident", {"status": detail.get("status"), "severity": detail.get("severity")})
            for comment in detail.get("comments") or []:
                _evidence(state, f"cmt:{detail['id']}:{comment['id']}", "alertflow_comment", "comment", comment.get("body") or "", {"body": comment.get("body")})
            for alert in detail.get("externalAlerts") or []:
                _evidence(state, f"ext:{detail['id']}:{alert.get('source')}:{alert.get('alertKey')}", "alertflow_external_alert", "external_alert", alert.get("title") or alert.get("status"), alert)
        for inc in listing.get("incidents") or []:
            _evidence(state, f"inc:{inc['id']}", "alertflow_incident", "incident", inc.get("title") or inc.get("status") or "incident", {"status": inc.get("status"), "severity": inc.get("severity")})
        if metrics.get("tls"):
            _evidence(state, f"tls:{event['service_id']}", "alertflow_tls", "tls", metrics["tls"].get("error") or "tls snapshot", metrics["tls"], ["TLS_SNAPSHOT_ONLY"])
            gaps.append("TLS_SNAPSHOT_ONLY")
        check = metrics.get("check") or {}
        _evidence(state, f"svc:{event['service_id']}", "alertflow_service", "service", "check configuration from the metrics payload", check)
        _evidence(state, "met:24h", "alertflow_metric", "metric_series", "24h series", {"points": len(state["series"])})
        for row in key_checks(rows):
            _evidence(state, f"chk:{row['id']}", "alertflow_check", "http_check", row.get("error") or row["status"], row)
        state["coverage"]["gaps"] = gaps
        return state

    def collect_diagnostics(self, state):
        cfg = load_config(self.settings.diagnostics_file)
        result = fetch(cfg, state["event"]["occurred_at"], state["event"]["occurred_at"])
        if cfg:
            result = mark_stale(result, datetime.fromisoformat(state["event"]["occurred_at"].replace("Z", "+00:00")))
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
            _evidence(state, ident, "project_diagnostic", item["kind"], item.get("summary") or item.get("status") or item["name"], item)
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
        plan = self.model.complete("initial_plan", state)
        state["plan"] = plan
        state["guard"]["llm_calls"] += 1
        self.store.append_event(state["run_id"], "plan.created", {"version": plan.get("version")})
        return state

    def lead_model(self, state):
        reason = before_lead(state["guard"], self.settings, state["started"], datetime.now(timezone.utc).timestamp())
        if reason or state.get("finished"):
            state["stop_reason"] = reason or state.get("stop_reason") or ""
            state["finished"] = True
            return state
        decision = self.model.complete("lead", state)
        state["guard"]["lead_rounds"] += 1
        stop = charge_llm(state["guard"], self.settings, json.dumps(decision))
        if stop:
            state["stop_reason"] = stop
            state["finished"] = True
        state["pending_tool"] = decision
        state["messages"].append({"role": "ai", "content": json.dumps(decision)})
        if decision.get("tool") is None:
            state["guard"]["plain_text_streak"] += 1
            if state["guard"]["plain_text_streak"] >= 2:
                state["stop_reason"] = "no_progress"
                state["finished"] = True
        else:
            state["guard"]["plain_text_streak"] = 0
        return state

    def after_lead(self, state):
        if state.get("finished") and (state.get("pending_tool") or {}).get("tool") != "finish_investigation":
            return "report"
        tool = (state.get("pending_tool") or {}).get("tool")
        if tool == "finish_investigation" or state.get("finished"):
            return "report"
        if tool == "request_replan":
            return "replan"
        return "tools"

    def lead_tools(self, state):
        decision = state.get("pending_tool") or {}
        name = decision.get("tool")
        args = decision.get("args") or {}
        signature = name + json.dumps(args, sort_keys=True)
        previous = state["guard"]["tool_sigs"]
        if len(previous) >= 1 and previous[-1] == signature:
            result = {"status": "success", "note": "cached", "evidence_ids": []}
        else:
            result = clip(run_tool(name, args, state, self.settings, {"alertflow": self.alertflow, "model": self.model}))
        stop = note_tool(state["guard"], signature)
        if stop:
            state["stop_reason"] = stop
            state["finished"] = True
        state["messages"].append({"role": "tool", "name": name, "content": json.dumps(result)})
        self.store.append_event(state["run_id"], "tool.completed", {"name": name, "status": result.get("status")})
        return state

    def replan(self, state):
        if state["guard"]["replans"] >= self.settings.max_replans:
            state["stop_reason"] = "budget_replans"
            state["finished"] = True
            return state
        new = self.model.complete("replan", state)
        state["guard"]["replans"] += 1
        if _plan_ok(state.get("plan"), new, state["evidence"]):
            state["plan"] = new
            self.store.append_event(state["run_id"], "plan.revised", {"version": new.get("version")})
        else:
            self.store.append_event(state["run_id"], "plan.rejected", {})
        state["replan_requested"] = ""
        state["pending_tool"] = {"tool": "finish_investigation", "args": {}}
        return state

    def write_report(self, state):
        if state.get("failure_reason") == "collect_failed":
            return state
        draft = self.model.complete("write_report", state)
        if not isinstance(draft, dict) or "observations" not in draft:
            draft = draft_from_state(state)
        state["report"] = draft
        state["guard"]["llm_calls"] += 1
        return state

    def verify_node(self, state):
        if state.get("failure_reason") == "collect_failed":
            return state
        tokens = [self.settings.agent_webhook_token, self.settings.agent_api_token, self.settings.alertflow_agent_token, self.settings.llm_api_key]
        result = verify(state["report"], state["evidence"], tokens)
        state["report"] = result["draft"]
        state["verification"] = {"passed": result["passed"], "findings": result["findings"], "warnings": result["warnings"]}
        if not result["passed"]:
            state["revisions"] += 1
        return state

    def after_verify(self, state):
        if state.get("failure_reason") == "collect_failed":
            return "persist"
        if state["verification"]["passed"]:
            return "persist"
        if state["revisions"] <= self.settings.max_revisions:
            return "revise"
        return "persist"

    def persist(self, state):
        run_id = state["run_id"]
        self.store.save_evidence(run_id, state.get("evidence") or {})
        if state.get("failure_reason") == "collect_failed":
            self.store.update(run_id, status="failed", failure_reason="collect_failed", stage="persist", finished_at=iso_now())
            self.store.append_event(run_id, "run.failed", {"reason": "collect_failed"})
            return state
        passed = (state.get("verification") or {}).get("passed")
        status = "completed" if passed and not state.get("stop_reason") else "partial"
        report = dict(state.get("report") or {})
        report.update({
            "schema_version": "v1",
            "run_id": run_id,
            "incident_id": state["event"].get("incident_id"),
            "service_id": state["event"]["service_id"],
            "version": 1,
            "status": status,
            "coverage": {
                "from": state["coverage"].get("from"),
                "to": state["coverage"].get("to"),
                "check_samples": len(state.get("raw_checks") or []),
                "diagnostic_source": state.get("diagnostic_source") or "not_configured",
                "gaps": state["coverage"].get("gaps") or [],
            },
            "verification": state.get("verification"),
            "usage": {"llm_calls": state["guard"]["llm_calls"], "tool_calls": state["guard"]["tool_calls"], "tokens": state["guard"]["tokens"], "mode": state["mode"]},
            "completed_at": iso_now(),
        })
        if state.get("stop_reason"):
            report.setdefault("unknowns", []).append({"text": "The investigation stopped because of the agent budget.", "code": state["stop_reason"], "evidence_ids": []})
        version = 2 if state["event"].get("event") == "incident_resolve" else 1
        report["version"] = version
        self.store.save_report(run_id, version, report)
        self.store.update(run_id, status=status, stop_reason=state.get("stop_reason") or None, stage="persist", finished_at=iso_now(), usage=report["usage"])
        self.store.append_event(run_id, "report.saved", {"version": version, "status": status, "cited": sorted(cited(report))})
        state["report"] = report
        return state


def _plan_ok(old, new, evidence) -> bool:
    if not old or not isinstance(new, dict):
        return False
    if new.get("version", 0) <= old.get("version", 0):
        return False
    old_ids = {h["id"]: h for h in old.get("hypotheses") or []}
    new_ids = {h["id"]: h for h in new.get("hypotheses") or []}
    for ident, hyp in old_ids.items():
        if ident not in new_ids or new_ids[ident].get("layer") != hyp.get("layer"):
            return False
    for hyp in new.get("hypotheses") or []:
        if hyp.get("status") in {"supported", "weakened", "ruled_out"}:
            ids = (hyp.get("evidence_for") or []) + (hyp.get("evidence_against") or [])
            if not ids or any(i not in evidence for i in ids):
                return False
    return True
