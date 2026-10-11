#!/usr/bin/env python3
"""Write a contract webhook body the tests and send_event.py can post."""

import json
from pathlib import Path

EVENT = {
    "schema_version": "v1",
    "event_id": "incident_start:1",
    "event": "incident_start",
    "incident_id": 1,
    "service_id": "api",
    "service": "API",
    "url": "https://example.com/health",
    "status": "down",
    "occurred_at": "2026-10-11T10:00:00Z",
    "message": "API went down",
}

if __name__ == "__main__":
    path = Path(__file__).resolve().parent / "scenarios" / "incident_start.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(EVENT, indent=2) + "\n")
    print(path)
