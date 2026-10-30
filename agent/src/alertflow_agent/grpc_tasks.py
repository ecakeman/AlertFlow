"""AgentTaskService in the same process as FastAPI.

StartInvestigation and StartChat only persist the task. The existing worker
runs LangGraph afterwards.
"""

import hmac
import json
from concurrent.futures import ThreadPoolExecutor

import grpc
from fastapi.responses import JSONResponse
from google.protobuf.json_format import MessageToDict

from alertflow_agent.api.app import _ingest_v2
from alertflow_agent.generated.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc


class TaskServicer(agent_pb2_grpc.AgentTaskServiceServicer):
    def __init__(self, settings, store) -> None:
        self.settings = settings
        self.store = store

    def StartInvestigation(self, request, context):
        _require(context, self.settings.agent_task_grpc_token)
        payload = MessageToDict(request.payload, preserving_proto_field_name=True) if request.payload else {}
        envelope = {
            "schema_version": "v2",
            "message_id": request.message_id,
            "message_type": request.message_type,
            "correlation_id": request.correlation_id or request.message_id,
            "payload": payload,
        }
        return _accept(_ingest_v2(envelope, self.store, self.settings), context)

    def StartChat(self, request, context):
        _require(context, self.settings.agent_task_grpc_token)
        payload = MessageToDict(request.payload, preserving_proto_field_name=True) if request.payload else {}
        payload.setdefault("request_id", request.request_id)
        payload.setdefault("message", request.message)
        if request.incident_id and "incident_id" not in payload:
            payload["incident_id"] = request.incident_id
        return _accept(_ingest_v2({
            "schema_version": "v2",
            "message_id": request.message_id,
            "message_type": "chat.requested",
            "correlation_id": request.message_id,
            "payload": payload,
        }, self.store, self.settings), context, request_id=request.request_id)


def _require(context, token: str) -> None:
    if not token:
        context.abort(grpc.StatusCode.UNAVAILABLE, "agent task token is not configured")
    given = ""
    for key, value in context.invocation_metadata():
        if key.lower() == "authorization":
            given = value
            break
    prefix = "Bearer "
    provided = given[len(prefix):] if given.startswith(prefix) else ""
    if len(provided) != len(token) or not hmac.compare_digest(provided, token):
        context.abort(grpc.StatusCode.UNAUTHENTICATED, "unauthorized")


def _accept(response, context, request_id: str = "") -> agent_pb2.TaskAccept:
    if isinstance(response, JSONResponse):
        body = json.loads(response.body or b"{}")
        if response.status_code >= 500:
            context.abort(grpc.StatusCode.INTERNAL, "could not store the task")
        if response.status_code >= 400:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(body.get("error") or "invalid request"))
        action = body.get("action") or ""
        return agent_pb2.TaskAccept(
            accepted=action != "duplicate",
            duplicate=action == "duplicate",
            run_id=str(body.get("run_id") or ""),
            request_id=request_id or str(body.get("request_id") or ""),
        )
    action = (response or {}).get("action") or ""
    return agent_pb2.TaskAccept(
        accepted=action != "duplicate",
        duplicate=action == "duplicate",
        run_id=str((response or {}).get("run_id") or ""),
        request_id=request_id or str((response or {}).get("request_id") or ""),
    )


def bind_addr(value: str) -> str:
    """grpc-python 不接受 ":9000"（缺 host 会被判 unparseable host:port）。

    兼容旧的省略 host 写法：补成 0.0.0.0，容器里才真正可绑定。
    """
    addr = (value or "").strip()
    if not addr:
        return "0.0.0.0:9000"
    if addr.startswith(":"):
        return f"0.0.0.0{addr}"
    return addr


def serve_tasks(settings, store) -> grpc.Server:
    server = grpc.server(
        ThreadPoolExecutor(max_workers=4),
        options=(("grpc.max_receive_message_length", 1024 * 1024),),
    )
    agent_pb2_grpc.add_AgentTaskServiceServicer_to_server(TaskServicer(settings, store), server)
    server.add_insecure_port(bind_addr(settings.agent_grpc_addr))
    server.start()
    return server
