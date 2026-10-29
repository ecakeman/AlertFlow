import json
import logging
from datetime import datetime, timezone
from typing import TypedDict

from langgraph.graph import END, StateGraph

from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import error_stats, latency_stats
from alertflow_agent.analysis.timeline import coverage, key_checks, segments, status_counts
from alertflow_agent.integrations.alertflow import collect_checks, iso_now, normalize_check
from alertflow_agent.integrations.project_diagnostics import fetch, load_config
from alertflow_agent.runtime.guards import before_lead, fresh_guard, note_tool
from alertflow_agent.runtime.model import call_model
from alertflow_agent.runtime.planner import validate_plan_diff
from alertflow_agent.runtime.tools import clip, run_tool
from alertflow_agent.runtime.verify import build_catalog, cited, verify

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
    prior: dict          # 检索到的既往上下文（结构化）
    prior_block: str     # 渲染给模型的既往上下文文本
    memory: dict         # remember 的写入结果
    diagnostic_calls: int
    prior_report: dict
    fact_catalog: list
    llm_report: dict
    replan_bad: bool
    cancelled: bool
    script_args: dict
    # 随事件一起下推的默认上下文（AlertFlow 已组装好）。它不进 state["event"]，
    # 只用于建 Evidence Registry；后续深挖仍走查询工具。
    provided_context: dict


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
    def __init__(self, settings, store, alertflow, model, checkpointer=None, memory=None, embed=None, rerank=None) -> None:
        self.settings = settings
        self.store = store
        self.alertflow = alertflow
        self.model = model
        self.memory = memory      # 长期记忆仓储（None → 记忆关闭）
        self.embed = embed        # 嵌入客户端（None / 未配置 → 不做向量召回）
        self.rerank = rerank      # 重排客户端（None / 未配置 → 退回向量序）
        self.graph = self._graph(checkpointer)

    def _refresh_runtime_config(self) -> None:
        """每 run 拉一次 AlertFlow 侧配置（模型端点/密钥/记忆开关）。

        这样在 UI 里改配置立刻生效，不用重建容器。拉不到就沿用本地 env——
        **配置来源是"增强"，不是"依赖"**。
        """
        try:
            cfg = self.alertflow.runtime_config()
        except Exception:
            return
        if not isinstance(cfg, dict) or not cfg:
            return
        from alertflow_agent.integrations.embeddings import EmbedClient, RerankClient
        from alertflow_agent.runtime.llm import OpenAIModel

        s = self.settings
        llm = cfg.get("llm") or {}
        if llm.get("base_url"):
            s.llm_base_url = llm["base_url"]
        if llm.get("api_key"):
            s.llm_api_key = llm["api_key"]
        if llm.get("model"):
            s.llm_model = llm["model"]
        embed = cfg.get("embed") or {}
        if embed.get("base_url"):
            s.embed_base_url = embed["base_url"]
        if embed.get("api_key"):
            s.embed_api_key = embed["api_key"]
        if embed.get("model"):
            s.embed_model = embed["model"]
        if embed.get("dim"):
            s.embed_dim = int(embed["dim"])
        rerank = cfg.get("rerank") or {}
        if rerank.get("base_url"):
            s.rerank_base_url = rerank["base_url"]
        if rerank.get("api_key"):
            s.rerank_api_key = rerank["api_key"]
        if rerank.get("model"):
            s.rerank_model = rerank["model"]
        if rerank.get("style"):
            s.rerank_style = rerank["style"]
        if "memory_enabled" in cfg:
            s.memory_enabled = bool(cfg["memory_enabled"])
        if cfg.get("context_budget_tokens"):
            s.context_budget_tokens = int(cfg["context_budget_tokens"])

        self.embed = EmbedClient(s.embed_base_url, s.embed_api_key, s.embed_model, s.embed_dim)
        self.rerank = RerankClient(s.rerank_base_url, s.rerank_api_key, s.rerank_model, s.rerank_style)
        if s.llm_api_key:
            try:
                self.model = OpenAIModel(s)
            except Exception:  # noqa: BLE001 - keep the existing model on failure
                pass

    def execute(self, run_id: str, resume: bool = False) -> dict:
        row = self.store.get(run_id)
        if not row or row.get("status") == "cancelled":
            return row or {}
        self._refresh_runtime_config()
        self._notify_started(run_id, row)
        try:
            if resume:
                self._resume(row)
            else:
                self._invoke_fresh(self._fresh_row(row), self._fresh_thread(row))
        except Exception as exc:
            self._fail(run_id, type(exc).__name__)
        current = self.store.get(run_id) or {}
        if current.get("status") == "running":
            self._fail(run_id, "checkpoint_unresolved")
        return self.store.get(run_id) or {}

    def _fresh_thread(self, row: dict) -> str:
        run_id = row["id"]
        snap = self._snapshot(run_id)
        if snap is not None and not getattr(snap, "next", ()):
            usage = dict(row.get("usage") or {})
            attempt = int(usage.get("attempt") or 0) + 1
            usage["attempt"] = attempt
            self.store.update(run_id, usage=usage)
            return f"{run_id}:{attempt}"
        return run_id

    def _fresh_row(self, row: dict) -> dict:
        return self.store.get(row["id"]) or row

    def _resume(self, row: dict) -> None:
        run_id = row["id"]
        usage = dict(row.get("usage") or {})
        attempt = int(usage.get("attempt") or 0)
        threads = [run_id] + [f"{run_id}:{n}" for n in range(1, attempt + 1)]
        pending = None
        finished = False
        for thread in threads:
            snap = self._snapshot(thread)
            if snap is None:
                continue
            if getattr(snap, "next", ()):
                pending = thread
            else:
                finished = True
        if pending:
            self.graph.invoke(None, {"configurable": {"thread_id": pending}})
            return
        self.store.append_event(run_id, "run.resumed_without_checkpoint", {
            "reason": "checkpoint_finished" if finished else "empty_checkpoint",
        })
        attempt += 1
        usage["attempt"] = attempt
        self.store.update(run_id, usage=usage)
        self._invoke_fresh(self.store.get(run_id), f"{run_id}:{attempt}")

    def _snapshot(self, thread_id: str):
        try:
            snap = self.graph.get_state({"configurable": {"thread_id": thread_id}})
        except Exception:
            return None
        if not getattr(snap, "values", None):
            return None
        return snap

    def _invoke_fresh(self, row: dict, thread_id: str) -> dict:
        run_id = row["id"]
        event = dict(row["input_event"])
        # 下推的上下文单独取出来：它是"启动这次诊断的材料"，不是事件本身。
        # 混进 state["event"] 会被当成事件字段喂给模型。
        provided_context = event.pop("diagnostic_context", None)
        occurred = event["occurred_at"]
        end_at = event.get("resolved_at") or occurred
        return self.graph.invoke({
                "run_id": run_id,
                "mode": row["mode"],
                "event": event,
                "provided_context": provided_context,
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
                "script": event.get("script") or ["finish"],
                "analysis_n": 0,
                "interval": None,
                "started": datetime.now(timezone.utc).timestamp(),
                "source_status": {},
                "skill_texts": {},
                "diagnostic_calls": 0,
                "prior_report": self._prior_report(row),
                "replan_bad": event.get("replan_bad"),
        }, {"configurable": {"thread_id": thread_id}})

    def _notify_started(self, run_id: str, row: dict) -> None:
        """告诉 AlertFlow「这次诊断开始了」，前端因此能显示"运行中"。

        不入事务：丢了只是少一个进行中的提示。message_id 固定，重跑/恢复不会重复。
        """
        if not self.settings.agent_callback_token:
            return
        from alertflow_agent.callbacks import started_callback

        try:
            self.store.enqueue_callback(started_callback(run_id, row))
        except Exception:  # noqa: BLE001 - 提示发不出去不影响诊断
            log.warning("could not queue the run.started callback")

    def _fail(self, run_id: str, reason: str) -> None:
        if self.store.finish(
            run_id,
            callback=self._failure_callback(run_id, reason),
            status="failed",
            failure_reason=reason,
            stage="failed",
            finished_at=iso_now(),
        ):
            self.store.append_event(run_id, "run.failed", {"reason": reason})

    def _report_callback(self, run: dict, version: int, report: dict, cited_ids, evidence: dict):
        """回传消息。未配置回调令牌时返回 None：不入队，免得堆一堆没人领的消息。

        只回传报告引用到的证据——完整证据留在 Agent 自己这边。
        """
        if not self.settings.agent_callback_token:
            return None
        from alertflow_agent.callbacks import report_callback

        used = set(cited_ids)
        return report_callback(
            run.get("id") or "", run, version, report, report.get("usage") or {},
            {key: value for key, value in evidence.items() if key in used},
        )

    def _failure_callback(self, run_id: str, reason: str):
        if not self.settings.agent_callback_token:
            return None
        from alertflow_agent.callbacks import failure_callback

        return failure_callback(run_id, self.store.get(run_id) or {}, reason)

    def _graph(self, checkpointer):
        graph = StateGraph(GraphState)
        graph.add_node("bootstrap", lambda s: s)
        graph.add_node("collect_alertflow", self.collect_alertflow)
        graph.add_node("collect_diagnostics", self.collect_diagnostics)
        graph.add_node("analyze", self.analyze)
        graph.add_node("recall", self.recall)
        graph.add_node("select_skills", self.select_skills)
        graph.add_node("initial_plan", self.initial_plan)
        graph.add_node("lead_model", self.lead_model)
        graph.add_node("lead_tools", self.lead_tools)
        graph.add_node("replan", self.replan)
        graph.add_node("write_report", self.write_report)
        graph.add_node("verify", self.verify_node)
        graph.add_node("persist", self.persist)
        graph.add_node("remember", self.remember)
        graph.set_entry_point("bootstrap")
        graph.add_edge("bootstrap", "collect_alertflow")
        graph.add_edge("collect_alertflow", "collect_diagnostics")
        graph.add_edge("collect_diagnostics", "analyze")
        graph.add_edge("analyze", "recall")
        graph.add_edge("recall", "select_skills")
        graph.add_conditional_edges("select_skills", self.after_skills, {"plan": "initial_plan", "report": "write_report", "persist": "persist"})
        graph.add_conditional_edges("initial_plan", self.after_plan, {"lead": "lead_model", "persist": "persist"})
        graph.add_conditional_edges("lead_model", self.after_lead, {"tools": "lead_tools", "report": "write_report", "persist": "persist", "lead": "lead_model"})
        graph.add_conditional_edges("lead_tools", self.after_tools, {"lead": "lead_model", "replan": "replan", "report": "write_report", "persist": "persist"})
        graph.add_conditional_edges("replan", self.after_replan, {"lead": "lead_model", "report": "write_report", "persist": "persist"})
        graph.add_edge("write_report", "verify")
        graph.add_conditional_edges("verify", self.after_verify, {"revise": "write_report", "persist": "persist"})
        graph.add_edge("persist", "remember")
        graph.add_edge("remember", END)
        if checkpointer is None:
            from langgraph.checkpoint.memory import MemorySaver

            checkpointer = MemorySaver()
        return graph.compile(checkpointer=checkpointer)

    def collect_alertflow(self, state):
        if self._stop_if_cancelled(state):
            return state
        event = state["event"]
        if event.get("event") == "incident_resolve" and event.get("resolved_at") and event.get("occurred_at"):
            duration = _epoch(event["resolved_at"]) - _epoch(event["occurred_at"])
            _evidence(state, "evt:resolve", "alertflow_incident", "resolve", "incident recovered", {
                "occurred_at": event["occurred_at"],
                "resolved_at": event["resolved_at"],
                "duration_s": duration,
            })
        # 有下推的默认上下文就用它建注册表，不再把同样的数据回拉一遍。
        # 只有上下文在但一份可用证据都没有时（服务不在配置里、指标源整段缺失），
        # 才回到实时读取，并把这件事记进 coverage。
        context = state.get("provided_context")
        if context:
            self._seed_from_context(state, context)
            if state["evidence"]:
                return state
            state["coverage"].setdefault("gaps", []).append("CONTEXT_INSUFFICIENT")
        return self._collect_live(state)

    def _collect_live(self, state):
        """没有下推上下文时的实时读取（v1 事件、或上下文不可用）。"""
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
        if listing:
            for inc in listing.get("incidents") or []:
                _evidence(state, f"inc:{inc['id']}", "alertflow_incident", "incident", inc.get("title") or inc.get("status") or "incident", {"status": inc.get("status"), "severity": inc.get("severity")})
        for row in key_checks(state["raw_checks"]):
            _evidence(state, f"chk:{row['id']}", "alertflow_check", "http_check", row.get("error") or row["status"], row)
        state["coverage"]["gaps"] = gaps
        return state

    def _seed_from_context(self, state, context):
        """用下推的默认上下文建立 Evidence Registry。

        覆盖 collect_alertflow 原来要现拉的四样东西：当前 Incident（含评论与外部告警）、
        服务检查配置、24 小时指标与 TLS 快照、近期 Incident、默认检查记录。
        查询工具不删——它们负责后续按需深挖（更长时间范围、更久历史、特定 Incident）。
        """
        service_id = state["event"]["service_id"]
        gaps = list(state["coverage"].get("gaps") or [])
        status = {"provided_context": "ok"}

        window = context.get("timeWindow") or {}
        if window.get("from") and window.get("to"):
            # 窗口由事件时间决定，不由投递时刻决定——Go 侧已经算好了，直接采用。
            state["coverage"]["from"] = _epoch(window["from"])
            state["coverage"]["to"] = _epoch(window["to"])
        coverage = context.get("coverage") or {}
        for name, value in (coverage.get("sources") or {}).items():
            status[name] = value
        for code in list(coverage.get("gaps") or []) + list(coverage.get("truncated") or []):
            gaps.append(str(code))

        service = context.get("service") or {}
        check_cfg = service.get("check") or {}
        state["interval"] = check_cfg.get("interval")
        if service.get("present"):
            _evidence(state, f"svc:{service_id}", "alertflow_service", "service",
                      "check configuration from the pushed context", check_cfg)
        else:
            gaps.append("SERVICE_NOT_IN_CONFIG")

        metrics = context.get("metrics") or {}
        series = [{"ts": point.get("ts"), "avg_latency": point.get("avgLatency")}
                  for point in (metrics.get("series") or [])]
        state["series"] = series
        if series:
            _evidence(state, "met:24h", "alertflow_metric", "metric_series",
                      "series pushed with the event",
                      {"series": series, "from": metrics.get("from"), "to": metrics.get("to"),
                       "bucket_seconds": metrics.get("bucketSeconds")})
        if metrics.get("tls"):
            _evidence(state, f"tls:{service_id}", "alertflow_tls", "tls",
                      metrics["tls"].get("error") or "tls snapshot", metrics["tls"], ["TLS_SNAPSHOT_ONLY"])
            gaps.append("TLS_SNAPSHOT_ONLY")

        trigger = context.get("triggeringCheck")
        if isinstance(trigger, dict) and trigger.get("id") is not None:
            row = normalize_check(trigger)
            _evidence(state, f"chk:{row['id']}", "alertflow_check", "http_check",
                      row.get("error") or row["status"], row, ["TRIGGERING_CHECK"])

        rows = [normalize_check(item) for item in (context.get("recentChecks") or []) if isinstance(item, dict)]
        rows.sort(key=lambda row: row["ts"])
        state["raw_checks"] = rows
        for row in key_checks(rows):
            _evidence(state, f"chk:{row['id']}", "alertflow_check", "http_check",
                      row.get("error") or row["status"], row)

        incident = context.get("incident") or {}
        if incident.get("id"):
            _evidence(state, f"inc:{incident['id']}", "alertflow_incident", "incident",
                      incident.get("title") or incident.get("status") or "incident",
                      {"status": incident.get("status"), "severity": incident.get("severity")})
            for comment in incident.get("comments") or []:
                _evidence(state, f"cmt:{incident['id']}:{comment.get('id')}", "alertflow_comment",
                          "comment", comment.get("body") or "", {"body": comment.get("body")})
        for inc in context.get("relatedIncidents") or []:
            if isinstance(inc, dict) and inc.get("id"):
                _evidence(state, f"inc:{inc['id']}", "alertflow_incident", "incident",
                          inc.get("title") or inc.get("status") or "incident",
                          {"status": inc.get("status"), "severity": inc.get("severity")})

        state["source_status"] = status
        state["coverage"]["gaps"] = list(dict.fromkeys(gaps))
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
        if getattr(self.settings, "agent_diagnostics_mode", "timeline") != "legacy":
            state["diagnostic_source"] = "timeline"
            return state
        try:
            cfg = load_config(self.settings.diagnostics_file)
            start = datetime.fromtimestamp(state["coverage"]["from"], timezone.utc).isoformat()
            end = datetime.fromtimestamp(state["coverage"]["to"], timezone.utc).isoformat()
            event_time = datetime.fromisoformat(state["event"]["occurred_at"].replace("Z", "+00:00"))
            result = fetch(cfg, start, end, event_time)
        except Exception:
            log.warning("project diagnostics failed")
            result = {"coverage": "invalid", "items": [], "dropped": 0}
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
        if state["event"]["event"] == "incident_start" and offline:
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
                "memory": getattr(self, "memory", None),
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
        state["fact_catalog"] = build_catalog(state.get("evidence") or {})
        draft = call_model(self.model, "write_report", state, self.settings, self.store)
        if not draft:
            if not state.get("failure_reason"):
                state["failure_reason"] = "llm_error"
            return state
        state["llm_report"] = draft
        state["report"] = draft
        return state

    def verify_node(self, state):
        if state.get("failure_reason") or state.get("cancelled") or not state.get("report"):
            return state
        tokens = [self.settings.agent_webhook_token, self.settings.agent_api_token, self.settings.alertflow_agent_token, self.settings.llm_base_url, self.settings.llm_api_key]
        result = verify(
            state["report"], state["evidence"], tokens, state["coverage"].get("gaps") or [],
            state.get("fact_catalog") or build_catalog(state.get("evidence") or {}),
        )
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
            self._fail(run_id, state["failure_reason"])
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
            self._fail(run_id, "report_missing")
            return state
        fresh = self.store.get(run_id) or {}
        if fresh.get("stage") == "revise" or fresh.get("input_event", {}).get("event") == "incident_resolve":
            version = 2
        report["version"] = version
        pending = (fresh.get("input_event") or {}).get("pending_revise")
        used = sorted(cited(report))
        self.store.commit_report(
            run_id, version, report, status, state.get("stop_reason") or None, report["usage"],
            queue_revise=bool(pending and self.settings.agent_revise_on_resolve and version == 1),
            cited=used,
            replace_partial=fresh.get("stage") == "resume" and version == 1,
            # 回调与报告在同一个事务里入队：报告落库了，结果就一定会被回传。
            callback=self._report_callback(fresh, version, report, used, state.get("evidence") or {}),
        )
        state["report"] = report
        return state

    def recall(self, state):
        """在 analyze 之后、规划之前注入「既往上下文」。

        放在这里的理由：此时本次症状与内部证据签名都已经抽出来了，才谈得上「像不像」。
        检索失败不影响诊断——只记日志，state 里不放 prior_block。
        """
        if not getattr(self, "memory", None) or not self.settings.memory_enabled:
            return state
        try:
            from alertflow_agent.runtime.recall import recall_prior, render_block

            prior = recall_prior(state, self.memory, self.embed, getattr(self, "rerank", None), self.settings)
            if prior:
                state["prior"] = prior
                block = render_block(prior)
                if block:
                    state["prior_block"] = block
                # 落事件，供测试与排查看"这次到底命中了几条历史"。
                self.store.append_event(state["run_id"], "memory.recalled", {
                    "hits": len(prior.get("hits") or []),
                    "same_key_hits": len(prior.get("same_key_hits") or []),
                    "signature_key": prior.get("signature_key"),
                    "has_profile": bool(prior.get("profile")),
                    "embedded": bool(getattr(self.embed, "ready", False)),
                })
            else:
                self.store.append_event(state["run_id"], "memory.recalled", {"hits": 0, "has_profile": False})
        except Exception as exc:  # noqa: BLE001
            log.warning("recall failed: %s", type(exc).__name__)
        return state

    def remember(self, state):
        """写在报告之后的**独立一步**：把这次诊断写进长期记忆。

        刻意放在推理图之外、persist 之后：它失败绝不能影响报告，所以整段包在 try
        里，出错只记日志。事件卡是确定性抽取（不额外调模型），只有向量化需要外部 API。
        """
        if not getattr(self, "memory", None) or not self.settings.memory_enabled:
            return state
        try:
            from alertflow_agent.runtime.memory import remember as _remember

            state["memory"] = _remember(state, state.get("report") or {}, self.memory, self.embed, self.settings)
            self.store.append_event(state["run_id"], "memory.written", state["memory"])
        except Exception as exc:  # noqa: BLE001 - 记忆失败不得影响诊断
            log.warning("remember failed: %s", type(exc).__name__)
            self.store.append_event(state["run_id"], "memory.failed", {"error": type(exc).__name__})
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
