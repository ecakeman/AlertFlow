#!/usr/bin/env python3
"""Tiny health target for the P0 audit. Modes: ok, 503, slow, flap, reset."""

from __future__ import annotations

import argparse
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODE = "ok"
LOCK = threading.Lock()
FLAP = {"n": 0}


class Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path != "/health":
            self.send_error(404)
            return
        with LOCK:
            mode = MODE
        if mode == "ok":
            self._send(200, b"ok")
        elif mode == "503":
            self._send(503, b"unavailable")
        elif mode == "slow":
            import time

            time.sleep(30)
            self._send(200, b"slow")
        elif mode == "flap":
            with LOCK:
                FLAP["n"] += 1
                bad = FLAP["n"] % 2 == 1
            self._send(503 if bad else 200, b"flap")
        elif mode == "reset":
            self.close_connection = True
            return
        else:
            self._send(500, b"unknown mode")

    def do_POST(self) -> None:  # noqa: N802
        global MODE
        if not self.path.startswith("/mode"):
            self.send_error(404)
            return
        query = self.path.split("?", 1)[1] if "?" in self.path else ""
        picked = ""
        for part in query.split("&"):
            if part.startswith("m="):
                picked = part[2:]
        if picked not in {"ok", "503", "slow", "flap", "reset"}:
            self._send(400, b"bad mode")
            return
        with LOCK:
            MODE = picked
            FLAP["n"] = 0
        self._send(200, picked.encode())

    def _send(self, code: int, body: bytes) -> None:
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args) -> None:
        return


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8099)
    args = parser.parse_args()
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
