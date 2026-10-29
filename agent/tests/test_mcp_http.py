"""本地 Streamable HTTP。这不是 Prometheus 或 Loki 的集成测试。"""

import socket
import threading
import time

import uvicorn
from mcp.server.fastmcp import FastMCP
from starlette.applications import Starlette
from starlette.responses import PlainTextResponse
from starlette.routing import Route

from alertflow_agent.config import Settings
from alertflow_agent.integrations.mcp import HTTPSource, MCPError
from alertflow_agent.runtime.tools import run_tool

HITS = []


def query(query: str = "") -> str:
    HITS.append(("query", query))
    if query == "empty":
        return "[]"
    if query == "bad":
        raise RuntimeError("broken result")
    return "1"


def _server(app):
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning"))
    server.install_signal_handlers = lambda: None
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    deadline = time.time() + 5
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.2):
                return server, port
        except OSError:
            time.sleep(0.05)
    raise RuntimeError("mcp test server did not start")


def _bearer(app, token):
    async def asgi(scope, receive, send):
        if scope["type"] == "http":
            headers = {key.decode().lower(): value.decode() for key, value in scope.get("headers") or []}
            if headers.get("authorization") != f"Bearer {token}":
                await send({
                    "type": "http.response.start",
                    "status": 401,
                    "headers": [(b"content-type", b"text/plain")],
                })
                await send({"type": "http.response.body", "body": b"unauthorized"})
                return
        await app(scope, receive, send)

    return asgi


def _mcp_app():
    server = FastMCP("fake", json_response=True, stateless_http=True)
    server.add_tool(query)
    return _bearer(server.streamable_http_app(), "lab-token")


def test_streamable_http_initialize_discovery_call_and_auth():
    HITS.clear()
    server, port = _server(_mcp_app())
    try:
        url = f"http://127.0.0.1:{port}/mcp"
        source = HTTPSource(url, "lab-token", 5, 65536)
        assert source.call("query", {"query": "up"}) == "1"
        assert HITS == [("query", "up")]
        with pytest_raises(MCPError, "missing_tool"):
            source.call("range_query", {"query": "up"})
        assert HITS == [("query", "up")]
        with pytest_raises(MCPError, "missing_tool"):
            source.call("series", {})
        assert all(name == "query" for name, _ in HITS)
        denied = HTTPSource(url, "wrong-token", 5, 65536)
        with pytest_raises(MCPError, "auth"):
            denied.call("query", {"query": "up"})
        assert HITS == [("query", "up")]
        with pytest_raises(MCPError, "empty"):
            source.call("query", {"query": "empty"})
        with pytest_raises(MCPError, "malformed"):
            source.call("query", {"query": "bad"})
    finally:
        server.should_exit = True


SCHEMA = []


def prom_query(query: str = "", timestamp: str = "", truncation_limit: int = 0) -> str:
    SCHEMA.append(("query", query, timestamp, truncation_limit))
    return "1"


def prom_range(query: str = "", start_time: str = "", end_time: str = "", step: str = "", truncation_limit: int = 0) -> str:
    SCHEMA.append(("range_query", query, start_time, end_time, step, truncation_limit))
    return "1"


def prom_label_values(label: str = "", matches: list | None = None, start_time: str = "", end_time: str = "", truncation_limit: int = 0) -> str:
    SCHEMA.append(("label_values", label, list(matches or []), start_time, end_time, truncation_limit))
    return "api"


class _Bridge:
    def __init__(self, source):
        self.source = source

    def call(self, name, args):
        return self.source.call(name, args)


def test_v018_schema_server_receives_incident_time_and_selector():
    SCHEMA.clear()
    app = FastMCP("prom-schema", json_response=True, stateless_http=True)
    app.add_tool(prom_query, name="query")
    app.add_tool(prom_range, name="range_query")
    app.add_tool(prom_label_values, name="label_values")
    server, port = _server(app.streamable_http_app())
    try:
        source = HTTPSource(f"http://127.0.0.1:{port}/mcp", "", 5, 65536)
        settings = Settings(
            prometheus_mcp_url=f"http://127.0.0.1:{port}/mcp",
            prometheus_service_selectors='{"api":"service=\\"api\\""}',
            mcp_max_samples=100,
            mcp_max_window_seconds=3600,
        )
        state = {"event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z"}, "evidence": {}, "coverage": {"gaps": []}}
        assert run_tool("query", {"query": "up"}, state, settings, {"mcp": _Bridge(source)})["status"] == "success"
        assert run_tool("range_query", {"query": "rate(http_requests_total[5m])"}, state, settings, {"mcp": _Bridge(source)})["status"] == "success"
        assert run_tool("label_values", {"label": "service"}, state, settings, {"mcp": _Bridge(source)})["status"] == "success"
    finally:
        server.should_exit = True
    assert SCHEMA[0] == ("query", 'up{service="api"}', "2026-10-11T10:00:00Z", 100)
    assert SCHEMA[1] == (
        "range_query",
        'rate(http_requests_total{service="api"}[5m])',
        "2026-10-11T09:00:00Z",
        "2026-10-11T10:00:00Z",
        "36s",
        100,
    )
    assert SCHEMA[2] == (
        "label_values",
        "service",
        ['{service="api"}'],
        "2026-10-11T09:00:00Z",
        "2026-10-11T10:00:00Z",
        100,
    )


def test_http_429_timeout_and_disconnect_are_distinct():
    async def limited(request):
        return PlainTextResponse("too many requests", status_code=429)

    server, port = _server(Starlette(routes=[Route("/mcp", limited, methods=["GET", "POST"])]))
    try:
        source = HTTPSource(f"http://127.0.0.1:{port}/mcp", "", 2, 65536)
        with pytest_raises(MCPError, "rate_limit"):
            source.call("query", {"query": "up"})
    finally:
        server.should_exit = True

    closed = socket.socket()
    closed.bind(("127.0.0.1", 0))
    closed_port = closed.getsockname()[1]
    closed.close()
    with pytest_raises(MCPError, "disconnect"):
        HTTPSource(f"http://127.0.0.1:{closed_port}/mcp", "", 1, 65536).call("query", {"query": "up"})

    hanging = socket.socket()
    hanging.bind(("127.0.0.1", 0))
    hanging.listen(1)
    hanging_port = hanging.getsockname()[1]
    try:
        with pytest_raises(MCPError, "timeout"):
            HTTPSource(f"http://127.0.0.1:{hanging_port}/mcp", "", 0.4, 65536).call("query", {"query": "up"})
    finally:
        hanging.close()


def pytest_raises(kind_type, kind):
    import pytest
    catcher = pytest.raises(kind_type)
    catcher._expected_kind = kind
    return _Kind(catcher, kind)


class _Kind:
    def __init__(self, catcher, kind):
        self.catcher = catcher
        self.kind = kind

    def __enter__(self):
        self.caught = self.catcher.__enter__()
        return self.caught

    def __exit__(self, exc_type, exc, tb):
        result = self.catcher.__exit__(exc_type, exc, tb)
        if result:
            assert self.caught.value.kind == self.kind
        return result
