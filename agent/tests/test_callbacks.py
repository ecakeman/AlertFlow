import uuid

import grpc

from alertflow_agent.callbacks import (
    MAX_EVIDENCE_ITEMS,
    CallbackSender,
    backoff_seconds,
    bounded_evidence,
    failure_callback,
    report_callback,
)
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore


def _run(store, run_id=None):
    run_id = run_id or str(uuid.uuid4())
    store.insert_run({
        "id": run_id, "dedupe_key": f"incident_start:{run_id}", "incident_id": 7,
        "service_id": "api", "event_type": "incident_start", "mode": "agent",
        "status": "running", "stage": "running", "input_event": {"occurred_at": "2026-10-11T10:00:00Z"}, "usage": {},
    })
    return run_id


def _settings(**overrides):
    base = dict(agent_callback_token="cb", alertflow_grpc_target="alertflow:9090")
    base.update(overrides)
    return Settings(**base)


def test_report_save_enqueues_the_callback_in_the_same_step():
    store = MemoryStore()
    run_id = _run(store)
    callback = report_callback(run_id, store.get(run_id), 1, {"summary": "s", "status": "completed"}, {"tokens": 12}, {})
    assert store.commit_report(run_id, 1, {"summary": "s", "status": "completed"}, "completed", None, {}, callback=callback) == "saved"
    rows = store.callback_rows()
    assert len(rows) == 1
    assert rows[0]["message_id"] == f"run:{run_id}:v1"
    assert rows[0]["status"] == "pending"
    # 重复入队是幂等的。
    assert store.enqueue_callback(callback) is False
    assert len(store.callback_rows()) == 1


def test_finish_enqueues_a_failure_callback():
    store = MemoryStore()
    run_id = _run(store)
    callback = failure_callback(run_id, store.get(run_id), "llm_error")
    assert store.finish(run_id, callback=callback, status="failed", failure_reason="llm_error", stage="failed") is True
    rows = store.callback_rows()
    assert rows[0]["message_type"] == "run.failed"
    assert rows[0]["payload"]["payload"]["error"] == "llm_error"


def test_cancelled_run_enqueues_nothing():
    store = MemoryStore()
    run_id = _run(store)
    store.update(run_id, status="cancelled")
    callback = failure_callback(run_id, store.get(run_id), "boom")
    assert store.finish(run_id, callback=callback, status="failed") is False
    assert store.callback_rows() == []


def test_claim_marks_dead_after_the_attempt_limit():
    store = MemoryStore()
    run_id = _run(store)
    store.enqueue_callback(report_callback(run_id, store.get(run_id), 1, {"summary": "s"}, {}, {}))
    for _ in range(3):
        claimed = store.claim_callbacks(10, 60, max_attempts=3)
        for row in claimed:
            store.finish_callback(row["id"], "retry_wait", "boom", 0)
    assert store.claim_callbacks(10, 60, max_attempts=3) == []
    assert store.callback_rows()[0]["status"] == "dead"


def test_drain_posts_and_marks_sent():
    store = MemoryStore()
    run_id = _run(store)
    store.enqueue_callback(report_callback(run_id, store.get(run_id), 1, {"summary": "s"}, {}, {}))
    posted = {}

    def fake_submit(payload):
        posted["json"] = payload
        return {"stored": True, "duplicate": False}

    sender = CallbackSender(_settings(), store)
    sender.submit = fake_submit
    assert sender.configured() is True
    assert sender.drain() == 1
    assert posted["json"]["message_id"] == f"run:{run_id}:v1"
    assert store.callback_rows()[0]["status"] == "sent"


class _Rejected(grpc.RpcError):
    def code(self):
        return grpc.StatusCode.INVALID_ARGUMENT


def test_drain_retries_on_transport_error_and_gives_up_on_4xx():
    store = MemoryStore()
    first, second = _run(store), _run(store)
    store.enqueue_callback(report_callback(first, store.get(first), 1, {"summary": "a"}, {}, {}))
    store.enqueue_callback(report_callback(second, store.get(second), 1, {"summary": "b"}, {}, {}))

    def boom(payload):
        if payload["payload"]["report"]["summary"] == "a":
            raise RuntimeError("connection refused")
        raise _Rejected()

    sender = CallbackSender(_settings(), store)
    sender.submit = boom
    assert sender.drain() == 0
    statuses = {row["payload"]["payload"]["report"]["summary"]: row["status"] for row in store.callback_rows()}
    assert statuses["a"] == "retry_wait"
    # INVALID_ARGUMENT 是永久失败：重试同样的请求没有意义。
    assert statuses["b"] == "dead"


def test_backoff_is_bounded():
    assert backoff_seconds(1) == 5
    assert backoff_seconds(2) == 10
    assert backoff_seconds(20) == 300


def test_only_the_new_grpc_credentials_enqueue_every_callback():
    from alertflow_agent.chat_worker import ChatWorker
    from alertflow_agent.runtime.graph import Runner
    from alertflow_agent.runtime.llm import ScriptedModel

    settings = Settings(alertflow_grpc_target="alertflow:9090", alertflow_agent_grpc_token="tok", llm_api_key="k")
    store = MemoryStore()
    run_id = _run(store)
    runner = Runner(settings, store, None, ScriptedModel())
    row = store.get(run_id)
    runner._notify_started(run_id, row)
    report = runner._report_callback(row, 1, {"status": "completed", "summary": "s"}, [], {})
    failed = runner._failure_callback(run_id, "boom")
    assert report["message_type"] == "run.completed"
    assert failed["message_type"] == "run.failed"
    store.enqueue_callback(report)
    store.enqueue_callback(failed)
    store.enqueue_chat_request({"request_id": "chat-1", "incident_id": 7, "message": "why?", "history": []})

    class Answer:
        def answer(self, prompt):
            return "because"

    assert ChatWorker(settings, store, type("R", (), {"settings": settings, "model": Answer(), "memory": None})()).drain() == 1
    kinds = {item["message_type"] for item in store.callback_rows()}
    assert kinds == {"run.started", "run.completed", "run.failed", "chat.completed"}


def test_callbacks_stay_out_without_a_grpc_target_or_token():
    from alertflow_agent.runtime.graph import Runner
    from alertflow_agent.runtime.llm import ScriptedModel

    for settings in (
        Settings(alertflow_agent_grpc_token="tok"),
        Settings(alertflow_grpc_target="alertflow:9090"),
    ):
        store = MemoryStore()
        run_id = _run(store)
        runner = Runner(settings, store, None, ScriptedModel())
        runner._notify_started(run_id, store.get(run_id))
        assert runner._report_callback(store.get(run_id), 1, {"status": "completed"}, [], {}) is None
        assert runner._failure_callback(run_id, "boom") is None
        assert store.callback_rows() == []


def test_unconfirmed_callback_stays_retryable():
    store = MemoryStore()
    run_id = _run(store)
    store.enqueue_callback(report_callback(run_id, store.get(run_id), 1, {"summary": "s"}, {}, {}))
    sender = CallbackSender(_settings(), store)
    sender.submit = lambda payload: {"stored": False, "duplicate": False}
    assert sender.drain() == 0
    assert store.callback_rows()[0]["status"] == "retry_wait"


def test_unconfigured_sender_is_inert():
    sender = CallbackSender(Settings(agent_callback_token="", alertflow_base_url=""), MemoryStore())
    assert sender.configured() is False


def test_bounded_evidence_caps_items_and_drops_data():
    evidence = {}
    for i in range(MAX_EVIDENCE_ITEMS + 20):
        evidence[f"ev:{i}"] = {"summary": "s", "data": {"blob": "x" * 50}}
    assert len(bounded_evidence(evidence)) == MAX_EVIDENCE_ITEMS

    huge = {"one": {"summary": "s", "data": {"blob": "x" * (300 * 1024)}}}
    out = bounded_evidence(huge)
    assert "data" not in out["one"]
    assert out["one"]["summary"] == "s"
