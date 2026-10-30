"""remember 真实路径回归：Go 下推的 float 数字不得把记忆写入打挂。"""

import json
import os

import pytest

from alertflow_agent.analysis.stats import error_stats
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.integrations.alertflow import normalize_check
from alertflow_agent.memory_store import InMemoryMemoryRepo, MemoryRepo
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from alertflow_agent.runtime.memory import build_card, remember
from tests.fakes import FakeAlertFlow, event_row


def _p6_checks():
    """P6 真实 run 里 evidence 的检查行形状：id/attempts/statusCode 都是 float。"""
    return [
        {"id": 2.0, "ts": 1793367339, "status": "offline", "attempts": 3.0, "latency_ms": 37.0,
         "status_code": None, "error": "lookup fault-lab.fault-lab.svc on 10.96.0.10:53: no such host"},
        {"id": 11.0, "ts": 1793367350, "status": "offline", "attempts": 1.0, "latency_ms": 15.0,
         "status_code": 503.0, "error": "Service Unavailable"},
    ]


def _p6_state():
    report = {
        "summary": "fault-lab 在窗口内间歇性 outage，19 次 DNS 失败加 5 次 503。",
        "verification": {"passed": True},
        "status": "completed",
        "version": 1,
    }
    return {
        "run_id": "624d23a8-033f-486b-86a4-ad05a13e67f8",
        "event": {
            "event": "incident_start", "service_id": "fault-lab", "service_name": "Fault Lab",
            "status": "down", "severity": "outage", "occurred_at": "2026-10-30T13:52:57Z",
            "started_at": "2026-10-30T13:52:57Z", "resolved_at": "2026-10-30T13:53:57Z",
            "incident_id": 4.0, "error": "Service Unavailable",
        },
        "raw_checks": _p6_checks(),
        "series": [{"ts": 1793367000.0, "avg_latency": None}],
        "interval": 60,
        "evidence": {
            "chk:2.0": {"source_type": "alertflow_check", "source_ref": "chk:2.0", "data": _p6_checks()[0]},
            "chk:11.0": {"source_type": "alertflow_check", "source_ref": "chk:11.0", "data": _p6_checks()[1]},
        },
        "report": report,
    }


def test_normalize_check_coerces_go_floats():
    row = normalize_check({"id": 2.0, "ts": 1793367339, "status": "offline", "attempts": 3.0,
                           "latencyMs": 37.0, "statusCode": 503.0, "error": "Service Unavailable"})
    assert row == {"id": 2, "ts": 1793367339, "status": "offline", "latency_ms": 37,
                   "status_code": 503, "error": "Service Unavailable", "attempts": 3}
    assert normalize_check({"id": 3.0, "ts": 1, "statusCode": None, "attempts": 0})["attempts"] == 1


def test_error_stats_treats_float_and_int_codes_alike():
    stats = error_stats(_p6_checks(), lambda text, code: "http_status" if code else "dns")
    assert stats["status_codes"] == {"503": 1}


def test_build_card_accepts_float_status_codes():
    state = _p6_state()
    card = build_card(state, state["report"])
    assert card.service_id == "fault-lab"
    assert card.conclusion.layer in ("network_dns", "probe", "app_internal", "dependency", "unknown", "tls")
    # 关键回归点：503.0 必须被记成 503，而不是抛 ValueError
    assert card.symptom.status_codes == [503]


def test_remember_writes_episode_and_attachment():
    repo = InMemoryMemoryRepo()
    out = remember(_p6_state(), _p6_state()["report"], repo, None, Settings(memory_enabled=True))
    assert out["saved"] is True
    assert len(repo.episodes) == 1
    assert len(repo.attachments) == 1
    attachment = repo.attachments[0]
    assert attachment["anchor_type"] == "incident"
    assert attachment["anchor_id"] == "4"
    assert attachment["attachment_type"] == "diagnosis_report"
    assert attachment["trust_level"] == "validated_agent_report"
    # 同一 run 重写：episode 版本递增，attachment 幂等
    out2 = remember(_p6_state(), _p6_state()["report"], repo, None, Settings(memory_enabled=True))
    assert out2["attachment_saved"] is False
    assert len(repo.episodes) == 1
    assert len(repo.attachments) == 1


def test_full_run_remember_step_saves_memory():
    store = MemoryStore()
    row = event_row("agent")
    store.insert_run(row)
    repo = InMemoryMemoryRepo()
    settings = Settings(memory_enabled=True)
    Runner(settings, store, FakeAlertFlow(), ScriptedModel(), memory=repo).execute(row["id"])
    assert any(event["type"] == "memory.written" for event in store.list_events(row["id"]))
    assert not any(event["type"] == "memory.failed" for event in store.list_events(row["id"]))
    assert len(repo.episodes) == 1
    assert len(repo.attachments) == 1


@pytest.mark.skipif(not os.environ.get("ALERTFLOW_TEST_DATABASE_URL"),
                    reason="ALERTFLOW_TEST_DATABASE_URL is not set")
def test_remember_writes_to_postgres():
    url = os.environ["ALERTFLOW_TEST_DATABASE_URL"]
    repo = MemoryRepo(url)
    run_id = "624d23a8-033f-486b-86a4-ad05a13e67f8"
    # Agent DB 的 episodes.run_id 有外键：测试数据必须先有对应 run 行（CI 里由夹具准备）
    try:
        repo.conn.execute("select 1")
    except Exception:  # noqa: BLE001
        pytest.skip("database is not reachable")
    out = remember(_p6_state(), _p6_state()["report"], repo, None, Settings(memory_enabled=True))
    assert out["saved"] is True
    # attachment 以唯一键幂等：重跑时 attachment_saved 可能为 False，但数据必须存在
    assert repo.list_attachments("fault-lab", limit=5)
    assert repo.get_profile("fault-lab") is not None
    assert repo.list_recent("fault-lab", 30, 10)
