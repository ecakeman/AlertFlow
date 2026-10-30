"""A2A 1.0 HTTP+JSON server. Protocol routes come from a2a-sdk 1.3.0."""

from __future__ import annotations

import asyncio
import os
from datetime import datetime, timezone

import uvicorn
from google.protobuf.json_format import MessageToDict, ParseDict
from starlette.applications import Starlette

from a2a.server.agent_execution.agent_executor import AgentExecutor
from a2a.server.request_handlers.default_request_handler_v2 import DefaultRequestHandlerV2
from a2a.server.routes import create_agent_card_routes, create_rest_routes
from a2a.server.tasks.task_store import TaskStore
from a2a.types.a2a_pb2 import (
    AgentCapabilities,
    AgentCard,
    Artifact,
    ListTasksRequest,
    ListTasksResponse,
    Part,
    Task,
    TaskArtifactUpdateEvent,
    TaskState,
    TaskStatus,
    TaskStatusUpdateEvent,
)
from a2a.utils.constants import TransportProtocol

from alertflow_agent.a2a_adapter.ragflow import artifact_text, research
from alertflow_agent.a2a_adapter.store import MemoryLedger, PostgresLedger


def build_card(public_url: str) -> AgentCard:
    card = AgentCard(
        name="AlertFlow Runbook Specialist",
        description="Looks up AlertFlow operations runbooks.",
        version="0.1.0",
        default_input_modes=["application/json"],
        default_output_modes=["application/json"],
    )
    card.supported_interfaces.add(
        url=public_url.rstrip("/"),
        protocol_binding=TransportProtocol.HTTP_JSON,
        protocol_version="1.0",
    )
    card.capabilities.CopyFrom(AgentCapabilities(streaming=False))
    card.skills.add(
        id="runbook_research",
        name="Runbook research",
        description="Answer one operations question from the AlertFlow runbook.",
        tags=["runbook"],
    )
    return card


_TERMINAL = {"completed", "failed", "canceled"}
_EMPTY_ARTIFACT = {
    "schema": "runbook-research.v1",
    "content": "",
    "citations": [],
    "citations_available": False,
}


class LedgerTaskStore(TaskStore):
    def __init__(self, ledger, read_task):
        self.ledger = ledger
        self.read_task = read_task

    async def save(self, task: Task, context) -> None:
        body = MessageToDict(task, preserving_proto_field_name=True)
        artifact = None
        if task.artifacts:
            artifact = MessageToDict(task.artifacts[-1], preserving_proto_field_name=True)
        state = TaskState.Name(task.status.state)
        self.ledger.save_task(task.id, state, artifact, body, state == "TASK_STATE_CANCELED")

    async def get(self, task_id: str, context) -> Task | None:
        row = self.read_task(task_id)
        if not row or not row.get("task_json"):
            return None
        task = ParseDict(row["task_json"], Task())
        if task.id != task_id:
            task.id = task_id
        return task

    async def list(self, params: ListTasksRequest, context) -> ListTasksResponse:
        return ListTasksResponse(tasks=[], next_page_token="", page_size=0, total_size=0)

    async def delete(self, task_id: str, context) -> None:
        cancel = getattr(self, "persist_cancel", None)
        if cancel:
            cancel(task_id, "")


class RunbookExecutor(AgentExecutor):
    def __init__(self, ledger, settings: dict):
        self.ledger = ledger
        self.settings = settings
        self._jobs: list[asyncio.Task] = []

    async def join(self) -> None:
        pending = [job for job in self._jobs if not job.done()]
        if pending:
            await asyncio.gather(*pending)

    def read_task(self, task_id: str) -> dict | None:
        row = self.ledger.get_task(task_id)
        if not row:
            return None
        if row.get("status") in _TERMINAL:
            return row
        if _expired(row.get("deadline_at")) or row["message_id"] not in self.ledger.live:
            self._store(row["task_id"], "failed", {**_EMPTY_ARTIFACT, "error": "runbook lookup failed"}, "")
            return self.ledger.get_task(task_id)
        return row

    async def execute(self, context, event_queue) -> None:
        message = context.message
        message_id = message.message_id
        task_id = context.task_id or message.task_id
        raw_timeout = self.settings.get("timeout_seconds")
        timeout = 30 if raw_timeout in (None, "") else int(raw_timeout)
        if not self.ledger.claim(message_id, task_id, timeout):
            self.ledger.alias(message_id, task_id)
            row = self.read_task(task_id) or {}
            await self._emit(
                event_queue, task_id, context.context_id,
                row.get("status") or "failed",
                row.get("artifact") or _EMPTY_ARTIFACT,
                persist=False,
                artifact_event=False,
            )
            return
        self.ledger.live.add(message_id)
        await self._emit(event_queue, task_id, context.context_id, "working", None, persist=True)
        query = "\n".join(part.text for part in message.parts if part.text)[:8000]
        self._jobs.append(asyncio.create_task(self._run(message_id, task_id, context.context_id, query, float(timeout))))

    async def _run(self, message_id: str, task_id: str, context_id: str, query: str, timeout: float) -> None:
        try:
            current = self.ledger.get_message(message_id) or {}
            if current.get("cancelled") or current.get("status") in _TERMINAL:
                return
            artifact = await asyncio.to_thread(
                research,
                self.settings.get("ragflow_base_url") or "",
                self.settings.get("ragflow_api_key") or "",
                self.settings.get("ragflow_agent_id") or "",
                query,
                timeout,
            )
            current = self.ledger.get_message(message_id) or {}
            if current.get("status") in _TERMINAL:
                return
            self._store(task_id, "completed", artifact, context_id)
        except Exception:
            current = self.ledger.get_message(message_id) or {}
            if current.get("status") not in _TERMINAL:
                self._store(task_id, "failed", {**_EMPTY_ARTIFACT, "error": "runbook lookup failed"}, context_id)
        finally:
            self.ledger.live.discard(message_id)

    async def cancel(self, context, event_queue) -> None:
        task_id = context.task_id
        self.persist_cancel(task_id, context.context_id or "")
        await event_queue.enqueue_event(TaskStatusUpdateEvent(
            task_id=task_id,
            context_id=context.context_id or "",
            status=TaskStatus(state=TaskState.TASK_STATE_CANCELED),
        ))

    def persist_cancel(self, task_id: str, context_id: str = "") -> None:
        row = self.ledger.get_task(task_id)
        if not row or row.get("status") in _TERMINAL:
            return
        self._store(row["task_id"], "canceled", row.get("artifact") or dict(_EMPTY_ARTIFACT), context_id)

    def _store(self, task_id: str, status: str, artifact: dict | None, context_id: str = "") -> None:
        row = self.ledger.get_task(task_id) or {}
        if row.get("status") in _TERMINAL and status != row.get("status"):
            return
        canonical = row.get("task_id") or task_id
        kept = _context_id(row, context_id)
        task = _task(canonical, kept, status, artifact)
        body = MessageToDict(task, preserving_proto_field_name=True)
        self.ledger.save_task(canonical, status, artifact, body, status == "canceled")

    async def _emit(self, event_queue, task_id: str, context_id: str, status: str, artifact: dict | None, persist: bool, artifact_event: bool = True) -> None:
        task = _task(task_id, context_id, status, artifact)
        if persist:
            body = MessageToDict(task, preserving_proto_field_name=True)
            self.ledger.save_task(task_id, status, artifact, body, status == "canceled")
        if artifact_event and task.artifacts:
            await event_queue.enqueue_event(TaskArtifactUpdateEvent(
                task_id=task_id, context_id=context_id or "", artifact=task.artifacts[-1], last_chunk=True,
            ))
        await event_queue.enqueue_event(task)


def _context_id(row: dict, fallback: str = "") -> str:
    body = (row or {}).get("task_json") or {}
    return str(body.get("context_id") or fallback or "")


def _expired(deadline) -> bool:
    if not deadline:
        return False
    if isinstance(deadline, datetime):
        exp = deadline if deadline.tzinfo else deadline.replace(tzinfo=timezone.utc)
    else:
        exp = datetime.fromisoformat(str(deadline))
        if exp.tzinfo is None:
            exp = exp.replace(tzinfo=timezone.utc)
    return datetime.now(timezone.utc) >= exp


def _task(task_id: str, context_id: str, status: str, artifact: dict | None) -> Task:
    state = {
        "working": TaskState.TASK_STATE_WORKING,
        "completed": TaskState.TASK_STATE_COMPLETED,
        "failed": TaskState.TASK_STATE_FAILED,
        "canceled": TaskState.TASK_STATE_CANCELED,
    }[status]
    artifacts = []
    if artifact is not None:
        artifacts.append(Artifact(
            artifact_id="runbook-research.v1",
            name="runbook-research.v1",
            parts=[Part(text=artifact_text(artifact), media_type="application/json")],
        ))
    return Task(
        id=task_id,
        context_id=context_id or "",
        status=TaskStatus(state=state),
        artifacts=artifacts,
    )


def build_app(ledger=None, settings: dict | None = None, public_url: str = "http://127.0.0.1:8001") -> Starlette:
    ledger = ledger or MemoryLedger()
    settings = settings or {}
    card = build_card(public_url)
    executor = RunbookExecutor(ledger, settings)
    store = LedgerTaskStore(ledger, executor.read_task)
    store.persist_cancel = executor.persist_cancel
    handler = DefaultRequestHandlerV2(executor, store, card)
    return Starlette(routes=[*create_agent_card_routes(card), *create_rest_routes(handler)])


def main() -> None:
    dsn = os.environ.get("A2A_DATABASE_URL", "")
    ledger = PostgresLedger(dsn) if dsn else MemoryLedger()
    public_url = os.environ.get("A2A_PUBLIC_URL", "http://127.0.0.1:8001")
    settings = {
        "ragflow_base_url": os.environ.get("RAGFLOW_BASE_URL", ""),
        "ragflow_api_key": os.environ.get("RAGFLOW_API_KEY", ""),
        "ragflow_agent_id": os.environ.get("RAGFLOW_AGENT_ID", ""),
        "timeout_seconds": os.environ.get("A2A_TASK_TIMEOUT_SECONDS", "30"),
    }
    app = build_app(ledger, settings, public_url)
    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("A2A_PORT", "8000")))
