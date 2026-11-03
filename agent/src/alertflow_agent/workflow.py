import inspect
import json
import os
from datetime import datetime, timedelta, timezone

import httpx
from langgraph.graph import END, StateGraph

from alertflow_agent.artifacts import read_artifact, render_investigation, write_artifact
from alertflow_agent.schemas import DiagnosisReport, FaultSearchIndex, QueryTimelineInput, SearchFaultsInput

RPC_TIMEOUT = 60


class ScriptedModel:
    """没有外部模型时，按固定顺序查询时间轴和历史故障，再给出阶段性报告。"""

    def next_action(self, state: dict) -> dict:
        if not state.get("timeline_called"):
            return {"type": "query_timeline"}
        if not state.get("search_called"):
            return {"type": "search_faults", "query": state.get("service_name") or "abnormal"}
        records = state.get("timeline_records") or []
        observations = []
        for item in records[:8]:
            content = json.dumps(item.get("content") or {}, ensure_ascii=False, default=str)
            observations.append(f"{item.get('record_type')} {item.get('record_id')} {content}"[:500])
        if not observations:
            observations = ["本批次时间轴没有返回记录"]
        report = DiagnosisReport(
            summary=f"{state.get('service_name') or 'service'} 处于故障历程，输入版本 {state.get('input_version_from')}-{state.get('input_version_to')}",
            observations=observations,
            hypotheses=["可能与监控目标连续失败或应用上报的异常有关，尚未形成唯一根因"],
            unknowns=["外部模型未启用，结论停留在已观察到的记录"],
            next_steps=["核对应用日志与依赖健康"],
        )
        return {"type": "finish", "report": report.model_dump()}


class OpenAICompatibleModel:
    def __init__(self, base_url: str, api_key: str, model: str, client: httpx.AsyncClient | None = None):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.client = client

    async def next_action(self, state: dict) -> dict:
        prompt = {
            "instruction": "只输出 JSON。action 取 query_timeline、search_faults 或 finish。finish 时 report 必须含 summary、observations、hypotheses、unknowns、next_steps。",
            "state": {k: state.get(k) for k in ("episode_id", "service_name", "input_version_from", "input_version_to", "timeline_called", "search_called", "timeline_records", "search_hits")},
        }
        body = {
            "model": self.model,
            "messages": [{"role": "user", "content": json.dumps(prompt, ensure_ascii=False, default=str)}],
            "response_format": {"type": "json_object"},
        }
        payload = await _post_json(self.client, f"{self.base_url}/chat/completions", body, self.api_key)
        action = json.loads(payload["choices"][0]["message"]["content"])
        if action.get("type") == "finish":
            DiagnosisReport.model_validate(action["report"])
        return action


def model_from_env(client: httpx.AsyncClient | None = None):
    key = os.environ.get("ALERTFLOW_LLM_API_KEY", "")
    if not key:
        return ScriptedModel()
    return OpenAICompatibleModel(
        os.environ.get("ALERTFLOW_LLM_BASE_URL", "https://api.openai.com/v1"),
        key,
        os.environ.get("ALERTFLOW_LLM_MODEL", "gpt-4o-mini"),
        client,
    )


async def _call_action(model, state: dict) -> dict:
    action = model.next_action(state)
    if inspect.isawaitable(action):
        action = await action
    return action


def build_diagnosis_graph(model, gateway, checkpointer=None):
    graph = StateGraph(dict)

    async def load(state: dict):
        existing = await read_artifact(gateway.db, state["episode_id"], "INVESTIGATION")
        state["investigation_revision"] = 0 if existing is None else existing["version"]
        state["current_stage"] = "decide"
        return state

    async def decide(state: dict):
        if state.get("tool_rounds", 0) >= 4:
            action = ScriptedModel().next_action({**state, "timeline_called": True, "search_called": True})
        else:
            action = await _call_action(model, state)
        state["action"] = action
        state["current_stage"] = action["type"]
        return state

    def route(state: dict) -> str:
        return state["action"]["type"]

    async def query_node(state: dict):
        started = state.get("episode_started_at") or datetime.now(timezone.utc).isoformat()
        start = datetime.fromisoformat(started)
        if start.tzinfo is None:
            start = start.replace(tzinfo=timezone.utc)
        end = datetime.now(timezone.utc) + timedelta(minutes=1)
        parsed = QueryTimelineInput(
            episode_id=state["episode_id"], start_time=start, end_time=end,
            monitor_point_ids=state.get("monitor_point_ids") or [],
            app_event_ids=state.get("app_event_ids") or [],
        )
        records = await gateway.query_timeline(state, parsed)
        state["timeline_records"] = records
        state["timeline_called"] = True
        state["tool_rounds"] = state.get("tool_rounds", 0) + 1
        state["current_stage"] = "decide"
        return state

    async def search_node(state: dict):
        query = SearchFaultsInput(query=state["action"].get("query") or "abnormal")
        state["search_hits"] = await gateway.search_faults(state, query.query)
        state["search_called"] = True
        state["tool_rounds"] = state.get("tool_rounds", 0) + 1
        state["current_stage"] = "decide"
        return state

    async def write_node(state: dict):
        report = DiagnosisReport.model_validate(state["action"]["report"])
        findings = json.dumps(state.get("timeline_records") or [], ensure_ascii=False, default=str)[:4000]
        existing = await read_artifact(gateway.db, state["episode_id"], "INVESTIGATION")
        text = render_investigation(
            state.get("service_name") or "",
            state.get("episode_started_at") or "",
            (existing or {}).get("content_text"),
            state["task_id"], state["execution_generation"], findings,
            report.summary, "\n".join(f"- {item}" for item in report.unknowns) or "无",
        )
        current = await read_artifact(gateway.db, state["episode_id"], "INVESTIGATION")
        expected = None if current is None else current["version"]
        saved = await write_artifact(gateway.db, state["episode_id"], "INVESTIGATION", "MARKDOWN", text, None,
                                     state["task_id"], state["execution_generation"], expected)
        await gateway.register_artifact(state, "INVESTIGATION", saved["version"], "MARKDOWN", saved["content_hash"])
        report_current = await read_artifact(gateway.db, state["episode_id"], "DIAGNOSIS_REPORT")
        report_expected = None if report_current is None else report_current["version"]
        report_saved = await write_artifact(
            gateway.db, state["episode_id"], "DIAGNOSIS_REPORT", "JSON", None, report.model_dump(),
            state["task_id"], state["execution_generation"], report_expected,
        )
        await gateway.register_artifact(state, "DIAGNOSIS_REPORT", report_saved["version"], "JSON", report_saved["content_hash"])
        state["investigation_revision"] = saved["version"]
        state["report_revision"] = report_saved["version"]
        state["current_stage"] = "done"
        return state

    graph.add_node("load", load)
    graph.add_node("decide", decide)
    graph.add_node("query_timeline", query_node)
    graph.add_node("search_faults", search_node)
    graph.add_node("write", write_node)
    graph.set_entry_point("load")
    graph.add_edge("load", "decide")
    graph.add_conditional_edges("decide", route, {"query_timeline": "query_timeline", "search_faults": "search_faults", "finish": "write"})
    graph.add_edge("query_timeline", "decide")
    graph.add_edge("search_faults", "decide")
    graph.add_edge("write", END)
    return graph.compile(checkpointer=checkpointer)


async def run_diagnosis(model, gateway, task: dict, checkpointer=None) -> dict:
    app = build_diagnosis_graph(model, gateway, checkpointer)
    state = {
        "episode_id": task["episode_id"],
        "task_id": task["task_id"],
        "execution_generation": task["execution_generation"],
        "input_version_from": task.get("input_version_from") or 0,
        "input_version_to": task.get("input_version_to") or 0,
        "service_name": task.get("service_name") or "",
        "service_id": task.get("service_id") or "",
        "episode_started_at": task.get("episode_started_at"),
        "monitor_point_ids": task.get("monitor_point_ids") or [],
        "app_event_ids": task.get("app_event_ids") or [],
        "tool_rounds": 0,
        "current_stage": "load",
    }
    return await app.ainvoke(state, config={"configurable": {"thread_id": task["thread_id"]}})


async def run_archive(model, gateway, task: dict) -> str:
    investigation = await read_artifact(gateway.db, task["episode_id"], "INVESTIGATION") or {}
    report_row = await read_artifact(gateway.db, task["episode_id"], "DIAGNOSIS_REPORT") or {}
    report = report_row.get("content_json") or {}
    index = model.extract_index(investigation.get("content_text") or "", report)
    if inspect.isawaitable(index):
        index = await index
    return await gateway.save_fault(task, investigation.get("content_text") or "", report, index)


class ArchiveModel:
    def __init__(self, client: httpx.AsyncClient | None = None):
        self.client = client

    def extract_index(self, investigation: str, report: dict):
        if os.environ.get("ALERTFLOW_LLM_API_KEY"):
            return self._remote(investigation, report)
        return FaultSearchIndex(
            summary=str(report.get("summary") or investigation[:200] or "无摘要"),
            symptoms=list(report.get("observations") or []),
            observations=list(report.get("observations") or []),
            diagnosis=list(report.get("hypotheses") or []),
        )

    async def _remote(self, investigation: str, report: dict) -> FaultSearchIndex:
        model = OpenAICompatibleModel(
            os.environ.get("ALERTFLOW_LLM_BASE_URL", "https://api.openai.com/v1"),
            os.environ["ALERTFLOW_LLM_API_KEY"],
            os.environ.get("ALERTFLOW_LLM_MODEL", "gpt-4o-mini"),
            self.client,
        )
        body = {
            "model": model.model,
            "messages": [{"role": "user", "content": json.dumps({
                "instruction": "只根据材料提取 FaultSearchIndex，字段 summary,symptoms,observations,diagnosis。不要新增分类字段。",
                "investigation": investigation[:8000],
                "diagnosis_report": report,
            }, ensure_ascii=False)}],
            "response_format": {"type": "json_object"},
        }
        payload = await _post_json(model.client, f"{model.base_url}/chat/completions", body, model.api_key)
        return FaultSearchIndex.model_validate(json.loads(payload["choices"][0]["message"]["content"]))


async def _post_json(client: httpx.AsyncClient | None, url: str, body: dict, api_key: str) -> dict:
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
    if client is None:
        async with httpx.AsyncClient(timeout=RPC_TIMEOUT) as owned:
            response = await owned.post(url, json=body, headers=headers)
    else:
        response = await client.post(url, json=body, headers=headers)
    response.raise_for_status()
    return response.json()
