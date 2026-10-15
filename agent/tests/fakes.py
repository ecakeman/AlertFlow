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
