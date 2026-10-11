#!/usr/bin/env python3
"""HTTP stand-in for GET /internal/agent/v1. Tests and local smoke can point at it."""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BODY = {
    "/internal/agent/v1/incidents/1": {
        "id": 1, "serviceId": "api", "status": "ongoing", "severity": "outage", "title": "API",
        "comments": [], "externalAlerts": [],
    }
}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path == "/internal/agent/v1/incidents":
            payload = {"incidents": [BODY["/internal/agent/v1/incidents/1"]], "total": 1}
        elif path == "/internal/agent/v1/services/api/metrics":
            payload = {"check": {"interval": 60}, "series": [], "tls": None}
        elif path == "/internal/agent/v1/services/api/checks":
            payload = {"checks": [], "nextBefore": None}
        else:
            payload = BODY.get(path)
        if payload is None:
            self.send_error(404)
            return
        raw = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, fmt, *args):
        return


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 8098), Handler).serve_forever()
