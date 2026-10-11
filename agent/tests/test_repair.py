import copy
import threading
import uuid

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.main import _loop, build
from alertflow_agent.runtime.context import build_context
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from tests.fakes import FakeAlertFlow, event_row


class _MetricsDown(FakeAlertFlow):
    def metrics(self, service_id: str, range_: str = "24h") -> dict:
        raise RuntimeError("metrics down")


class _CollectDown(FakeAlertFlow):
    def incident(self, incident_id: int) -> dict:
        raise RuntimeError("incident down")

    def checks(self, service_id: str, before, limit: int) -> dict:
        raise RuntimeError("checks down")


class _TwoIncidents(FakeAlertFlow):
    def incidents(self, service_id: str, limit: int = 10) -> dict:
        return {"incidents": [
            {"id": 1, "serviceId": service_id, "status": "ongoing", "severity": "outage", "title": "API down"},
            {"id": 2, "serviceId": service_id, "status": "resolved", "severity": "outage", "title": "earlier"},
        ], "total": 2}

    def incident(self, incident_id: int) -> dict:
        body = super().incident(incident_id)
        body["id"] = incident_id
        return body


def _run(mode="agent", script=None, settings=None, client=None, **event):
    store = MemoryStore()
    row = event_row(mode)
    row["input_event"].update(event)
    if script is not None:
        row["input_event"]["script"] = script
    store.insert_run(row)
    model = ScriptedModel()
    runner = Runner(settings or Settings(), store, client or FakeAlertFlow(), model)
    runner.execute(row["id"])
    return store, row, model, runner


def test_one_source_failure_keeps_other_evidence():
    store, row, _, _ = _run("baseline", client=_MetricsDown())
    evidence = store.get_evidence(row["id"])
    assert "chk:11" in evidence
    assert "inc:1" in evidence
    assert store.get(row["id"])["status"] == "completed"
    failed, failed_row, _, _ = _run("baseline", client=_CollectDown())
    assert failed.get(failed_row["id"])["status"] == "failed"
    assert failed.get(failed_row["id"])["failure_reason"] == "collect_failed"


def test_plain_text_does_not_finish_and_finish_is_the_exit():
    store, row, _, _ = _run(script=["text"])
    saved = store.get(row["id"])
    assert saved["status"] == "partial"
    assert saved["stop_reason"] == "no_progress"
    assert store.latest_report(row["id"])["report"]["usage"]["tool_calls"] == 0


def test_replan_success_illegal_and_retry():
    store, row, _, _ = _run(script=["replan", "finish"])
    kinds = [event["type"] for event in store.list_events(row["id"])]
    assert "plan.revised" in kinds
    assert "tool.completed" in kinds
    assert store.get(row["id"])["status"] == "completed"
    bad, bad_row, _, _ = _run(script=["replan", "finish"], replan_bad=True)
    assert any(event["type"] == "plan.rejected" for event in bad.list_events(bad_row["id"]))
    assert bad.get(bad_row["id"])["status"] == "completed"


def test_budget_writes_partial():
    store, row, _, _ = _run(script=["finish"], settings=Settings(max_llm_calls=1))
    saved = store.get(row["id"])
    assert saved["status"] == "partial"
    assert saved["stop_reason"] == "budget_llm_calls"
    assert store.latest_report(row["id"])["report"]["summary"]


def test_skill_text_enters_later_context_and_subagent_is_isolated():
    _, _, model, _ = _run(script=["load_skill", "finish"])
    lead_prompts = [prompt for node, prompt in model.prompts if node == "lead"]
    assert "does not prove a database failure" not in lead_prompts[0]
    assert "does not prove a database failure" in lead_prompts[1]
    store, row, model, _ = _run(script=["history", "finish"], client=_TwoIncidents())
    sub = [prompt for node, prompt in model.prompts if node == "subagent"]
    assert sub
    assert "Explain the monitoring event" not in sub[0]
    assert "history_review.question" in sub[0]
    assert "rev:1" in store.get_evidence(row["id"])
    parent = {"event": {"event": "incident_start", "message": "secret"}, "plan": {"goal": "parent-only"}, "evidence": {}, "messages": [{"role": "ai", "content": "parent-message"}], "skills_loaded": []}
    isolated = build_context("subagent", {**parent, "question": "compare", "plan": parent["plan"], "messages": []})
    assert "parent-only" not in isolated
    assert "parent-message" not in isolated


def test_repeated_tool_stops_on_the_third_call():
    store, row, _, _ = _run(script=["get_metrics_series"])
    assert store.get(row["id"])["stop_reason"] == "repeated_tool_call"
    assert store.get(row["id"])["usage"]["tool_calls"] == 3


def test_concurrent_insert_returns_the_existing_run():
    store = MemoryStore()
    created = []

    def attempt():
        row = event_row("agent")
        row["id"] = str(uuid.uuid4())
        row["dedupe_key"] = "same"
        created.append(store.insert_run(copy.deepcopy(row)) is None)

    threads = [threading.Thread(target=attempt) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert created.count(True) == 1
    assert len(store.list_runs()) == 1


def test_resume_uses_checkpoint_and_records_a_missing_one():
    store, row, _, runner = _run("baseline")
    before = len(store.list_events(row["id"]))
    store.transition(row["id"], {"completed"}, status="queued", stage="resume")
    runner.execute(row["id"], resume=True)
    assert not any(event["type"] == "run.resumed_without_checkpoint" for event in store.list_events(row["id"])[before:])
    store.transition(row["id"], {"completed", "partial", "queued"}, status="queued", stage="resume")
    Runner(Settings(), store, FakeAlertFlow(), ScriptedModel()).execute(row["id"], resume=True)
    assert any(event["type"] == "run.resumed_without_checkpoint" for event in store.list_events(row["id"]))
    assert store.get(row["id"])["status"] == "completed"


def test_cancel_is_not_overwritten():
    store = MemoryStore()
    row = event_row("baseline")
    store.insert_run(row)
    store.update(row["id"], status="cancelled")
    runner = Runner(Settings(), store, FakeAlertFlow(), ScriptedModel())
    runner.execute(row["id"])
    assert store.get(row["id"])["status"] == "cancelled"
    assert store.finish(row["id"], status="completed") is False


def test_startup_marks_running_for_resume_and_refuses_a_partial_model_config():
    store = MemoryStore()
    row = event_row("agent")
    row["status"] = "running"
    store.insert_run(row)
    stop = threading.Event()
    stop.set()
    _loop(Settings(), store, runner=None, stop=stop)
    assert store.get(row["id"])["status"] == "queued"
    assert store.get(row["id"])["stage"] == "resume"
    try:
        build(Settings(llm_api_key="secret", llm_model="", llm_base_url=""))
    except RuntimeError as exc:
        assert "LLM_MODEL" in str(exc)
    else:
        raise AssertionError("empty model config should fail")


def test_resume_api_rejects_running_and_completed():
    from fastapi.testclient import TestClient

    from alertflow_agent.api.app import create_app

    store = MemoryStore()
    row = event_row("agent")
    store.insert_run(row)
    store.update(row["id"], status="running")
    client = TestClient(create_app(Settings(agent_api_token="api"), store, runner=None))
    assert client.post(f"/api/v1/runs/{row['id']}/resume", headers={"Authorization": "Bearer api"}).status_code == 409
    store.update(row["id"], status="completed")
    assert client.post(f"/api/v1/runs/{row['id']}/resume", headers={"Authorization": "Bearer api"}).status_code == 409
    store.update(row["id"], status="failed")
    assert client.post(f"/api/v1/runs/{row['id']}/resume", headers={"Authorization": "Bearer api"}).status_code == 202
    assert store.get(row["id"])["stage"] == "resume"
