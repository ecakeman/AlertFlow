import os
import uuid
from datetime import datetime, timezone

import pytest

os.environ.setdefault("ALERTFLOW_DATABASE_URL", "postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")

from alertflow_agent.fd import connect, save_fault_document, search_faults, stable_fd_id
from alertflow_agent.receipts import accept_task
from alertflow_agent.schemas import FaultSearchIndex, QueryTimelineInput
from alertflow_agent.textutil import rrf
from alertflow_agent.workflow import ScriptedModel, run_archive, run_diagnosis


class FakeGateway:
    def __init__(self, db):
        self.db = db
        self.registered = []

    def query_timeline(self, state, parsed):
        return [{
            "record_type": "APP_EVENT",
            "record_id": "rec-1",
            "event_time": datetime.now(timezone.utc).isoformat(),
            "received_at": datetime.now(timezone.utc).isoformat(),
            "source": "APP",
            "content": {"event_kind": "HEALTH_STATUS", "health_status": "ABNORMAL"},
        }]

    def search_faults(self, state, query):
        return []

    def register_artifact(self, state, artifact_type, version, content_type, content_hash):
        self.registered.append(artifact_type)

    def save_fault(self, task, investigation, report, index):
        return save_fault_document(self.db, task["episode_id"], task["service_id"], investigation, report, index)


@pytest.fixture()
def db():
    conn = connect()
    with conn.cursor() as cur:
        cur.execute("TRUNCATE cognitive.fault_documents, cognitive.episode_artifacts, cognitive.agent_task_receipts")
    conn.commit()
    yield conn
    conn.close()


def test_rrf_dedup():
    scores = rrf([["a", "b"], ["b", "c"]])
    assert scores["b"] > scores["a"]


def test_query_timeline_requires_timezone():
    with pytest.raises(ValueError):
        QueryTimelineInput(episode_id="e", start_time=datetime.now(), end_time=datetime.now())


def test_receipt_is_idempotent(db):
    task = {"task_id": str(uuid.uuid4()), "episode_id": str(uuid.uuid4()), "task_type": "DIAGNOSIS",
            "execution_generation": 1, "input_version_from": 1, "input_version_to": 1}
    first = accept_task(db, task)
    second = accept_task(db, task)
    assert first["duplicate"] is False
    assert second["duplicate"] is True
    task["execution_generation"] = 2
    third = accept_task(db, task)
    assert third["duplicate"] is False and third["status"] == "ACCEPTED"


def test_diagnosis_writes_artifacts_once_per_task(db):
    gateway = FakeGateway(db)
    episode_id = str(uuid.uuid4())
    task = {
        "task_id": str(uuid.uuid4()), "episode_id": episode_id, "task_type": "DIAGNOSIS",
        "execution_generation": 1, "input_version_from": 1, "input_version_to": 1,
        "thread_id": f"diagnosis:{episode_id}", "service_name": "orders",
        "episode_started_at": datetime.now(timezone.utc).isoformat(),
    }
    run_diagnosis(ScriptedModel(), gateway, task)
    run_diagnosis(ScriptedModel(), gateway, task)
    with db.cursor() as cur:
        cur.execute("SELECT content_text FROM cognitive.episode_artifacts WHERE episode_id=%s AND artifact_type='INVESTIGATION'", (episode_id,))
        text = cur.fetchone()[0]
    assert text.count(task["task_id"]) == 1
    assert gateway.registered.count("INVESTIGATION") == 2


def test_archive_search_roundtrip(db):
    gateway = FakeGateway(db)
    episode = str(uuid.uuid4())
    service = str(uuid.uuid4())
    task = {"episode_id": episode, "service_id": service, "task_id": str(uuid.uuid4()), "execution_generation": 1}
    from alertflow_agent.artifacts import write_artifact
    write_artifact(db, episode, "INVESTIGATION", "MARKDOWN", "# 故障概况\n\npool exhausted\n", None, task["task_id"], 1, None)
    write_artifact(db, episode, "DIAGNOSIS_REPORT", "JSON", None, {
        "summary": "连接池耗尽", "observations": ["pool exhausted"], "hypotheses": ["数据库连接池耗尽"],
        "unknowns": [], "next_steps": [],
    }, task["task_id"], 1, None)
    fd_id = run_archive(type("M", (), {"extract_index": lambda self, inv, report: FaultSearchIndex(summary=report["summary"], symptoms=report["observations"], observations=report["observations"], diagnosis=report["hypotheses"])})(), gateway, task)
    assert fd_id == stable_fd_id(episode)
    again = save_fault_document(db, episode, service, "doc", {"summary": "连接池耗尽"}, FaultSearchIndex(summary="连接池耗尽", symptoms=["pool exhausted"], observations=["pool exhausted"], diagnosis=["数据库连接池耗尽"]))
    assert again == fd_id
    hits = search_faults(db, service, "连接池耗尽")
    assert hits and hits[0]["fd_id"] == fd_id
    assert "investigation_content" in hits[0]
