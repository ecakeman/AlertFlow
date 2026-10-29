"""可选的 Prometheus / Loki MCP 客户端。

URL 为空时不创建对象。会话只在 ``call`` 里打开，一次调用一次连接。
"""

from __future__ import annotations

import asyncio
from datetime import timedelta

import httpx

PROMETHEUS_TOOLS = ("query", "range_query", "metric_metadata", "label_names", "label_values")
LOKI_TOOLS = ("loki_query",)


class MCPError(Exception):
    def __init__(self, kind: str):
        self.kind = kind


class HTTPSource:
    def __init__(self, url: str, token: str, timeout: float):
        self.url = url
        self.token = token
        self.timeout = timeout

    def call(self, name: str, args: dict | None) -> str:
        try:
            return asyncio.run(self._call(name, args or {}))
        except MCPError:
            raise
        except Exception as exc:
            raise MCPError(_kind(exc)) from None

    async def _call(self, name: str, args: dict) -> str:
        from mcp import ClientSession
        from mcp.client.streamable_http import streamablehttp_client

        headers = {"Authorization": "Bearer " + self.token} if self.token else None
        timeout = self.timeout
        try:
            async with streamablehttp_client(
                self.url, headers=headers, timeout=timeout, sse_read_timeout=timeout,
            ) as (read, write, _session_id):
                async with ClientSession(read, write) as session:
                    await session.initialize()
                    result = await session.call_tool(
                        name, args, read_timeout_seconds=timedelta(seconds=timeout),
                    )
        except MCPError:
            raise
        except Exception as exc:
            raise MCPError(_kind(exc)) from None
        return _text(result)


class MCPSources:
    def __init__(self, prometheus: HTTPSource | None, loki: HTTPSource | None):
        self.prometheus = prometheus
        self.loki = loki

    def call(self, name: str, args: dict | None) -> str:
        source = self.prometheus if name in PROMETHEUS_TOOLS else self.loki
        if source is None:
            raise MCPError("unreadable")
        return source.call(name, args)


def sources_from_settings(settings) -> MCPSources | None:
    prometheus = _source(getattr(settings, "prometheus_mcp_url", ""), getattr(settings, "prometheus_mcp_token", ""), settings)
    loki = _source(getattr(settings, "loki_mcp_url", ""), getattr(settings, "loki_mcp_token", ""), settings)
    if prometheus is None and loki is None:
        return None
    return MCPSources(prometheus, loki)


def enabled_mcp_tools(settings) -> list[str]:
    names: list[str] = []
    if str(getattr(settings, "prometheus_mcp_url", "") or "").strip():
        names.extend(PROMETHEUS_TOOLS)
    if str(getattr(settings, "loki_mcp_url", "") or "").strip():
        names.extend(LOKI_TOOLS)
    return names


def _source(url: str, token: str, settings) -> HTTPSource | None:
    url = str(url or "").strip()
    if not url:
        return None
    timeout = float(getattr(settings, "mcp_timeout_seconds", 10) or 10)
    return HTTPSource(url, str(token or ""), timeout)


def _text(result) -> str:
    if getattr(result, "isError", False):
        raise MCPError("unreadable")
    parts = []
    for block in getattr(result, "content", None) or []:
        text = getattr(block, "text", None)
        if isinstance(text, str) and text.strip():
            parts.append(text.strip())
    if not parts:
        raise MCPError("unreadable")
    return "\n".join(parts)[:2000]


def _kind(exc: BaseException) -> str:
    if isinstance(exc, (TimeoutError, asyncio.TimeoutError, httpx.TimeoutException)):
        return "timeout"
    status = getattr(getattr(exc, "response", None), "status_code", None)
    if status in (401, 403):
        return "auth"
    text = str(exc).lower()
    if any(mark in text for mark in ("unauthorized", "forbidden", "401", "403")):
        return "auth"
    if "timeout" in text or "timed out" in text:
        return "timeout"
    return "unreadable"
