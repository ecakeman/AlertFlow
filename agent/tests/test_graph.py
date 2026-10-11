from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from tests.fakes import FakeAlertFlow, event_row


def test_baseline_run_persists_a_verified_report():
    store = MemoryStore()
    row = event_row("baseline")
    store.insert_run(row)
    runner = Runner(Settings(max_revisions=2), store, FakeAlertFlow(), ScriptedModel())
    runner.execute(row["id"])
    saved = store.get(row["id"])
    report = store.latest_report(row["id"])
    assert saved["status"] == "completed"
    assert report["version"] == 1
    assert report["report"]["verification"]["passed"] is True
    assert "ana:status@0" in store.get_evidence(row["id"])


def test_agent_finish_is_the_exit():
    store = MemoryStore()
    row = event_row("agent")
    row["input_event"]["script"] = ["finish"]
    store.insert_run(row)
    runner = Runner(Settings(), store, FakeAlertFlow(), ScriptedModel())
    runner.execute(row["id"])
    assert store.get(row["id"])["status"] == "completed"
    assert store.latest_report(row["id"])["report"]["usage"]["tool_calls"] == 0
