"""gRPC client for AlertFlowControlService.

Method names match the old HTTP client. Returned dicts keep the camelCase
keys the rest of the agent already reads. There is no HTTP fallback.
"""

from datetime import datetime, timezone

import grpc

from alertflow_agent.generated.alertflow.agent.v1 import agent_pb2, agent_pb2_grpc


def _epoch(value) -> int:
    if isinstance(value, (int, float)):
        return int(value)
    text = str(value).replace("Z", "+00:00")
    return int(datetime.fromisoformat(text).timestamp())


class AlertFlowClient:
    def __init__(self, target: str, token: str, timeout: float = 10) -> None:
        self.target = (target or "").strip()
        self.token = token or ""
        self.timeout = timeout
        self._channel = None
        self._stub = None

    def _rpc(self):
        if not self.target or not self.token:
            raise RuntimeError("alertflow grpc target is not configured")
        if self._stub is None:
            self._channel = grpc.insecure_channel(self.target)
            self._stub = agent_pb2_grpc.AlertFlowControlServiceStub(self._channel)
        return self._stub

    def _meta(self):
        return [("authorization", f"Bearer {self.token}")]

    def close(self) -> None:
        if self._channel is not None:
            self._channel.close()
            self._channel = None
            self._stub = None

    def runtime_config(self) -> dict:
        msg = self._rpc().GetRuntimeConfig(
            agent_pb2.GetRuntimeConfigRequest(), timeout=self.timeout, metadata=self._meta(),
        )
        return {
            "memory_enabled": msg.memory_enabled,
            "context_budget_tokens": msg.context_budget_tokens,
            "llm": _endpoint(msg.llm),
            "embed": _endpoint(msg.embed),
            "rerank": _endpoint(msg.rerank),
        }

    def incidents(self, service_id: str, limit: int = 10) -> dict:
        msg = self._rpc().ListIncidents(
            agent_pb2.ListIncidentsRequest(service_id=service_id, limit=limit),
            timeout=self.timeout, metadata=self._meta(),
        )
        return {"incidents": [_incident(row) for row in msg.incidents], "total": msg.total}

    def incident(self, incident_id: int) -> dict:
        msg = self._rpc().GetIncident(
            agent_pb2.GetIncidentRequest(id=int(incident_id)),
            timeout=self.timeout, metadata=self._meta(),
        )
        out = _incident(msg.incident)
        out["comments"] = [
            {"id": row.id, "username": row.username, "body": row.body, "createdAt": row.created_at}
            for row in msg.comments
        ]
        return out

    def metrics(self, service_id: str, range_: str = "24h") -> dict:
        msg = self._rpc().GetMetrics(
            agent_pb2.GetMetricsRequest(service_id=service_id, range=range_),
            timeout=self.timeout, metadata=self._meta(),
        )
        series = []
        for point in msg.series:
            series.append({
                "ts": point.ts,
                "avgLatency": point.avg_latency if point.has_avg_latency else None,
                "errors": point.errors,
                "warnings": point.warnings,
            })
        return {
            "serviceId": msg.service_id,
            "name": msg.name,
            "url": msg.url,
            "to": msg.to,
            "bucketSeconds": msg.bucket_seconds,
            "uptimeWindows": {"days7": msg.uptime_7d, "days30": msg.uptime_30d, "days365": msg.uptime_365d},
            "series": series,
            "from": getattr(msg, "from"),
        }

    def checks(self, service_id: str, before: int | None, limit: int) -> dict:
        req = agent_pb2.ListChecksRequest(service_id=service_id, limit=limit)
        if before:
            req.before = int(before)
        msg = self._rpc().ListChecks(req, timeout=self.timeout, metadata=self._meta())
        return {
            "checks": [_check(row) for row in msg.checks],
            "nextBefore": msg.next_before or None,
        }

    def timeline(self, service_id: str, start: int, end: int, sources: list[str], limit: int = 100,
                 cursor_at: int = 0, cursor_id: int = 0) -> dict:
        req = agent_pb2.GetTimelineRequest(
            service_id=service_id, to=int(end), sources=list(sources),
            limit=limit, cursor_at=int(cursor_at), cursor_id=int(cursor_id),
        )
        setattr(req, "from", int(start))
        msg = self._rpc().GetTimeline(req, timeout=self.timeout, metadata=self._meta())
        return {
            "events": [_timeline_event(row) for row in msg.events],
            "hasNext": msg.has_next,
            "nextCursorAt": msg.next_cursor_at,
            "nextCursorId": msg.next_cursor_id,
            "from": getattr(msg, "from"),
            "to": msg.to,
            "retentionDays": msg.retention_days,
            "coverage": [
                {
                    "source": row.source,
                    "status": row.status,
                    "lastSuccessAt": row.last_success_at if row.has_last_success_at else None,
                    "lastError": row.last_error,
                }
                for row in msg.coverage
            ],
        }

    def source_coverage(self, service_id: str) -> dict:
        msg = self._rpc().GetSourceCoverage(
            agent_pb2.GetSourceCoverageRequest(service_id=service_id),
            timeout=self.timeout, metadata=self._meta(),
        )
        return {
            "retentionDays": msg.retention_days,
            "sources": [
                {"source": row.source, "status": row.status, "lastError": row.last_error}
                for row in msg.sources
            ],
        }

    def kubernetes_state(self, service_id: str) -> dict:
        msg = self._rpc().GetKubernetesState(
            agent_pb2.GetKubernetesStateRequest(service_id=service_id),
            timeout=self.timeout, metadata=self._meta(),
        )
        return {
            "objects": [
                {
                    "clusterId": row.cluster_id, "namespace": row.namespace, "kind": row.kind,
                    "name": row.name, "uid": row.uid, "active": row.active,
                    "lastSeenAt": row.last_seen_at, "status": row.status,
                }
                for row in msg.objects
            ]
        }

    def submit_callback(self, payload: dict) -> dict:
        body = payload.get("payload") if isinstance(payload.get("payload"), dict) else {}
        report = body.get("report") if isinstance(body.get("report"), dict) else None
        evidence = body.get("evidence") if isinstance(body.get("evidence"), dict) else None
        usage = body.get("usage") if isinstance(body.get("usage"), dict) else None
        req = agent_pb2.AgentCallback(
            schema_version=str(payload.get("schema_version") or "v2"),
            message_id=str(payload.get("message_id") or ""),
            message_type=str(payload.get("message_type") or ""),
            correlation_id=str(payload.get("correlation_id") or ""),
            run_id=str(body.get("run_id") or ""),
            service_id=str(body.get("service_id") or ""),
            event_type=str(body.get("event_type") or ""),
            mode=str(body.get("mode") or ""),
            status=str(body.get("status") or ""),
            error=str(body.get("error") or ""),
            request_id=str(body.get("request_id") or ""),
            reply=str(body.get("reply") or ""),
        )
        if body.get("incident_id"):
            req.incident_id = int(body["incident_id"])
            req.has_incident_id = True
        if report is not None:
            req.report.update(report)
        if evidence is not None:
            req.evidence.update(evidence)
        if usage is not None:
            req.usage.update(usage)
        ids = body.get("evidence_ids")
        if isinstance(ids, list):
            req.evidence_ids.extend(str(item) for item in ids)
        msg = self._rpc().SubmitAgentCallback(req, timeout=self.timeout, metadata=self._meta())
        return {"stored": msg.stored, "duplicate": msg.duplicate}


def _endpoint(msg) -> dict:
    out = {"base_url": msg.base_url, "api_key": msg.api_key, "model": msg.model}
    if msg.dim:
        out["dim"] = msg.dim
    if msg.style:
        out["style"] = msg.style
    return out


def _incident(msg) -> dict:
    return {
        "id": msg.id,
        "serviceId": msg.service_id,
        "serviceName": msg.service_name,
        "status": msg.status,
        "source": msg.source,
        "severity": msg.severity,
        "title": msg.title,
        "startedAt": msg.started_at,
        "resolvedAt": msg.resolved_at if msg.has_resolved_at else None,
        "createdAt": msg.created_at,
        "updatedAt": msg.updated_at,
    }


def _check(row) -> dict:
    return {
        "id": row.id,
        "ts": row.ts,
        "status": row.status,
        "latencyMs": row.latency_ms if row.has_latency_ms else None,
        "statusCode": row.status_code if row.has_status_code else None,
        "error": row.error,
        "attempts": row.attempts or 1,
    }


def _timeline_event(row) -> dict:
    return {
        "id": row.id,
        "eventKey": row.event_key,
        "serviceId": row.service_id,
        "source": row.source,
        "eventType": row.event_type,
        "severity": row.severity,
        "title": row.title,
        "summary": row.summary,
        "occurredAt": row.occurred_at,
        "observedAt": row.observed_at,
        "recordedAt": row.recorded_at,
        "incidentId": row.incident_id if row.has_incident_id else None,
    }


def normalize_check(row: dict) -> dict:
    return {
        "id": row.get("id"),
        "ts": _epoch(row["ts"]) if not isinstance(row.get("ts"), (int, float)) else int(row["ts"]),
        "status": row.get("status"),
        "latency_ms": row.get("latencyMs", row.get("latency_ms")),
        "status_code": row.get("statusCode", row.get("status_code")),
        "error": row.get("error") or "",
        "attempts": row.get("attempts") or 1,
    }


def collect_checks(client: AlertFlowClient, service_id: str, window_from: int, window_to: int, max_rows: int) -> list[dict]:
    rows = []
    before = None
    while len(rows) < max_rows:
        page = client.checks(service_id, before, min(500, max_rows - len(rows)))
        batch = page.get("checks") or []
        if not batch:
            break
        for raw in batch:
            row = normalize_check(raw)
            if row["ts"] < window_from:
                return [item for item in rows if window_from <= item["ts"] <= window_to]
            if row["ts"] <= window_to:
                rows.append(row)
        nxt = page.get("nextBefore")
        if not nxt:
            break
        before = int(nxt)
    return rows


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()
