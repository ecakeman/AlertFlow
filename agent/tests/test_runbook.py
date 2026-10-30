import socket
import threading
import time

import httpx
import pytest
import uvicorn

from alertflow_agent.a2a_adapter.ragflow import parse_completion
from alertflow_agent.a2a_adapter.server import RunbookExecutor, build_app, build_card
from alertflow_agent.a2a_adapter.store import MemoryLedger
from alertflow_agent.config import Settings
from alertflow_agent.runtime.context import build_blocks
from alertflow_agent.runtime.tools import run_tool


class Queue:
    def __init__(self):
        self.events = []

    async def enqueue_event(self, event):
        self.events.append(event)


class Message:
    def __init__(self, message_id, text):
        self.message_id = message_id
        self.task_id = "task-1"
        self.parts = [type("Part", (), {"text": text})()]


class Context:
    def __init__(self, message_id="m1"):
        self.message = Message(message_id, "how do I restart the API")
        self.task_id = "task-1"
        self.context_id = "ctx-1"


def test_card_declares_only_runbook_research():
    card = build_card("http://a2a-adapter:8000")
    assert [skill.id for skill in card.skills] == ["runbook_research"]
    assert list(card.default_input_modes) == ["application/json"]
    assert card.supported_interfaces[0].protocol_binding == "HTTP+JSON"


def test_missing_ragflow_api_key_does_not_post(monkeypatch):
    import httpx
    from alertflow_agent.a2a_adapter.ragflow import research

    def boom(*args, **kwargs):
        raise AssertionError("ragflow was called")

    monkeypatch.setattr(httpx, "post", boom)
    with pytest.raises(RuntimeError, match="not configured"):
        research("http://ragflow.test", "", "agent-1", "restart", 1)


def test_uncited_completion_is_not_a_citation():
    artifact = parse_completion({"code": 0, "data": {"data": {"content": "see doc-1", "reference": {"chunks": [{"content": "doc-1"}]}}}})
    assert artifact["citations_available"] is False
    assert artifact["citations"] == []
    cited = parse_completion({"code": 0, "data": {"data": {"content": "ok", "reference": {"chunks": [{"document_id": "doc", "id": "chunk"}]}}}})
    assert cited["citations_available"] is True


def test_repeat_message_does_not_call_ragflow_again():
    calls = []

    def research(base_url, api_key, agent_id, query, timeout):
        calls.append(query)
        return {"schema": "runbook-research.v1", "content": "step", "citations": [{"document_id": "doc", "id": "chunk"}], "citations_available": True}

    import alertflow_agent.a2a_adapter.server as server
    server.research = research
    ledger = MemoryLedger()
    executor = RunbookExecutor(ledger, {"ragflow_base_url": "http://ragflow:9380", "ragflow_agent_id": "agent", "ragflow_api_key": "token", "timeout_seconds": 5})
    import asyncio

    async def twice():
        await executor.execute(Context(), Queue())
        await executor.join()
        await executor.execute(Context(), Queue())

    asyncio.run(twice())
    assert calls == ["how do I restart the API"]
    assert ledger.get_message("m1")["status"] == "completed"
    assert ledger.get_message("m1")["artifact"]["citations_available"] is True


def test_empty_adapter_url_does_not_call_the_client():
    calls = []
    state = {"event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"}, "evidence": {}, "run_id": "run-1", "plan": {"gaps": []}}
    result = run_tool("consult_runbook_agent", {"question": "why"}, state, Settings(a2a_adapter_url=""), {"a2a": lambda *args: calls.append(args)})
    assert result["status"] == "invalid_request"
    assert calls == []
    quiet = build_blocks("lead", {"event": {}})
    assert all(block.name != "runbook_tool" for block in quiet)


def test_one_consult_records_a_gap_when_the_agent_fails():
    state = {"event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"}, "evidence": {}, "run_id": "run-1", "plan": {"gaps": []}}
    settings = Settings(a2a_adapter_url="http://a2a-adapter:8000")

    def fail(url, payload, message_id):
        assert payload["service_id"] == "api"
        assert "timeline" not in payload
        raise RuntimeError("down")

    first = run_tool("consult_runbook_agent", {"question": "restart", "hypothesis": "disk"}, state, settings, {"a2a": fail})
    second = run_tool("consult_runbook_agent", {"question": "again"}, state, settings, {"a2a": fail})
    assert first["status"] == "error"
    assert state["coverage"]["gaps"] == ["runbook agent did not answer"]
    assert second["note"] == "runbook already consulted"


def _port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


def _listen(app, port):
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning"))
    server.install_signal_handlers = lambda: None
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    deadline = time.time() + 5
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.2):
                return server
        except OSError:
            time.sleep(0.05)
    raise RuntimeError("a2a test server did not start")


def test_send_message_then_get_task_and_a_duplicate_skips_ragflow():
    calls = []

    def research(base_url, api_key, agent_id, query, timeout):
        calls.append(query)
        return {
            "schema": "runbook-research.v1",
            "content": "restart the process",
            "citations": [{"document_id": "doc", "id": "chunk"}],
            "citations_available": True,
        }

    import alertflow_agent.a2a_adapter.server as server
    server.research = research
    port = _port()
    url = f"http://127.0.0.1:{port}"
    http = _listen(build_app(
        MemoryLedger(),
        {"ragflow_base_url": "http://ragflow.test", "ragflow_agent_id": "agent", "timeout_seconds": 5},
        url,
    ), port)
    try:
        card = httpx.get(f"{url}/.well-known/agent-card.json", timeout=5).json()
        assert [skill["id"] for skill in card["skills"]] == ["runbook_research"]
        from alertflow_agent.integrations.runbook import consult
        payload = {"service_id": "api", "question": "restart"}
        first = consult(url, payload, "message-1", timeout=5)
        second = consult(url, payload, "message-1", timeout=5)
        assert first["schema"] == "runbook-research.v1"
        assert first["citations_available"] is True
        assert second["content"] == first["content"]
        assert len(calls) == 1
    finally:
        http.should_exit = True


def test_failed_lookup_is_a_coverage_gap():
    def research(base_url, api_key, agent_id, query, timeout):
        raise RuntimeError("down")

    import alertflow_agent.a2a_adapter.server as server
    server.research = research
    port = _port()
    url = f"http://127.0.0.1:{port}"
    http = _listen(build_app(MemoryLedger(), {"ragflow_base_url": "http://ragflow.test", "ragflow_agent_id": "agent", "timeout_seconds": 5}, url), port)
    try:
        state = {
            "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"},
            "evidence": {},
            "run_id": "run-http",
            "coverage": {"gaps": []},
        }
        result = run_tool("consult_runbook_agent", {"question": "restart"}, state, Settings(a2a_adapter_url=url), {})
        assert result["status"] == "error"
        assert result["evidence_ids"] == []
        assert state["coverage"]["gaps"] == ["runbook agent did not answer"]
        assert "runbook:runbook:run-http" not in state["evidence"]
    finally:
        http.should_exit = True


def _ok_artifact():
    return {
        "schema": "runbook-research.v1",
        "content": "restart the process",
        "citations": [{"document_id": "doc", "id": "chunk"}],
        "citations_available": True,
    }


def test_working_duplicate_stays_working_until_the_lookup_finishes():
    started = threading.Event()
    release = threading.Event()
    calls = []

    def research(base_url, api_key, agent_id, query, timeout):
        calls.append(query)
        started.set()
        assert release.wait(2)
        return _ok_artifact()

    import alertflow_agent.a2a_adapter.server as server
    import asyncio
    server.research = research
    ledger = MemoryLedger()
    executor = RunbookExecutor(ledger, {"ragflow_base_url": "http://ragflow.test", "ragflow_api_key": "k", "ragflow_agent_id": "a", "timeout_seconds": 30})

    async def go():
        await executor.execute(Context(), Queue())
        assert await asyncio.to_thread(started.wait, 2)
        await executor.execute(Context(), Queue())
        assert ledger.get_message("m1")["status"] == "working"
        assert ledger.get_message("m1")["task_json"]["status"]["state"] == "TASK_STATE_WORKING"
        assert len(calls) == 1
        release.set()
        await executor.join()

    asyncio.run(go())
    assert ledger.get_message("m1")["status"] == "completed"
    assert len(calls) == 1


def test_expired_working_task_fails_without_another_lookup():
    started = threading.Event()
    release = threading.Event()
    calls = []

    def research(base_url, api_key, agent_id, query, timeout):
        calls.append(query)
        started.set()
        assert release.wait(2)
        return _ok_artifact()

    import alertflow_agent.a2a_adapter.server as server
    import asyncio
    server.research = research
    ledger = MemoryLedger()
    executor = RunbookExecutor(ledger, {"ragflow_base_url": "http://ragflow.test", "ragflow_api_key": "k", "ragflow_agent_id": "a", "timeout_seconds": 0})

    async def go():
        await executor.execute(Context(), Queue())
        assert await asyncio.to_thread(started.wait, 2)
        await asyncio.sleep(0.02)
        stored = executor.read_task("task-1")
        assert stored["status"] == "failed"
        await executor.execute(Context(), Queue())
        assert len(calls) == 1
        release.set()
        await executor.join()

    asyncio.run(go())
    assert ledger.get_message("m1")["status"] == "failed"
    assert ledger.get_message("m1")["task_json"]["status"]["state"] == "TASK_STATE_FAILED"
    assert len(calls) == 1


def test_restart_fails_a_working_task_without_rerunning():
    started = threading.Event()
    release = threading.Event()
    calls = []

    def research(base_url, api_key, agent_id, query, timeout):
        calls.append(query)
        started.set()
        assert release.wait(2)
        return _ok_artifact()

    import alertflow_agent.a2a_adapter.server as server
    import asyncio
    server.research = research
    ledger = MemoryLedger()
    settings = {"ragflow_base_url": "http://ragflow.test", "ragflow_api_key": "k", "ragflow_agent_id": "a", "timeout_seconds": 30}
    executor = RunbookExecutor(ledger, settings)

    async def go():
        await executor.execute(Context(), Queue())
        assert await asyncio.to_thread(started.wait, 2)
        ledger.live.clear()
        restarted = RunbookExecutor(ledger, settings)
        row = restarted.read_task("task-1")
        assert row["status"] == "failed"
        assert row["task_json"]["status"]["state"] == "TASK_STATE_FAILED"
        release.set()
        await executor.join()

    asyncio.run(go())
    assert ledger.get_message("m1")["status"] == "failed"
    assert len(calls) == 1


def test_postgres_restart_fails_the_saved_working_task():
    import os
    from pathlib import Path

    url = os.environ.get("ALERTFLOW_TEST_DATABASE_URL", "")
    if not url:
        import pytest
        pytest.skip("ALERTFLOW_TEST_DATABASE_URL is not set")
    import psycopg
    from alertflow_agent.a2a_adapter.server import RunbookExecutor
    from alertflow_agent.a2a_adapter.store import PostgresLedger

    name = "alertflow_test_a2a_restart"
    admin = url
    if "/postgres" not in admin:
        raise RuntimeError("maintenance database must be named postgres")
    with psycopg.connect(admin, autocommit=True) as conn:
        conn.execute(f'DROP DATABASE IF EXISTS "{name}"')
        conn.execute(f'CREATE DATABASE "{name}"')
    target = admin.rsplit("/", 1)[0] + "/" + name
    try:
        root = Path(__file__).resolve().parents[2] / "backend" / "internal" / "db" / "migrations"
        sql = []
        for filename in ("00018_a2a_adapter_tasks.sql", "00019_a2a_task_aliases.sql"):
            text = (root / filename).read_text()
            sql.append(text.split("-- +goose Down", 1)[0].replace("-- +goose Up", ""))
        with psycopg.connect(target, autocommit=True) as conn:
            conn.execute("\n".join(sql))
        first = PostgresLedger(target)
        executor = RunbookExecutor(first, {"timeout_seconds": 30})
        assert first.claim("m1", "task-1", 30)
        first.live.add("m1")
        executor._store("task-1", "working", None)
        restarted = RunbookExecutor(PostgresLedger(target), {"timeout_seconds": 30})
        row = restarted.read_task("task-1")
        assert row["status"] == "failed"
        again = PostgresLedger(target).get_task("task-1")
        assert again["status"] == "failed"
        assert again["task_json"]["status"]["state"] == "TASK_STATE_FAILED"
    finally:
        with psycopg.connect(admin, autocommit=True) as conn:
            conn.execute(f'DROP DATABASE IF EXISTS "{name}"')
