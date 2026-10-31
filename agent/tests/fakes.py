def event_row(mode: str) -> dict:
    return {
        "id": "11111111-1111-1111-1111-111111111111",
        "dedupe_key": "incident_start:1",
        "incident_id": 1,
        "service_id": "api",
        "event_type": "incident_start",
        "mode": mode,
        "status": "queued",
        "stage": "queued",
        "input_event": {
            "schema_version": "v1",
            "event_id": "incident_start:1",
            "event": "incident_start",
            "incident_id": 1,
            "service_id": "api",
            "service_name": "API",
            "service_url": "https://example.com/health",
            "status": "down",
            "occurred_at": "2026-10-11T10:00:00Z",
        },
        "usage": {},
    }


class FakeAlertFlow:
    def incident(self, incident_id: int) -> dict:
        return {
            "id": incident_id,
            "serviceId": "api",
            "status": "ongoing",
            "severity": "outage",
            "title": "API down",
            "comments": [],
        }

    def incidents(self, service_id: str, limit: int = 10) -> dict:
        return {"incidents": [{"id": 1, "serviceId": service_id, "status": "ongoing", "severity": "outage", "title": "API down"}], "total": 1}

    def metrics(self, service_id: str, range_: str = "24h") -> dict:
        return {"check": {"interval": 60, "timeout": 10}, "series": [{"ts": 1, "avgLatency": 20}], "tls": None}

    def checks(self, service_id: str, before, limit: int) -> dict:
        return {
            "checks": [
                {"id": 10, "ts": "2026-10-11T09:30:00Z", "status": "online", "latencyMs": 20, "statusCode": 200, "error": "", "attempts": 1},
                {"id": 11, "ts": "2026-10-11T09:59:00Z", "status": "offline", "latencyMs": 15, "statusCode": 503, "error": "Service Unavailable", "attempts": 1},
            ],
            "nextBefore": None,
        }

    def timeline(self, service_id: str, start=None, end=None, sources=None, limit: int = 50) -> dict:
        return {
            "events": [
                {
                    "id": 21, "eventKey": "kubernetes.pod.changed:api-1",
                    "source": "kubernetes", "eventType": "kubernetes.pod.changed",
                    "occurredAt": "2026-10-11T09:58:00Z", "observedAt": "2026-10-11T09:58:05Z",
                    "recordedAt": "2026-10-11T09:58:05Z", "title": "Pod api-1 restarted",
                    "kind": "pod", "severity": "warning",
                    "clusterId": "lab", "namespace": "default",
                    "resourceKind": "Pod", "resourceName": "api-1", "resourceUid": "uid-api-1",
                    "details": {"reason": "OOMKilled"},
                },
                {
                    "id": 22, "eventKey": "incident:1",
                    "source": "incident", "eventType": "incident.opened",
                    "occurredAt": "2026-10-11T09:59:30Z", "observedAt": "2026-10-11T09:59:30Z",
                    "recordedAt": "2026-10-11T09:59:31Z", "title": "Incident opened",
                    "clusterId": "lab",
                },
            ],
            "hasNext": False, "from": start, "to": end, "retentionDays": 7,
            "coverage": {"from": start, "to": end},
        }

    def kubernetes_state(self, service_id: str) -> dict:
        return {
            "objects": [
                {
                    "uid": "uid-api-1", "kind": "Pod", "name": "api-1", "namespace": "default",
                    "clusterId": "lab", "active": True, "summary": "Pod api-1 running",
                    "lastSeenAt": "2026-10-11T09:58:05Z", "fingerprint": "fp-1",
                }
            ]
        }
