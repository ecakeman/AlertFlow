"""把诊断结果回传给 AlertFlow。

两条设计要点：

* **报告落库与回调入队在同一个数据库事务里完成**——否则会出现「报告已经写好、
  进程在发出回调前挂掉、之后再也不回传」的窗口。
* **发送是 at-least-once**：崩溃后重发同一条消息是安全的，因为 AlertFlow 按
  message_id 幂等。这里只保证"发不丢"，不保证"只发一次"。
"""

from __future__ import annotations

import json
import logging
import threading
from datetime import datetime, timezone

import grpc

log = logging.getLogger("alertflow_agent")

SCHEMA_VERSION = "v2"

# 回传的证据只保留报告引用到的那一份，并且有界。完整证据留在 Agent 这边。
MAX_EVIDENCE_ITEMS = 60
MAX_EVIDENCE_BYTES = 256 * 1024
MAX_ERROR_CHARS = 300

LEASE_SECONDS = 60
BACKOFF_BASE_SECONDS = 5
BACKOFF_MAX_SECONDS = 300


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _clip(value, limit: int) -> str:
    text = "" if value is None else str(value)
    return text if len(text) <= limit else text[:limit] + "…"


def _as_iso(value) -> str | None:
    if value is None:
        return None
    if hasattr(value, "isoformat"):
        return value.isoformat()
    return str(value)


def correlation_id(run_id: str, run: dict) -> str:
    incident_id = run.get("incident_id")
    if incident_id is None:
        incident_id = (run.get("input_event") or {}).get("incident_id")
    return f"incident:{incident_id}" if incident_id else f"run:{run_id}"


def _incident_id(run: dict):
    incident_id = run.get("incident_id")
    if incident_id is None:
        incident_id = (run.get("input_event") or {}).get("incident_id")
    return incident_id


def _envelope(run_id: str, message_id: str, message_type: str, run: dict, payload: dict) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "message_id": message_id,
        "message_type": message_type,
        "correlation_id": correlation_id(run_id, run),
        "sent_at": iso_now(),
        "payload": payload,
    }


def bounded_evidence(evidence: dict) -> dict:
    """只回传报告引用到的证据，并给条数与体积设限。

    放大块会把整条回调撑爆，所以超限时先丢 data 只留摘要；再超就整条不收。
    """
    out: dict = {}
    total = 0
    for key, value in list((evidence or {}).items())[:MAX_EVIDENCE_ITEMS]:
        if not isinstance(value, dict):
            continue
        try:
            size = len(json.dumps(value))
        except (TypeError, ValueError):
            continue
        if total + size > MAX_EVIDENCE_BYTES:
            value = {k: v for k, v in value.items() if k != "data"}
            try:
                size = len(json.dumps(value))
            except (TypeError, ValueError):
                continue
            if total + size > MAX_EVIDENCE_BYTES:
                continue
        out[key] = value
        total += size
    return out


def report_callback(run_id: str, run: dict, version: int, report: dict, usage: dict, evidence: dict) -> dict:
    """一次诊断完成的回传消息。

    message_id 带版本：resolve 触发的二次报告是另一条消息，而不是覆盖前一条。
    """
    payload = {
        "run_id": run_id,
        "incident_id": _incident_id(run),
        "service_id": run.get("service_id") or "",
        "event_type": run.get("event_type") or "",
        "mode": run.get("mode") or "",
        "status": report.get("status") or "completed",
        "report": report,
        "evidence": bounded_evidence(evidence),
        "usage": usage or {},
        "started_at": _as_iso(run.get("started_at")),
        "finished_at": iso_now(),
    }
    return _envelope(run_id, f"run:{run_id}:v{version}", "run.completed", run, payload)


def started_callback(run_id: str, run: dict) -> dict:
    """「这次诊断开始了」。它没有报告可带，只让 AlertFlow 知道有个运行在跑。

    这条不参与任何事务：丢了只是少一个进行中的提示，不影响结果回传。
    """
    payload = {
        "run_id": run_id,
        "incident_id": _incident_id(run),
        "service_id": run.get("service_id") or "",
        "event_type": run.get("event_type") or "",
        "mode": run.get("mode") or "",
        "status": "running",
        "started_at": _as_iso(run.get("started_at")) or iso_now(),
    }
    return _envelope(run_id, f"run:{run_id}:started", "run.started", run, payload)


def failure_callback(run_id: str, run: dict, reason: str) -> dict:
    """诊断失败的回传消息。error 只放安全的摘要，不放堆栈或配置。"""
    payload = {
        "run_id": run_id,
        "incident_id": _incident_id(run),
        "service_id": run.get("service_id") or "",
        "event_type": run.get("event_type") or "",
        "status": "failed",
        "error": _clip(reason, MAX_ERROR_CHARS),
        "finished_at": iso_now(),
    }
    return _envelope(run_id, f"run:{run_id}:failed", "run.failed", run, payload)


def chat_callback(request_id: str, incident_id, reply: str, evidence_ids: list) -> dict:
    """一条排查问题的回答。与 run 的回传走同一条 outbox / 同一套重试。"""
    envelope = {
        "schema_version": SCHEMA_VERSION,
        "message_id": f"chat:{request_id}:answered",
        "message_type": "chat.completed",
        "correlation_id": f"incident:{incident_id}" if incident_id else f"chat:{request_id}",
        "sent_at": iso_now(),
        "payload": {
            "request_id": request_id,
            "incident_id": incident_id,
            "reply": reply,
            "evidence_ids": list(evidence_ids or [])[:40],
            "finished_at": iso_now(),
        },
    }
    return envelope


def chat_failed_callback(request_id: str, incident_id, reason: str) -> dict:
    return {
        "schema_version": SCHEMA_VERSION,
        "message_id": f"chat:{request_id}:failed",
        "message_type": "chat.failed",
        "correlation_id": f"incident:{incident_id}" if incident_id else f"chat:{request_id}",
        "sent_at": iso_now(),
        "payload": {
            "request_id": request_id,
            "incident_id": incident_id,
            "error": _clip(reason, MAX_ERROR_CHARS),
            "finished_at": iso_now(),
        },
    }


def backoff_seconds(attempt: int) -> int:
    delay = BACKOFF_BASE_SECONDS
    for _ in range(1, max(attempt, 1)):
        delay *= 2
        if delay >= BACKOFF_MAX_SECONDS:
            return BACKOFF_MAX_SECONDS
    return delay


def callbacks_enabled(settings) -> bool:
    """gRPC 目标和令牌都在才入队。只配新令牌、或只配旧 HTTP 令牌，都不够。"""
    target = (getattr(settings, "alertflow_grpc_target", "") or "").strip()
    token = (getattr(settings, "alertflow_agent_grpc_token", "") or "").strip()
    if not token:
        token = (getattr(settings, "agent_callback_token", "") or "").strip()
    return bool(target and token)


def _permanent(status_code: int) -> bool:
    if status_code in (408, 429):
        return False
    return 400 <= status_code < 500


class CallbackSender:
    """把 outbox 里的待发回调送给 AlertFlow。

    思路与 Go 侧的投递 worker 一致：先领取（带租约），发送成功才标 sent；失败按
    退避重试，超过上限标 dead。进程崩溃留下的 sending 行在租约过期后会被重新领取。
    """

    def __init__(self, settings, store) -> None:
        self.settings = settings
        self.store = store
        self.target = (settings.alertflow_grpc_target or "").strip()
        self.token = settings.alertflow_agent_grpc_token or settings.agent_callback_token
        self.submit = None

    def configured(self) -> bool:
        return callbacks_enabled(self.settings)

    def run(self, stop: threading.Event) -> None:
        warned = False
        while not stop.is_set():
            if not self.configured():
                if not warned:
                    log.warning("callback is not configured; diagnosis results stay in the outbox")
                    warned = True
                stop.wait(self.settings.callback_poll_seconds)
                continue
            try:
                sent = self.drain()
            except Exception:
                log.exception("callback drain failed")
                sent = 0
            stop.wait(0.2 if sent else self.settings.callback_poll_seconds)

    def drain(self) -> int:
        rows = self.store.claim_callbacks(
            self.settings.callback_batch, LEASE_SECONDS, self.settings.callback_max_attempts,
        )
        sent = 0
        for row in rows:
            error, permanent = self._send(row)
            attempts = int(row.get("attempt_count") or 1)
            if error is None:
                self.store.finish_callback(row["id"], "sent", None, 0)
                sent += 1
            elif permanent or attempts >= self.settings.callback_max_attempts:
                self.store.finish_callback(row["id"], "dead", error, 0)
            else:
                self.store.finish_callback(row["id"], "retry_wait", error, backoff_seconds(attempts))
        return sent

    def _send(self, row: dict) -> tuple[str | None, bool]:
        try:
            if self.submit is not None:
                result = self.submit(row["payload"])
            else:
                from alertflow_agent.integrations.alertflow import AlertFlowClient

                result = AlertFlowClient(self.target, self.token).submit_callback(row["payload"])
        except grpc.RpcError as exc:
            code = exc.code()
            permanent = code in {grpc.StatusCode.INVALID_ARGUMENT, grpc.StatusCode.UNAUTHENTICATED, grpc.StatusCode.PERMISSION_DENIED}
            return _clip(code.name, MAX_ERROR_CHARS), permanent
        except Exception as exc:  # noqa: BLE001 - 网络问题一律可重试
            return _clip(type(exc).__name__, MAX_ERROR_CHARS), False
        if isinstance(result, dict) and (result.get("stored") or result.get("duplicate")):
            return None, False
        # 没有确认就重试。重复的 message_id 在 Go 侧是 duplicate，不会写出第二份报告。
        return _clip("callback was not stored", MAX_ERROR_CHARS), False
