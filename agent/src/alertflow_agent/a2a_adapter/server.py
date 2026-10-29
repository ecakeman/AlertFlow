"""A2A 1.0 HTTP+JSON server. Protocol routes come from a2a-sdk 1.3.0."""

from __future__ import annotations

import os

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
        default_input_modes=["text/plain"],
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


class LedgerTaskStore(TaskStore):
    def __init__(self, ledger):
        self.ledger = ledger

    async def save(self, task: Task, context) -> None:
        body = MessageToDict(task, preserving_proto_field_name=True)
        artifact = None
        if task.artifacts:
            artifact = MessageToDict(task.artifacts[-1], preserving_proto_field_name=True)
        state = TaskState.Name(task.status.state)
        self.ledger.save_task(task.id, state, artifact, body, state == "TASK_STATE_CANCELED")

    async def get(self, task_id: str, context) -> Task | None:
        row = self.ledger.get_task(task_id)
        if not row or not row.get("task_json"):
            return None
        return ParseDict(row["task_json"], Task())

    async def list(self, params: ListTasksRequest, context) -> ListTasksResponse:
        return ListTasksResponse(tasks=[], next_page_token="", page_size=0, total_size=0)

    async def delete(self, task_id: str, context) -> None:
        self.ledger.cancel(task_id)


class RunbookExecutor(AgentExecutor):
    def __init__(self, ledger, settings: dict):
        self.ledger = ledger
        self.settings = settings

    async def execute(self, context, event_queue) -> None:
        message = context.message
        message_id = message.message_id
        task_id = context.task_id or message.task_id
        timeout = int(self.settings.get("timeout_seconds") or 30)
        if not self.ledger.claim(message_id, task_id, timeout):
            prior = self.ledger.get_message(message_id) or {}
            stored = prior.get("task_json")
            task = ParseDict(stored, Task()) if stored else Task(id=prior.get("task_id") or task_id)
            await event_queue.enqueue_event(task)
            return
        query = "\n".join(part.text for part in message.parts if part.text)[:8000]
        try:
            if self.ledger.get_message(message_id).get("cancelled"):
                await self._finish(event_queue, task_id, context.context_id, "canceled", {"schema": "runbook-research.v1", "content": "", "citations": [], "citations_available": False})
                return
            artifact = research(
                self.settings.get("ragflow_base_url") or "",
                self.settings.get("ragflow_api_key") or "",
                self.settings.get("ragflow_agent_id") or "",
                query,
                float(timeout),
            )
            await self._finish(event_queue, task_id, context.context_id, "completed", artifact)
        except Exception:
            await self._finish(
                event_queue, task_id, context.context_id, "failed",
                {"schema": "runbook-research.v1", "content": "", "citations": [], "citations_available": False, "error": "runbook lookup failed"},
            )

    async def cancel(self, context, event_queue) -> None:
        task_id = context.task_id
        self.ledger.cancel(task_id)
        await event_queue.enqueue_event(TaskStatusUpdateEvent(
            task_id=task_id,
            context_id=context.context_id or "",
            status=TaskStatus(state=TaskState.TASK_STATE_CANCELED),
        ))

    async def _finish(self, event_queue, task_id: str, context_id: str, status: str, artifact: dict) -> None:
        state = {
            "completed": TaskState.TASK_STATE_COMPLETED,
            "failed": TaskState.TASK_STATE_FAILED,
            "canceled": TaskState.TASK_STATE_CANCELED,
        }[status]
        proto_artifact = Artifact(
            artifact_id="runbook-research.v1",
            name="runbook-research.v1",
            parts=[Part(text=artifact_text(artifact), media_type="application/json")],
        )
        task = Task(
            id=task_id,
            context_id=context_id or "",
            status=TaskStatus(state=state),
            artifacts=[proto_artifact],
        )
        body = MessageToDict(task, preserving_proto_field_name=True)
        self.ledger.save_task(task_id, status, artifact, body, status == "canceled")
        await event_queue.enqueue_event(TaskArtifactUpdateEvent(
            task_id=task_id, context_id=context_id or "", artifact=proto_artifact, last_chunk=True,
        ))
        await event_queue.enqueue_event(task)


def build_app(ledger=None, settings: dict | None = None, public_url: str = "http://127.0.0.1:8001") -> Starlette:
    ledger = ledger or MemoryLedger()
    settings = settings or {}
    card = build_card(public_url)
    executor = RunbookExecutor(ledger, settings)
    handler = DefaultRequestHandlerV2(executor, LedgerTaskStore(ledger), card)
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
