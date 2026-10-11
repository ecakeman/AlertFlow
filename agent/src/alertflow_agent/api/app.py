import hmac
import json
import uuid
from datetime import datetime, timezone

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from alertflow_agent.runtime.verify import cited
from alertflow_agent.schemas.events import WebhookBody


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
        if len(raw) > 64 * 1024:
            return JSONResponse({"error": "body too large"}, status_code=413)
        try:
            payload = json.loads(raw)
            body = WebhookBody.model_validate(payload)
        except Exception:
            return JSONResponse({"error": "invalid request body"}, status_code=400)
        problem = body.problem()
        if problem:
            return JSONResponse({"error": problem}, status_code=400)
        event = body.model_dump(mode="json")
        if not event.get("service_url"):
            event["service_url"] = event.get("url") or ""
        if not event.get("service_name"):
            event["service_name"] = event.get("service") or event["service_id"]
        if body.event == "warning" and not settings.agent_enable_warning_runs:
            return {"run_id": None, "action": "ignored"}
        if body.event == "incident_resolve":
            return _resolve(store, body, event, settings)
        row = _row(event, "baseline" if body.event == "warning" else "agent")
        existing = store.insert_run(row)
        if existing:
            return JSONResponse({"run_id": existing["id"], "action": "duplicate"}, status_code=200)
        return JSONResponse({"run_id": row["id"], "action": "started"}, status_code=202)

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

    return app


def _api(request: Request, settings) -> bool:
    if not settings.agent_api_token:
        return False
    return _auth(request.headers.get("authorization", ""), settings.agent_api_token)


def _row(event: dict, mode: str) -> dict:
    return {
        "id": str(uuid.uuid4()),
        "dedupe_key": event["event_id"],
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


def _resolve(store, body, event, settings):
    found = store.find_incident(body.incident_id) if body.incident_id else None
    if not found:
        return {"run_id": None, "action": "ignored"}
    action = store.record_resolve(
        found["id"], body.event_id, event.get("resolved_at"), settings.agent_revise_on_resolve,
    )
    code = 202 if action in {"revise", "recorded", "resolved"} else 200
    return JSONResponse({"run_id": found["id"], "action": action}, status_code=code)
