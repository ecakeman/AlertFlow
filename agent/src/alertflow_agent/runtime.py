import os
import threading
from datetime import datetime, timezone

import grpc
from psycopg import Connection
from psycopg.rows import dict_row

from alertflow_agent.fd import connect, save_fault_document, search_faults
from alertflow_agent.gen.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc
from alertflow_agent.receipts import accept_task, advisory_lock, advisory_unlock, mark_finished, mark_running, unfinished
from alertflow_agent.workflow import ArchiveModel, model_from_env, run_archive, run_diagnosis

try:
    from langgraph.checkpoint.postgres import PostgresSaver
except Exception:  # pragma: no cover
    PostgresSaver = None


class SystemClient:
    def __init__(self, stub, db):
        self.stub = stub
        self.db = db

    def get_input(self, task: dict):
        return self.stub.GetEpisodeInput(agent_pb2.GetEpisodeInputRequest(
            task_id=task["task_id"], episode_id=task["episode_id"],
            target_input_version=int(task.get("input_version_to") or 0),
            execution_generation=int(task["execution_generation"]),
        ))

    def query_timeline(self, state, parsed):
        response = self.stub.QueryTimeline(agent_pb2.QueryTimelineRequest(
            task_id=state["task_id"], episode_id=state["episode_id"],
            target_input_version=int(state["input_version_to"]),
            execution_generation=int(state["execution_generation"]),
            start_time=_ts(parsed.start_time), end_time=_ts(parsed.end_time),
            monitor_point_ids=parsed.monitor_point_ids, app_event_ids=parsed.app_event_ids,
        ))
        records = []
        for rec in response.records:
            from google.protobuf.json_format import MessageToDict
            records.append({
                "record_type": rec.record_type, "record_id": rec.record_id,
                "event_time": rec.event_time.ToDatetime(timezone.utc).isoformat(),
                "received_at": rec.received_at.ToDatetime(timezone.utc).isoformat(),
                "source": rec.source, "content": MessageToDict(rec.content),
            })
        return records

    def search_faults(self, state, query: str):
        return search_faults(self.db, state["service_id"], query)

    def register_artifact(self, state, artifact_type: str, version: int, content_type: str, content_hash: str):
        kind = agent_pb2.ARTIFACT_TYPE_DIAGNOSIS_REPORT if artifact_type == "DIAGNOSIS_REPORT" else agent_pb2.ARTIFACT_TYPE_INVESTIGATION
        response = self.stub.RegisterArtifact(agent_pb2.RegisterArtifactRequest(
            task_id=state["task_id"], episode_id=state["episode_id"], artifact_type=kind,
            execution_generation=int(state["execution_generation"]), artifact_version=int(version),
            storage_key=f"{state['episode_id']}/{artifact_type}", content_type=content_type, content_hash=content_hash,
        ))
        if not response.registered:
            raise RuntimeError("artifact registration rejected")

    def save_fault(self, task, investigation, report, index):
        return save_fault_document(self.db, task["episode_id"], task["service_id"], investigation, report, index)

    def started(self, task):
        self.stub.ReportTaskStarted(agent_pb2.ReportTaskStartedRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], execution_generation=int(task["execution_generation"]),
        ))

    def heartbeat(self, task):
        self.stub.HeartbeatTask(agent_pb2.HeartbeatTaskRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], execution_generation=int(task["execution_generation"]),
        ))

    def complete(self, task, outcome: str, processed: int = 0, fd_id: str = "", error: str = ""):
        kind = agent_pb2.AGENT_TASK_TYPE_ARCHIVE if task["task_type"] == "ARCHIVE" else agent_pb2.AGENT_TASK_TYPE_DIAGNOSIS
        ack = self.stub.CompleteTask(agent_pb2.CompleteTaskRequest(
            task_id=task["task_id"], episode_id=task["episode_id"], task_type=kind,
            execution_generation=int(task["execution_generation"]), outcome=outcome,
            processed_input_version=int(processed), fd_id=fd_id, error_code=error, error_message=error,
        ))
        if not ack.accepted:
            raise RuntimeError(ack.message or "complete rejected")


def _ts(value: datetime):
    from google.protobuf.timestamp_pb2 import Timestamp
    stamp = Timestamp()
    stamp.FromDatetime(value.astimezone(timezone.utc))
    return stamp


class ExecutionService(agent_pb2_grpc.AgentExecutionServiceServicer):
    def __init__(self, dsn: str, go_target: str):
        self.dsn = dsn
        self.go_target = go_target
        self.model = model_from_env()
        self.archive_model = ArchiveModel()
        self.checkpointer = _checkpointer(dsn)
        self.running = set()
        self.lock = threading.Lock()
        self.channel = grpc.insecure_channel(go_target)

    def SubmitTask(self, request, context):
        task = _task_from_request(request)
        conn = connect(self.dsn)
        try:
            result = accept_task(conn, task)
        finally:
            conn.close()
        if result.get("accepted") and not result.get("duplicate"):
            task["thread_id"] = result["thread_id"]
            self.enqueue(task)
        return agent_pb2.SubmitTaskResponse(
            accepted=bool(result.get("accepted")), duplicate=bool(result.get("duplicate")),
            task_id=task["task_id"], receipt_status=result.get("status", ""), message=result.get("message", ""),
        )

    def GetArtifact(self, request, context):
        from alertflow_agent.artifacts import read_artifact
        kind = "DIAGNOSIS_REPORT" if request.artifact_type == agent_pb2.ARTIFACT_TYPE_DIAGNOSIS_REPORT else "INVESTIGATION"
        conn = connect(self.dsn)
        try:
            row = read_artifact(conn, request.episode_id, kind)
        finally:
            conn.close()
        if row is None:
            context.abort(grpc.StatusCode.NOT_FOUND, "artifact not found")
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

    def enqueue(self, task: dict):
        with self.lock:
            if task["task_id"] in self.running:
                return
            self.running.add(task["task_id"])
        threading.Thread(target=self._run, args=(task,), daemon=True).start()

    def _run(self, task: dict):
        conn = connect(self.dsn)
        thread_id = task.get("thread_id") or f"diagnosis:{task['episode_id']}"
        key = advisory_lock(conn, thread_id)
        client = SystemClient(agent_pb2_grpc.SystemGatewayServiceStub(self.channel), conn)
        try:
            mark_running(conn, task["task_id"])
            client.started(task)
            client.heartbeat(task)
            if task["task_type"] == "ARCHIVE":
                snap = client.get_input(task)
                task["service_id"] = snap.service_id
                fd_id = run_archive(self.archive_model, client, task)
                client.complete(task, "SUCCEEDED", fd_id=fd_id)
            else:
                snap = client.get_input(task)
                task["service_name"] = snap.service_name
                task["service_id"] = snap.service_id
                task["episode_started_at"] = snap.episode_started_at.ToDatetime(timezone.utc).isoformat()
                run_diagnosis(self.model, client, task, self.checkpointer)
                client.complete(task, "SUCCEEDED", processed=int(task.get("input_version_to") or 0))
            mark_finished(conn, task["task_id"], "COMPLETED", {"outcome": "SUCCEEDED"}, "CONFIRMED")
        except Exception as exc:
            try:
                client.complete(task, "RETRYABLE_FAILURE", error=type(exc).__name__ + ": " + str(exc)[:400])
            except Exception:
                pass
            try:
                mark_finished(conn, task["task_id"], "FAILED", {"error": str(exc)[:400]}, "SENT")
            except Exception:
                conn.rollback()
        finally:
            advisory_unlock(conn, key)
            conn.close()
            with self.lock:
                self.running.discard(task["task_id"])

    def recover_once(self):
        conn = connect(self.dsn)
        try:
            rows = unfinished(conn)
        finally:
            conn.close()
        for row in rows:
            row["thread_id"] = row.get("thread_id") or f"diagnosis:{row['episode_id']}"
            self.enqueue(row)


def _service_of_episode(conn, episode_id: str) -> str:
    with conn.cursor() as cur:
        cur.execute("SELECT service_id FROM system.episodes WHERE episode_id=%s", (episode_id,))
        row = cur.fetchone()
    if row is None:
        raise RuntimeError("episode not found")
    return str(row[0])


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


def _checkpointer(dsn: str):
    if PostgresSaver is None:
        return None
    conn = Connection.connect(dsn, autocommit=True, prepare_threshold=0, row_factory=dict_row)
    conn.execute("SET search_path TO cognitive, public")
    saver = PostgresSaver(conn)
    saver.setup()
    return saver


def serve():
    dsn = os.environ["ALERTFLOW_DATABASE_URL"]
    target = os.environ.get("ALERTFLOW_GRPC_TARGET", "127.0.0.1:9090")
    service = ExecutionService(dsn, target)
    server = grpc.server(futures_server())
    agent_pb2_grpc.add_AgentExecutionServiceServicer_to_server(service, server)
    addr = os.environ.get("AGENT_GRPC_ADDR", "0.0.0.0:9000")
    server.add_insecure_port(addr)
    server.start()
    threading.Thread(target=_recover_loop, args=(service,), daemon=True).start()
    server.wait_for_termination()


def futures_server():
    from concurrent import futures
    return futures.ThreadPoolExecutor(max_workers=8)


def _recover_loop(service: ExecutionService):
    import time
    while True:
        time.sleep(2)
        try:
            service.recover_once()
        except Exception:
            pass
