"""C3：TimelineNode + TimelineAttachment 与旧 Episode 并存（不迁移、不删旧表）。"""

from alertflow_agent.config import Settings
from alertflow_agent.memory_store import InMemoryMemoryRepo, episode_to_node
from alertflow_agent.runtime.memory import remember
from alertflow_agent.runtime.recall import recall_prior, render_block


def _state(**overrides):
    state = {
        "run_id": "run-1",
        "event": {
            "service_id": "api",
            "service_url": "https://api.example/health",
            "incident_id": 7,
            "started_at": "2026-10-11T09:00:00Z",
            "resolved_at": "2026-10-11T10:00:00Z",
            "severity": "outage",
        },
        "raw_checks": [{"id": 1, "ts": "2026-10-11T09:00:00Z", "status": "offline", "latency_ms": None}],
        "series": [],
        "interval": 60,
        "evidence": {
            "chk:1": {"source_type": "alertflow_check", "source_ref": "check:1", "kind": "http_check",
                      "data": {"status": "offline"}},
        },
    }
    state.update(overrides)
    return state


def _report(**overrides):
    report = {
        "version": 1,
        "status": "completed",
        "summary": "进程崩溃后重启",
        "verification": {"passed": True},
        "observations": [{"evidence_ids": ["chk:1"]}],
        "hypotheses": [],
    }
    report.update(overrides)
    return report


def test_remember_writes_episode_and_attachment():
    repo = InMemoryMemoryRepo()
    out = remember(_state(), _report(), repo, None, Settings())
    assert out["saved"] is True and out["attachment_saved"] is True
    assert len(repo.attachments) == 1
    att = repo.attachments[0]
    assert att["attachment_type"] == "diagnosis_report"
    assert att["anchor_type"] == "incident" and att["anchor_id"] == "7"
    assert att["trust_level"] == "validated_agent_report"
    assert att["source_refs"][0]["source_record_id"] == "chk:1"
    assert att["summary"] == "进程崩溃后重启"


def test_repeated_remember_keeps_one_attachment():
    repo = InMemoryMemoryRepo()
    remember(_state(), _report(), repo, None, Settings())
    second = remember(_state(), _report(), repo, None, Settings())
    # 唯一约束同语义：episode 覆盖，附件不重复
    assert second["attachment_saved"] is False
    assert len(repo.attachments) == 1
    assert len(repo.episodes) == 1


def test_legacy_episode_reads_still_work():
    repo = InMemoryMemoryRepo()
    remember(_state(), _report(), repo, None, Settings())
    assert repo.count_episodes("api") == 1
    recent = repo.list_recent("api", 30, 10)
    assert len(recent) == 1 and recent[0]["service_id"] == "api"
    assert repo.get_episode(recent[0]["id"])["service_id"] == "api"
    assert repo.get_profile("api")["episode_count"] == 1


def test_episode_projects_to_timeline_node_with_attachment():
    repo = InMemoryMemoryRepo()
    remember(_state(), _report(), repo, None, Settings())
    nodes = repo.timeline_nodes("api", 30, 10)
    assert len(nodes) == 1
    node = nodes[0]
    assert node.node_id.startswith("episode:")
    assert node.node_type == "agent_diagnosis"
    assert len(node.attachments) == 1
    assert node.attachments[0].attachment_type == "diagnosis_report"


def test_recall_prior_and_render_block_include_attachments():
    repo = InMemoryMemoryRepo()
    remember(_state(), _report(), repo, None, Settings())
    prior = recall_prior(_state(), repo, None, None, Settings())
    assert prior is not None
    assert len(prior["attachments"]) == 1
    block = render_block(prior)
    assert "时间轴附件" in block
    assert "diagnosis_report" in block
    assert "进程崩溃后重启" in block


def test_schema_declares_timeline_attachments():
    from pathlib import Path

    sql = Path(__file__).resolve().parents[1].joinpath("schema.sql").read_text()
    assert "CREATE TABLE IF NOT EXISTS timeline_attachments" in sql
    assert "UNIQUE (anchor_type, anchor_id, attachment_type, source_system, source_record_id)" in sql
    assert "CREATE INDEX IF NOT EXISTS timeline_attachments_service" in sql
