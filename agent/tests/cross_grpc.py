"""Process on the other side of a Go ServeControl test.

The Go test starts this file. It serves AgentTaskService and calls back into
Go. Commands arrive as one JSON object per stdin line.
"""

from __future__ import annotations

import json
import sys
import time

import grpc
from google.protobuf.json_format import ParseDict
from google.protobuf.struct_pb2 import Struct

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.generated.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc
from alertflow_agent.grpc_tasks import serve_tasks
from alertflow_agent.integrations.alertflow import AlertFlowClient


def main() -> None:
    listen, task_token, go_target, go_token = sys.argv[1:5]
    settings = Settings(agent_grpc_addr=listen, agent_task_grpc_token=task_token)
    store = MemoryStore()
    server = serve_tasks(settings, store)
    client = AlertFlowClient(go_target, go_token, timeout=5)
    tasks = agent_pb2_grpc.AgentTaskServiceStub(grpc.insecure_channel(listen))
    print(json.dumps({"ready": True}), flush=True)
    for line in sys.stdin:
        if not line.strip():
            continue
        cmd = json.loads(line)
        if cmd.get("op") == "shutdown":
            break
        try:
            result = handle(cmd, store, client, tasks, task_token, go_target)
        except grpc.RpcError as exc:
            result = {"error": exc.code().name}
        print(json.dumps(result), flush=True)
    server.stop(0)


def handle(cmd, store, client, tasks, task_token, go_target) -> dict:
    op = cmd["op"]
    if op == "accept":
        payload = Struct()
        ParseDict({"event": {
            "event": "incident_start",
            "event_id": cmd["message_id"],
            "service_id": "svc",
            "service_name": "API",
            "occurred_at": "2026-10-28T12:00:00Z",
        }}, payload)
        resp = tasks.StartInvestigation(
            agent_pb2.InvestigationRequest(
                message_id=cmd["message_id"],
                message_type="diagnostic.requested",
                correlation_id=cmd["message_id"],
                payload=payload,
            ),
            metadata=(("authorization", "Bearer " + task_token),),
            timeout=5,
        )
        return {
            "accepted": resp.accepted,
            "duplicate": resp.duplicate,
            "runs": len(store.runs),
        }
    if op == "query":
        now = int(time.time())
        page = client.timeline("svc", now - 3600, now + 60, ["incident", "kubernetes"], 20)
        metrics = client.metrics("svc", "24h")
        checks = client.checks("svc", None, 10)
        event = (page.get("events") or [{}])[0]
        return {
            "events": len(page.get("events") or []),
            "resourceUid": event.get("resourceUid"),
            "details": event.get("details") or {},
            "name": metrics.get("name"),
            "checks": len(checks.get("checks") or []),
        }
    if op == "callback":
        first = client.submit_callback(cmd["body"])
        second = client.submit_callback(cmd["body"])
        return {"first": first, "second": second}
    if op == "bad_token":
        try:
            AlertFlowClient(go_target, "wrong-token", timeout=5).submit_callback(cmd["body"])
        except grpc.RpcError as exc:
            return {"error": exc.code().name}
        return {"error": ""}
    if op == "deadline":
        import socket
        import threading

        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        sock.listen(1)

        def hold() -> None:
            conn, _ = sock.accept()
            time.sleep(2)
            conn.close()

        threading.Thread(target=hold, daemon=True).start()
        host, port = sock.getsockname()
        try:
            AlertFlowClient(f"{host}:{port}", "go-token", timeout=0.2).timeline("svc", 1, 2, ["incident"], 1)
        except grpc.RpcError as exc:
            return {"error": exc.code().name}
        return {"error": ""}
    if op == "db_down":
        try:
            client.submit_callback(cmd["body"])
        except grpc.RpcError as exc:
            return {"error": exc.code().name}
        return {"stored": True}
    raise ValueError(op)


if __name__ == "__main__":
    main()
