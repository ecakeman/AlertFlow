import asyncio
import os
import uuid

import grpc
import httpx
import pytest

os.environ.setdefault("ALERTFLOW_DATABASE_URL", "postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")
os.environ.setdefault("ALERTFLOW_LLM_API_KEY", "")

from alertflow_agent.artifacts import write_artifact
from alertflow_agent.fd import connect
from alertflow_agent.gen.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc
from alertflow_agent.http_app import create_app
from alertflow_agent.receipts import accept_task
from alertflow_agent.runtime import AgentRuntime, SystemClient


@pytest.fixture
async def clean():
    conn = await connect()
    async with conn.cursor() as cur:
        await cur.execute("TRUNCATE cognitive.fault_documents, cognitive.episode_artifacts, cognitive.agent_task_receipts")
    await conn.commit()
    await conn.close()
    yield


def _task(episode=None, task_id=None):
    return {
        "task_id": task_id or str(uuid.uuid4()),
        "episode_id": episode or str(uuid.uuid4()),
        "task_type": "DIAGNOSIS",
        "execution_generation": 1,
        "input_version_from": 1,
        "input_version_to": 1,
        "thread_id": f"diagnosis:{episode or 'ep'}",
    }


async def test_worker_pool_limits_concurrency(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=2, queue_capacity=8)
    current = 0
    peak = 0

    async def execute(task):
        nonlocal current, peak
        current += 1
        peak = max(peak, current)
        await asyncio.sleep(0.2)
        current -= 1

    runtime.execute = execute
    await runtime.start(serve_grpc=False, start_recovery=False)
    try:
        for _ in range(4):
            await runtime.submit(_task())
        await asyncio.sleep(0.7)
        assert peak == 2
    finally:
        await runtime.shutdown(timeout=1)


async def test_full_queue_stays_recoverable(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=1, queue_capacity=1)
    started = asyncio.Event()

    async def execute(task):
        started.set()
        await asyncio.Event().wait()

    runtime.execute = execute
    await runtime.start(serve_grpc=False, start_recovery=False)
    try:
        first_task = _task()
        second_task = _task()
        third_task = _task()
        first = await runtime.submit(first_task)
        await started.wait()
        second = await runtime.submit(second_task)
        third = await runtime.submit(third_task)
        assert first["accepted"] and second["accepted"] and third["accepted"]
        assert third["message"] == "queued for recovery"
        conn = await connect()
        async with conn.cursor() as cur:
            await cur.execute("SELECT status FROM cognitive.agent_task_receipts WHERE task_id=%s", (third_task["task_id"],))
            status = (await cur.fetchone())[0]
        await conn.close()
        assert status == "ACCEPTED"
    finally:
        await runtime.shutdown(timeout=0.2)


async def test_recovery_requeues_accepted_receipt(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=1, queue_capacity=4)
    seen = []

    async def execute(task):
        seen.append(task["task_id"])

    episode = str(uuid.uuid4())
    task = _task(episode)
    task["thread_id"] = f"diagnosis:{episode}"
    conn = await connect()
    await accept_task(conn, task)
    await conn.close()
    runtime.execute = execute
    await runtime.start(serve_grpc=False, start_recovery=False)
    try:
        await runtime.recover_once()
        await asyncio.sleep(0.1)
        assert [str(item) for item in seen] == [task["task_id"]]
        seen.clear()
        await runtime.recover_once()
        await asyncio.sleep(0.1)
        assert seen == []
    finally:
        await runtime.shutdown(timeout=1)


async def test_same_episode_is_serial_and_different_episodes_overlap(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=2, queue_capacity=8)
    order = []

    async def execute(task):
        conn = await connect()
        key = await runtime_lock(conn, task["thread_id"])
        try:
            order.append(("start", task["episode_id"]))
            await asyncio.sleep(0.25)
            order.append(("end", task["episode_id"]))
        finally:
            from alertflow_agent.receipts import advisory_unlock
            await advisory_unlock(conn, key)
            await conn.close()

    async def runtime_lock(conn, thread_id):
        from alertflow_agent.receipts import advisory_lock
        return await advisory_lock(conn, thread_id)

    runtime.execute = execute
    await runtime.start(serve_grpc=False, start_recovery=False)
    try:
        shared = str(uuid.uuid4())
        other = str(uuid.uuid4())
        await runtime.submit(_task(other))
        await runtime.submit(_task(shared))
        await runtime.submit(_task(shared))
        await asyncio.sleep(1.2)
        shared_events = [item[0] for item in order if item[1] == shared]
        assert shared_events == ["start", "end", "start", "end"]
        other_start = next(i for i, item in enumerate(order) if item == ("start", other))
        other_end = next(i for i, item in enumerate(order) if item == ("end", other))
        shared_start = next(i for i, item in enumerate(order) if item == ("start", shared))
        first_shared_end = next(i for i, item in enumerate(order) if item == ("end", shared))
        assert other_start < first_shared_end and shared_start < other_end
    finally:
        await runtime.shutdown(timeout=1)


async def test_cancel_releases_advisory_lock(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=1, queue_capacity=2)
    held = asyncio.Event()

    async def execute(task):
        conn = await connect()
        from alertflow_agent.receipts import advisory_lock, advisory_unlock
        key = await advisory_lock(conn, task["thread_id"])
        held.set()
        try:
            await asyncio.Event().wait()
        finally:
            await advisory_unlock(conn, key)
            await conn.close()

    runtime.execute = execute
    episode = str(uuid.uuid4())
    await runtime.start(serve_grpc=False, start_recovery=False)
    try:
        await runtime.submit(_task(episode))
        await held.wait()
        runtime.workers[0].cancel()
        await asyncio.sleep(0.2)
        conn = await connect()
        from alertflow_agent.receipts import advisory_lock
        await asyncio.wait_for(advisory_lock(conn, f"diagnosis:{episode}"), timeout=1)
        await conn.close()
    finally:
        await runtime.shutdown(timeout=0.2)


async def test_health_and_grpc_roundtrip(clean):
    runtime = AgentRuntime(os.environ["ALERTFLOW_DATABASE_URL"], "127.0.0.1:1", max_concurrent=1, queue_capacity=4)

    async def execute(task):
        return None

    runtime.execute = execute
    await runtime.start(grpc_addr="127.0.0.1:0", start_recovery=False)
    app = create_app(runtime)
    try:
        async with app.router.lifespan_context(app):
            transport = httpx.ASGITransport(app=app)
            async with httpx.AsyncClient(transport=transport, base_url="http://agent") as client:
                live = await client.get("/health/live")
                ready = await client.get("/health/ready")
        assert live.status_code == 200 and live.json()["status"] == "live"
        assert ready.status_code == 200 and ready.json()["status"] == "ready"
        runtime.closing = True
        async with app.router.lifespan_context(app):
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://agent") as client:
                not_ready = await client.get("/health/ready")
        assert not_ready.status_code == 503
        runtime.closing = False

        episode = str(uuid.uuid4())
        await write_artifact(await _conn(), episode, "INVESTIGATION", "MARKDOWN", "body", None, str(uuid.uuid4()), 1, None)
        channel = grpc.aio.insecure_channel(f"127.0.0.1:{runtime.bound_port}")
        stub = agent_pb2_grpc.AgentExecutionServiceStub(channel)
        submitted = await stub.SubmitTask(agent_pb2.SubmitTaskRequest(
            task_id=str(uuid.uuid4()), episode_id=episode, task_type=agent_pb2.AGENT_TASK_TYPE_DIAGNOSIS,
            execution_generation=1, input_version_from=1, input_version_to=1,
        ))
        assert submitted.accepted
        artifact = await stub.GetArtifact(agent_pb2.GetArtifactRequest(
            episode_id=episode, artifact_type=agent_pb2.ARTIFACT_TYPE_INVESTIGATION,
        ))
        assert artifact.content_text == "body"
        gateway = grpc.aio.server()
        agent_pb2_grpc.add_SystemGatewayServiceServicer_to_server(_Gateway(), gateway)
        port = gateway.add_insecure_port("127.0.0.1:0")
        await gateway.start()
        try:
            client = SystemClient(agent_pb2_grpc.SystemGatewayServiceStub(grpc.aio.insecure_channel(f"127.0.0.1:{port}")), None)
            snap = await client.get_input({"task_id": "t", "episode_id": episode, "execution_generation": 1, "input_version_to": 1})
            assert snap.service_name == "orders"
        finally:
            await gateway.stop(1)
        await channel.close()
    finally:
        await runtime.shutdown(timeout=1)


async def _conn():
    return await connect()


class _Gateway(agent_pb2_grpc.SystemGatewayServiceServicer):
    async def GetEpisodeInput(self, request, context):
        return agent_pb2.GetEpisodeInputResponse(episode_id=request.episode_id, service_id="svc", service_name="orders")
