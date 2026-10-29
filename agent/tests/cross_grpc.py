"""Harness driven by the Go cross-language test. It uses the real Postgres store."""

import json
import sys
import uuid
from concurrent.futures import ThreadPoolExecutor

import grpc
from google.protobuf import struct_pb2

from alertflow_agent import config
from alertflow_agent.callbacks import CallbackSender
from alertflow_agent.chat_worker import ChatWorker
from alertflow_agent.db import PostgresStore, apply_schema
from alertflow_agent.generated.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc
from alertflow_agent.grpc_tasks import TaskServicer
from alertflow_agent.integrations.alertflow import AlertFlowClient
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import ScriptedModel
from fakes import FakeAlertFlow, event_row


class FixedModel:
    def answer(self, prompt):
        return "cross-language answer"


class DropFirstInvestigationAck(agent_pb2_grpc.AgentTaskServiceServicer):
    """The first accepted investigation is stored, then its success response is dropped."""

    def __init__(self, inner):
        self.inner = inner
        self.dropped = False

    def StartInvestigation(self, request, context):
        result = self.inner.StartInvestigation(request, context)
        if result.accepted and not self.dropped:
            self.dropped = True
            context.abort(grpc.StatusCode.UNAVAILABLE, "response dropped")
        return result

    def StartChat(self, request, context):
        return self.inner.StartChat(request, context)


class DropFirstStored:
    """Calls the real callback RPC, then treats the first stored result as lost."""

    def __init__(self, client):
        self.client = client
        self.dropped = False

    def __call__(self, payload):
        result = self.client.submit_callback(as_dict(payload))
        if result.get("stored") and not self.dropped:
            self.dropped = True
            raise ConnectionError("response dropped")
        return result


def reply(payload):
    sys.stdout.write(json.dumps(payload) + "\n")
    sys.stdout.flush()


def read_command():
    line = sys.stdin.readline()
    if not line:
        return None
    return json.loads(line)


def investigation(message_id):
    parsed = struct_pb2.Struct()
    parsed.update({
        "event": {
            "event": "incident_start",
            "event_id": message_id,
            "service_id": "svc",
            "service_name": "API",
            "occurred_at": "2026-10-28T12:00:00Z",
        },
        "diagnostic_context": {"service": {"id": "svc"}},
    })
    return agent_pb2.InvestigationRequest(
        message_id=message_id,
        message_type="diagnostic.requested",
        correlation_id=message_id,
        payload=parsed,
    )


def open_store(url):
    return PostgresStore(url)


def count_runs(url):
    store = open_store(url)
    try:
        return len(store.list_runs())
    finally:
        store.conn.close()


def run_keys(url):
    store = open_store(url)
    try:
        return [row["dedupe_key"] for row in store.list_runs()]
    finally:
        store.conn.close()


def as_dict(payload):
    if isinstance(payload, str):
        return json.loads(payload)
    return payload


def serve(listen, settings, store):
    server = grpc.server(
        ThreadPoolExecutor(max_workers=4),
        options=(("grpc.max_receive_message_length", 1024 * 1024),),
    )
    agent_pb2_grpc.add_AgentTaskServiceServicer_to_server(
        DropFirstInvestigationAck(TaskServicer(settings, store)), server,
    )
    server.add_insecure_port(listen)
    server.start()
    return server


def main():
    listen, task_token, go_target, go_token, database_url = sys.argv[1:6]
    apply_schema(database_url)
    store = PostgresStore(database_url)
    settings = config.Settings(
        agent_grpc_addr=listen,
        agent_task_grpc_token=task_token,
        alertflow_grpc_target=go_target,
        alertflow_agent_grpc_token=go_token,
        llm_api_key="test",
    )
    server = serve(listen, settings, store)
    hooks = Runner.__new__(Runner)
    hooks.settings = settings
    hooks.store = store
    hooks.model = FixedModel()
    hooks.memory = None
    reply({"ready": True})
    while True:
        cmd = read_command()
        if cmd is None or cmd.get("op") == "quit":
            break
        op = cmd["op"]
        try:
            if op == "bad_token":
                before = count_runs(database_url)
                channel = grpc.insecure_channel(listen)
                stub = agent_pb2_grpc.AgentTaskServiceStub(channel)
                try:
                    stub.StartInvestigation(
                        investigation("bad-py"),
                        metadata=(("authorization", "Bearer wrong"),),
                    )
                    reply({"stored": True, "persisted": count_runs(database_url)})
                except grpc.RpcError as exc:
                    reply({"code": exc.code().name, "persisted": count_runs(database_url), "before": before})
                finally:
                    channel.close()
            elif op == "runs":
                reply({"persisted": count_runs(database_url), "keys": run_keys(database_url)})
            elif op == "query":
                client = AlertFlowClient(go_target, go_token, timeout=5)
                timeline = client.timeline("svc", 1, 4_000_000_000, [])
                matched = next(row for row in timeline["events"] if row.get("eventKey") == "tl:1")
                reply({
                    "config": client.runtime_config(),
                    "incidents": client.incidents("svc", 10),
                    "incident": client.incident(int(cmd["incident_id"])),
                    "metrics": client.metrics("svc", "24h"),
                    "checks": client.checks("svc", None, 10),
                    "timeline": matched,
                    "sources": client.source_coverage("svc"),
                    "kubernetes": client.kubernetes_state("svc"),
                })
            elif op == "callbacks":
                run_id = str(uuid.uuid4())
                store.insert_run({
                    "id": run_id,
                    "dedupe_key": "graph-1",
                    "service_id": "svc",
                    "event_type": "incident_start",
                    "mode": "agent",
                    "status": "running",
                    "input_event": {"event": "incident_start", "service_id": "svc"},
                })
                row = store.get(run_id)
                bare = config.Settings(
                    alertflow_grpc_target="",
                    alertflow_agent_grpc_token="",
                    agent_callback_token="",
                )
                hooks.settings = bare
                before = len(store.callback_rows())
                hooks._notify_started(run_id, row)
                disabled = len(store.callback_rows()) == before
                hooks.settings = settings
                hooks._notify_started(run_id, row)
                report = hooks._report_callback(
                    row, 1, {"summary": "graph report", "status": "completed"}, [], {},
                )
                failed = hooks._failure_callback(run_id, "graph failed")
                store.enqueue_callback(report)
                store.enqueue_callback(failed)
                client = AlertFlowClient(go_target, go_token, timeout=5)
                sender = CallbackSender(settings, store)
                sender.submit = DropFirstStored(client)
                sent = sender.drain()
                waiting = sum(1 for item in store.callback_rows() if item["status"] == "retry_wait")
                store.conn.execute(
                    "UPDATE agent_callback_outbox SET next_attempt_at = now() WHERE status = 'retry_wait'",
                )
                retried = sender.drain()
                reply({"disabled": disabled, "sent": sent, "waiting": waiting, "retried": retried})
            elif op == "graph":
                row = event_row("agent")
                row["id"] = str(uuid.uuid4())
                row["dedupe_key"] = "graph-exec"
                row["service_id"] = "svc"
                row["incident_id"] = int(cmd["incident_id"])
                row["input_event"]["service_id"] = "svc"
                row["input_event"]["incident_id"] = int(cmd["incident_id"])
                row["input_event"]["script"] = ["finish"]
                store.insert_run(row)
                Runner(settings, store, FakeAlertFlow(), ScriptedModel()).execute(row["id"])
                sent = CallbackSender(settings, store).drain()
                saved = store.get(row["id"]) or {}
                reply({"status": saved.get("status"), "sent": sent, "run_id": row["id"]})
            elif op == "chat_answer":
                answered = ChatWorker(settings, store, hooks).drain()
                sent = CallbackSender(settings, store).drain()
                reply({"answered": answered, "sent": sent})
            elif op == "deadline":
                client = AlertFlowClient(cmd["target"], "token", timeout=0.2)
                try:
                    client.timeline("svc", 1, 2, [])
                    reply({"code": "OK"})
                except grpc.RpcError as exc:
                    reply({"code": exc.code().name})
            elif op == "break_store":
                store.conn.close()
                reply({"closed": True})
            else:
                reply({"error": "unknown op"})
        except Exception as exc:  # noqa: BLE001 - the Go test prints this line
            reply({"error": type(exc).__name__, "detail": str(exc)})
    server.stop(0)
    try:
        store.conn.close()
    except Exception:
        pass


if __name__ == "__main__":
    main()
