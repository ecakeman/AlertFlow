import hmac
import json
import uuid
from datetime import datetime, timezone

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from alertflow_agent.runtime.verify import cited
from alertflow_agent.schemas.events import (
    CHAT_MESSAGE_TYPE,
    TEST_MESSAGE_TYPE,
    AgentInbound,
    WebhookBody,
    normalize_event,
    sanitize_context,
)


def _bearer(header: str) -> str | None:
    if not header:
        return None
    scheme, _, token = header.partition(" ")
    if scheme.lower() != "bearer" or not token or " " in token.strip():
        return None
    return token.strip()


def _auth(header: str, token: str) -> bool:
    if not token:
        return False
    given = _bearer(header)
    if given is None:
        hmac.compare_digest(token, token)
        return False
    if len(given) != len(token):
        hmac.compare_digest(token, token)
        return False
    return hmac.compare_digest(given, token)


def create_app(settings, store, runner) -> FastAPI:
    app = FastAPI()
    app.state.settings = settings
    app.state.store = store
    app.state.runner = runner

    @app.get("/healthz")
    def healthz():
        return {"ok": True}

    @app.post("/webhooks/alertflow")
    async def webhook(request: Request):
        if not settings.agent_webhook_token:
            return JSONResponse({"error": "webhook token is not configured"}, status_code=503)
        if not _auth(request.headers.get("authorization", ""), settings.agent_webhook_token):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        raw = await request.body()
        if len(raw) > settings.webhook_max_bytes:
            return JSONResponse({"error": "body too large"}, status_code=413)
        try:
            payload = json.loads(raw)
        except Exception:
            return JSONResponse({"error": "invalid request body"}, status_code=400)
        if not isinstance(payload, dict):
            return JSONResponse({"error": "invalid request body"}, status_code=400)
        # v2 是带默认上下文的信封；v1 是早期不带上下文的默认载荷。两条都在，
        # 直到确认线上不再有 v1 的发送方。
        if str(payload.get("schema_version") or "") == "v2":
            return _ingest_v2(payload, store, settings)
        return _ingest_v1(payload, store, settings)

    @app.get("/api/v1/runs")
    def list_runs(request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return {"runs": store.list_runs()}

    @app.get("/api/v1/runs/{run_id}")
    def get_run(run_id: str, request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        row = store.get(run_id)
        if not row:
            return JSONResponse({"error": "not found"}, status_code=404)
        return row

    @app.get("/api/v1/runs/{run_id}/events")
    def events(run_id: str, request: Request, after_seq: int = 0):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return {"events": store.list_events(run_id, after_seq)}

    @app.get("/api/v1/runs/{run_id}/evidence")
    def evidence(run_id: str, request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        return {"evidence": store.get_evidence(run_id)}

    @app.get("/api/v1/runs/{run_id}/report")
    def report(run_id: str, request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        saved = store.latest_report(run_id)
        if not saved:
            return JSONResponse({"error": "not found"}, status_code=404)
        used = cited(saved["report"])
        evidence = {k: v for k, v in store.get_evidence(run_id).items() if k in used}
        return {"report": saved["report"], "evidence": evidence}

    @app.post("/api/v1/runs/{run_id}/resume")
    def resume(run_id: str, request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        if not store.get(run_id):
            return JSONResponse({"error": "not found"}, status_code=404)
        if not store.transition(run_id, {"failed", "partial"}, status="queued", stage="resume"):
            return JSONResponse({"error": "run cannot be resumed"}, status_code=409)
        return JSONResponse({"run_id": run_id, "action": "resume"}, status_code=202)

    @app.post("/api/v1/runs/{run_id}/cancel")
    def cancel(run_id: str, request: Request):
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        if not store.get(run_id):
            return JSONResponse({"error": "not found"}, status_code=404)
        if not store.transition(run_id, {"queued", "running", "failed", "partial"}, status="cancelled", finished_at=datetime.now(timezone.utc).isoformat()):
            return JSONResponse({"error": "run cannot be cancelled"}, status_code=409)
        return {"run_id": run_id, "action": "cancelled"}

    @app.post("/api/v1/incidents/{incident_id}/chat")
    async def incident_chat(incident_id: int, request: Request):
        """就某次事件与 Agent 对话：只读证据 + 报告 + 长期记忆，无工具、无写操作。"""
        if not _api(request, settings):
            return JSONResponse({"error": "unauthorized"}, status_code=401)
        try:
            body = await request.json()
        except Exception:
            return JSONResponse({"error": "invalid body"}, status_code=400)
        question = str((body or {}).get("message") or "").strip()
        if not question:
            return JSONResponse({"error": "message is required"}, status_code=400)
        history = (body or {}).get("history")
        if not isinstance(history, list):
            history = []

        from alertflow_agent.runtime.chat import answer as chat_answer

        try:
            return chat_answer(
                settings, store, getattr(runner, "memory", None), getattr(runner, "model", None),
                incident_id, history[-8:], question[:2000],
            )
        except Exception:  # noqa: BLE001 - 答不出来要如实说，而不是回一句套话
            return JSONResponse({"error": "the agent could not answer"}, status_code=502)

    return app


def _api(request: Request, settings) -> bool:
    if not settings.agent_api_token:
        return False
    return _auth(request.headers.get("authorization", ""), settings.agent_api_token)


def _ingest_v1(payload: dict, store, settings):
    """v1：早期默认 webhook，没有下推上下文，Agent 自己去拉。"""
    try:
        body = WebhookBody.model_validate(payload)
    except Exception:
        return JSONResponse({"error": "invalid request body"}, status_code=400)
    problem = body.problem()
    if problem:
        return JSONResponse({"error": problem}, status_code=400)
    event = normalize_event(body.model_dump(mode="json"))
    if body.event == "warning" and not settings.agent_enable_warning_runs:
        return {"run_id": None, "action": "ignored"}
    if body.event == "incident_resolve":
        return _resolve(store, event, body.event_id, settings)
    row = _row(event, "baseline" if body.event == "warning" else "agent", dedupe=event["event_id"])
    existing = store.insert_run(row)
    if existing:
        return JSONResponse({"run_id": existing["id"], "action": "duplicate"}, status_code=200)
    return JSONResponse({"run_id": row["id"], "action": "started"}, status_code=202)


def _ingest_v2(payload: dict, store, settings):
    """v2：带默认上下文的信封。幂等键用 message_id。"""
    try:
        envelope = AgentInbound.model_validate(payload)
    except Exception:
        return JSONResponse({"error": "invalid request body"}, status_code=400)
    problem = envelope.problem()
    if problem:
        return JSONResponse({"error": problem}, status_code=400)
    if envelope.message_type == TEST_MESSAGE_TYPE:
        # 连通性/鉴权自测：不建 run，免得留下一堆没有事件的空诊断。
        return {"message_id": envelope.message_id, "action": "ignored"}
    if envelope.message_type == CHAT_MESSAGE_TYPE:
        return _enqueue_chat(envelope, store)
    event = normalize_event(envelope.payload.get("event") or {})
    if not event.get("event_id"):
        event["event_id"] = envelope.message_id
    context = envelope.payload.get("diagnostic_context")
    if isinstance(context, dict):
        # 上下文随 run 一起持久化在 input_event 里，run 开始时再取出来放进 state；
        # 它不进 state["event"]，免得被当成事件字段喂给模型。
        event["diagnostic_context"] = sanitize_context(context)
    if event.get("event") == "warning" and not settings.agent_enable_warning_runs:
        return {"message_id": envelope.message_id, "action": "ignored"}
    if event.get("event") == "incident_resolve":
        return _resolve(store, event, envelope.message_id, settings)
    row = _row(event, "baseline" if event.get("event") == "warning" else "agent", dedupe=envelope.message_id)
    existing = store.insert_run(row)
    if existing:
        return JSONResponse({"run_id": existing["id"], "action": "duplicate"}, status_code=200)
    return JSONResponse({"run_id": row["id"], "action": "started"}, status_code=202)


def _enqueue_chat(envelope: AgentInbound, store):
    """把一条排查问题排进队列。

    这里只入库、立刻返回：真正的回答由 chat worker 在后台做，答案经回调 outbox 回传。
    把它当成 incident_start 会是灾难——那会开一次诊断，而不是回答一个问题。
    """
    payload = envelope.payload
    request_id = str(payload.get("request_id") or "").strip()
    question = str(payload.get("message") or "").strip()
    if not request_id or not question:
        return JSONResponse({"error": "request_id and message are required"}, status_code=400)
    history = payload.get("history")
    if not isinstance(history, list):
        history = []
    incident_id = payload.get("incident_id")
    created = store.enqueue_chat_request({
        "request_id": request_id,
        "incident_id": int(incident_id) if incident_id is not None else 0,
        "message": question[:4000],
        "history": history[-8:],
    })
    return JSONResponse(
        {"request_id": request_id, "action": "started" if created else "duplicate"},
        status_code=202 if created else 200,
    )


def _row(event: dict, mode: str, dedupe: str | None = None) -> dict:
    return {
        "id": str(uuid.uuid4()),
        "dedupe_key": dedupe or event["event_id"],
        "incident_id": event.get("incident_id"),
        "service_id": event["service_id"],
        "event_type": event["event"],
        "mode": mode,
        "status": "queued",
        "stage": "queued",
        "input_event": event,
        "usage": {},
    }


def _id_for(store, key: str) -> str:
    for row in store.list_runs():
        if row["dedupe_key"] == key:
            return row["id"]
    raise KeyError(key)


def _resolve(store, event: dict, event_id: str, settings):
    incident_id = event.get("incident_id")
    found = store.find_incident(incident_id) if incident_id else None
    if not found:
        return {"run_id": None, "action": "ignored"}
    action = store.record_resolve(
        found["id"], event_id, event.get("resolved_at"), settings.agent_revise_on_resolve,
    )
    code = 202 if action in {"revise", "recorded", "resolved"} else 200
    return JSONResponse({"run_id": found["id"], "action": action}, status_code=code)
