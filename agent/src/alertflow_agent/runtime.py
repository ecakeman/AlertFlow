import asyncio
import os
import signal
from datetime import datetime, timezone

import grpc
import httpx
from google.protobuf.json_format import MessageToDict
from pgvector.psycopg import register_vector_async
from psycopg.rows import dict_row
from psycopg_pool import AsyncConnectionPool

from alertflow_agent.artifacts import read_artifact
from alertflow_agent.fd import connect, fault_content_hash, save_fault_document, search_faults
from alertflow_agent.gen.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc
from alertflow_agent.http_app import create_app
from alertflow_agent.invocation import AgentInvocationClient
from alertflow_agent.mcp_client import McpClient
from alertflow_agent.receipts import accept_task, advisory_lock, advisory_unlock, mark_finished, mark_running, unfinished
from alertflow_agent.workflow import ArchiveModel, model_from_env, run_archive, run_diagnosis

try:
    from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
except Exception:  # pragma: no cover
    AsyncPostgresSaver = None

RPC_TIMEOUT = 30


def _int_env(name: str, default: int) -> int:
    raw = os.environ.get(name, "")
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        return default
    return value if value > 0 else default


class SystemClient:
    def __init__(self, stub, db):
        self.stub = stub
        self.db = db
        self.fault_hash = ""
        self.invocation = None
        self.mcp = None

    async def invoke_knowledge(self, state):
        if self.invocation is None:
            raise RuntimeError("knowledge agent is not configured")
        return await self.invocation.invoke(state)

    async def call_mcp(self, state, action):
        if self.mcp is None:
            raise RuntimeError("mcp gateway is not configured")
        text = await self.mcp.call_tool(action.get("name") or "", {"episode_id": state.get("episode_id")})
        return {"name": action.get("name"), "text": text}

    async def get_input(self, task: dict):
        return await self.stub.GetEpisodeInput(agent_pb2.GetEpisodeInputRequest(
            task_id=task["task_id"], episode_id=task["episode_id"],
            target_input_version=int(task.get("input_version_to") or 0),
            execution_generation=int(task["execution_generation"]),
        ), timeout=RPC_TIMEOUT)

    async def query_timeline(self, state, parsed):
        response = await self.stub.QueryTimeline(agent_pb2.QueryTimelineRequest(
            task_id=state["task_id"], episode_id=state["episode_id"],
            target_input_version=int(state["input_version_to"]),
            execution_generation=int(state["execution_generation"]),
            start_time=_ts(parsed.start_time), end_time=_ts(parsed.end_time),
            monitor_point_ids=parsed.monitor_point_ids, app_event_ids=parsed.app_event_ids,
        ), timeout=RPC_TIMEOUT)
        records = []
        for rec in response.records:
            records.append({
                "record_type": rec.record_type, "record_id": rec.record_id,
                "event_time": rec.event_time.ToDatetime(timezone.utc).isoformat(),
                "received_at": rec.received_at.ToDatetime(timezone.utc).isoformat(),
                "source": rec.source, "content": MessageToDict(rec.content),
            })
        return records

    async def search_faults(self, state, query: str):
        return await search_faults(self.db, state["service_id"], query)

    async def register_artifact(self, state, artifact_type: str, version: int, content_type: str, content_hash: str):
        kind = agent_pb2.ARTIFACT_TYPE_DIAGNOSIS_REPORT if artifact_type == "DIAGNOSIS_REPORT" else agent_pb2.ARTIFACT_TYPE_INVESTIGATION
        response = await self.stub.RegisterArtifact(agent_pb2.RegisterArtifactRequest(
            task_id=state["task_id"], episode_id=state["episode_id"], artifact_type=kind,
            execution_generation=int(state["execution_generation"]), artifact_version=int(version),
            storage_key=f"{state['episode_id']}/{artifact_type}", content_type=content_type, content_hash=content_hash,
        ), timeout=RPC_TIMEOUT)
        if not response.registered:
            raise RuntimeError("artifact registration rejected")

    async def save_fault(self, task, investigation, report, index):
        self.fault_hash = fault_content_hash(investigation, report)
        return await save_fault_document(self.db, task["episode_id"], task["service_id"], investigation, report, index)

    async def register_fault_document(self, task, fd_id: str, content_hash: str):
        response = await self.stub.RegisterFaultDocument(agent_pb2.RegisterFaultDocumentRequest(
            task_id=task["task_id"], episode_id=task["episode_id"],
            execution_generation=int(task["execution_generation"]), fd_id=fd_id, content_hash=content_hash,
        ), timeout=RPC_TIMEOUT)
        if not response.registered:
            raise RuntimeError(response.message or "fault document registration rejected")

    async def started(self, task):
        await self.stub.ReportTaskStarted(agent_pb2.ReportTaskStartedRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], execution_generation=int(task["execution_generation"]),
        ), timeout=RPC_TIMEOUT)

    async def heartbeat(self, task):
        await self.stub.HeartbeatTask(agent_pb2.HeartbeatTaskRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], execution_generation=int(task["execution_generation"]),
        ), timeout=RPC_TIMEOUT)

    async def complete(self, task, outcome: str, processed: int = 0, fd_id: str = "", error: str = ""):
        kind = agent_pb2.AGENT_TASK_TYPE_ARCHIVE if task["task_type"] == "ARCHIVE" else agent_pb2.AGENT_TASK_TYPE_DIAGNOSIS
        ack = await self.stub.CompleteTask(agent_pb2.CompleteTaskRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], task_type=kind,
            execution_generation=int(task["execution_generation"]), outcome=outcome,
            processed_input_version=int(processed), fd_id=fd_id, error_code=error, error_message=error,
        ), timeout=RPC_TIMEOUT)
        if not ack.accepted:
            raise RuntimeError(ack.message or "complete rejected")


def _ts(value: datetime):
    from google.protobuf.timestamp_pb2 import Timestamp
    stamp = Timestamp()
    stamp.FromDatetime(value.astimezone(timezone.utc))
    return stamp


class AgentRuntime:
    def __init__(self, dsn: str, go_target: str, *, max_concurrent: int | None = None, queue_capacity: int | None = None,
                 http_client: httpx.AsyncClient | None = None):
        self.dsn = dsn
        self.go_target = go_target
        self.max_concurrent = max_concurrent or _int_env("AGENT_MAX_CONCURRENT_TASKS", 4)
        self.queue_capacity = queue_capacity or _int_env("AGENT_TASK_QUEUE_CAPACITY", 128)
        self.http_client = http_client
        self.owns_http_client = http_client is None
        self.model = None
        self.archive_model = None
        self.checkpointer = None
        self.pool: AsyncConnectionPool | None = None
        self.checkpoint_pool: AsyncConnectionPool | None = None
        self.queue: asyncio.Queue | None = None
        self.known: set[str] = set()
        self.lock = asyncio.Lock()
        self.workers: list[asyncio.Task] = []
        self.recovery_task: asyncio.Task | None = None
        self.channel = None
        self.grpc = None
        self.initialized = False
        self.grpc_started = False
        self.workers_started = False
        self.closing = False

    async def start(self, *, grpc_addr: str | None = None, serve_grpc: bool = True, start_recovery: bool = True):
        if self.http_client is None:
            self.http_client = httpx.AsyncClient(timeout=60)
        self.model = model_from_env(self.http_client)
        self.archive_model = ArchiveModel(self.http_client)
        self.pool = AsyncConnectionPool(
            conninfo=self.dsn, min_size=1, max_size=max(4, self.max_concurrent), open=False,
            kwargs={"prepare_threshold": 0}, configure=_prepare_business,
        )
        await self.pool.open()
        await self._open_checkpointer()
        self.queue = asyncio.Queue(maxsize=self.queue_capacity)
        self.channel = grpc.aio.insecure_channel(self.go_target)
        self.knowledge_enabled = os.environ.get("ALERTFLOW_KNOWLEDGE_ENABLED", "") == "true"
        self.invocation_client = AgentInvocationClient(self.channel) if self.knowledge_enabled else None
        mcp_endpoint = os.environ.get("ALERTFLOW_MCP_ENDPOINT", "")
        mcp_token = os.environ.get("ALERTFLOW_MCP_TOKEN", "")
        self.mcp_client = McpClient(mcp_endpoint, mcp_token, self.http_client) if mcp_endpoint and mcp_token else None
        for _ in range(self.max_concurrent):
            self.workers.append(asyncio.create_task(self._worker()))
        self.workers_started = True
        if serve_grpc:
            self.grpc = grpc.aio.server()
            agent_pb2_grpc.add_AgentExecutionServiceServicer_to_server(ExecutionService(self), self.grpc)
            self.bound_port = self.grpc.add_insecure_port(grpc_addr or os.environ.get("AGENT_GRPC_ADDR", "0.0.0.0:9000"))
            await self.grpc.start()
            self.grpc_started = True
        else:
            self.grpc_started = True
        if start_recovery:
            self.recovery_task = asyncio.create_task(self._recover_loop())
        self.initialized = True

    async def _open_checkpointer(self):
        if AsyncPostgresSaver is None:
            return
        self.checkpoint_pool = AsyncConnectionPool(
            conninfo=self.dsn, min_size=1, max_size=max(4, self.max_concurrent), open=False,
            kwargs={"autocommit": True, "prepare_threshold": 0, "row_factory": dict_row},
            configure=_prepare_checkpoint,
        )
        await self.checkpoint_pool.open()
        self.checkpointer = AsyncPostgresSaver(self.checkpoint_pool)
        await self.checkpointer.setup()

    async def readiness(self, http_ready: bool) -> tuple[bool, str]:
        if self.closing:
            return False, "shutting down"
        if not self.initialized:
            return False, "runtime not initialized"
        if not http_ready:
            return False, "http not ready"
        if not self.grpc_started:
            return False, "grpc not started"
        if not self.workers_started:
            return False, "workers not started"
        try:
            async with self.pool.connection() as conn:
                async with conn.cursor() as cur:
                    await cur.execute("SELECT 1")
                    await cur.fetchone()
        except Exception:
            return False, "database unavailable"
        return True, ""

    async def submit(self, task: dict) -> dict:
        if self.closing:
            return {"accepted": False, "duplicate": False, "status": "", "message": "shutting down"}
        async with self.pool.connection() as conn:
            result = await accept_task(conn, task)
        if result.get("accepted") and not result.get("duplicate"):
            task["thread_id"] = result["thread_id"]
            queued = await self.enqueue(task)
            if not queued:
                result["message"] = "queued for recovery"
        return result

    async def enqueue(self, task: dict) -> bool:
        async with self.lock:
            if task["task_id"] in self.known:
                return False
            self.known.add(task["task_id"])
        try:
            self.queue.put_nowait(task)
        except asyncio.QueueFull:
            async with self.lock:
                self.known.discard(task["task_id"])
            return False
        return True

    async def _worker(self):
        while True:
            task = await self.queue.get()
            try:
                if task is None:
                    return
                await self.execute(task)
            finally:
                self.queue.task_done()

    async def execute(self, task: dict):
        conn = await connect(self.dsn)
        thread_id = task.get("thread_id") or f"diagnosis:{task['episode_id']}"
        key = None
        client = SystemClient(agent_pb2_grpc.SystemGatewayServiceStub(self.channel), conn)
        client.invocation = self.invocation_client
        client.mcp = self.mcp_client
        try:
            key = await advisory_lock(conn, thread_id)
            await mark_running(conn, task["task_id"])
            await client.started(task)
            await client.heartbeat(task)
            if task["task_type"] == "ARCHIVE":
                snap = await client.get_input(task)
                task["service_id"] = snap.service_id
                fd_id = await run_archive(self.archive_model, client, task)
                await client.register_fault_document(task, fd_id, client.fault_hash)
                await client.complete(task, "SUCCEEDED", fd_id=fd_id)
            else:
                snap = await client.get_input(task)
                task["service_name"] = snap.service_name
                task["service_id"] = snap.service_id
                task["episode_started_at"] = snap.episode_started_at.ToDatetime(timezone.utc).isoformat()
                task["knowledge_enabled"] = self.knowledge_enabled
                task["mcp_tools"] = await self.mcp_client.list_tools() if self.mcp_client is not None else []
                await run_diagnosis(self.model, client, task, self.checkpointer)
                await client.complete(task, "SUCCEEDED", processed=int(task.get("input_version_to") or 0))
            await mark_finished(conn, task["task_id"], "COMPLETED", {"outcome": "SUCCEEDED"}, "CONFIRMED")
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            try:
                await client.complete(task, "RETRYABLE_FAILURE", error=type(exc).__name__ + ": " + str(exc)[:400])
            except Exception:
                pass
            try:
                await mark_finished(conn, task["task_id"], "FAILED", {"error": str(exc)[:400]}, "SENT")
            except Exception:
                await conn.rollback()
        finally:
            if key is not None:
                try:
                    await advisory_unlock(conn, key)
                except Exception:
                    pass
            await conn.close()
            async with self.lock:
                self.known.discard(task["task_id"])

    async def recover_once(self):
        async with self.pool.connection() as conn:
            rows = await unfinished(conn)
        for row in rows:
            row["thread_id"] = row.get("thread_id") or f"diagnosis:{row['episode_id']}"
            try:
                await self.enqueue(row)
            except Exception:
                continue

    async def _recover_loop(self):
        while not self.closing:
            try:
                await asyncio.sleep(2)
                if self.closing:
                    return
                await self.recover_once()
            except asyncio.CancelledError:
                raise
            except Exception:
                continue

    async def shutdown(self, timeout: float | None = None):
        self.closing = True
        if self.recovery_task is not None:
            self.recovery_task.cancel()
            try:
                await self.recovery_task
            except asyncio.CancelledError:
                pass
        if self.queue is not None:
            for _ in self.workers:
                try:
                    self.queue.put_nowait(None)
                except asyncio.QueueFull:
                    break
        if timeout is None:
            timeout = float(os.environ.get("AGENT_SHUTDOWN_TIMEOUT", "20"))
        if self.workers:
            done, pending = await asyncio.wait(self.workers, timeout=timeout)
            for task in pending:
                task.cancel()
            if pending:
                await asyncio.wait(pending)
        if self.grpc is not None:
            await self.grpc.stop(5)
            self.grpc_started = False
        if self.channel is not None:
            await self.channel.close()
        if self.checkpoint_pool is not None:
            await self.checkpoint_pool.close()
        if self.pool is not None:
            await self.pool.close()
        if self.owns_http_client and self.http_client is not None:
            await self.http_client.aclose()


class ExecutionService(agent_pb2_grpc.AgentExecutionServiceServicer):
    def __init__(self, runtime: AgentRuntime):
        self.runtime = runtime

    async def SubmitTask(self, request, context):
        task = _task_from_request(request)
        result = await self.runtime.submit(task)
        return agent_pb2.SubmitTaskResponse(
            accepted=bool(result.get("accepted")), duplicate=bool(result.get("duplicate")),
            task_id=task["task_id"], receipt_status=result.get("status", ""), message=result.get("message", ""),
        )

    async def GetArtifact(self, request, context):
        kind = "DIAGNOSIS_REPORT" if request.artifact_type == agent_pb2.ARTIFACT_TYPE_DIAGNOSIS_REPORT else "INVESTIGATION"
        async with self.runtime.pool.connection() as conn:
            row = await read_artifact(conn, request.episode_id, kind)
        if row is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "artifact not found")
        response = agent_pb2.GetArtifactResponse(
            artifact_type=request.artifact_type, version=row["version"], content_type=row["content_type"],
            content_text=row["content_text"] or "", content_hash=row["content_hash"],
        )
        if row["content_json"]:
            from google.protobuf.struct_pb2 import Struct
            struct = Struct()
            struct.update(row["content_json"])
            response.content_json.CopyFrom(struct)
        return response


def _task_from_request(request) -> dict:
    kind = "ARCHIVE" if request.task_type == agent_pb2.AGENT_TASK_TYPE_ARCHIVE else "DIAGNOSIS"
    points, events = [], []
    for notice in request.notifications:
        points.extend(notice.changed_monitor_point_ids)
        events.extend(notice.changed_app_event_ids)
    return {
        "task_id": request.task_id,
        "episode_id": request.episode_id,
        "task_type": kind,
        "execution_generation": request.execution_generation,
        "input_version_from": request.input_version_from or None,
        "input_version_to": request.input_version_to or None,
        "final_input_version": request.final_input_version or None,
        "monitor_point_ids": points,
        "app_event_ids": events,
    }


async def _prepare_business(conn):
    await register_vector_async(conn)


async def _prepare_checkpoint(conn):
    await conn.execute("SET search_path TO cognitive, public")


def _parse_addr(raw: str) -> tuple[str, int]:
    host, _, port = raw.rpartition(":")
    return host or "0.0.0.0", int(port or "8000")


async def serve():
    dsn = os.environ["ALERTFLOW_DATABASE_URL"]
    target = os.environ.get("ALERTFLOW_GRPC_TARGET", "127.0.0.1:9090")
    runtime = AgentRuntime(dsn, target)
    await runtime.start()
    import uvicorn
    app = create_app(runtime)
    host, port = _parse_addr(os.environ.get("AGENT_HTTP_ADDR", "0.0.0.0:8000"))
    config = uvicorn.Config(app, host=host, port=port, log_level="info")
    server = uvicorn.Server(config)
    server.install_signal_handlers = lambda: None
    loop = asyncio.get_running_loop()
    stop = asyncio.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    http_task = asyncio.create_task(server.serve())
    await stop.wait()
    runtime.closing = True
    server.should_exit = True
    await runtime.shutdown()
    await http_task


def main():
    asyncio.run(serve())
