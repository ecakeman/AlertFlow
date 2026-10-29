"""可选的 Prometheus / Loki MCP 客户端。

URL 为空时不创建对象。会话只在 ``call`` 里打开：先 initialize，再列出工具，
然后只调用本项目批准且服务器确实提供的只读工具。
"""

from __future__ import annotations

import asyncio
import json
import re
from datetime import timedelta

import httpx

PROMETHEUS_TOOLS = ("query", "range_query", "metric_metadata", "label_names", "label_values")
LOKI_TOOLS = ("loki_query",)
SCOPED_TOOLS = frozenset({"query", "range_query", "label_values", "loki_query"})
EVIDENCE_TOOLS = SCOPED_TOOLS
APPROVED_TOOLS = frozenset(PROMETHEUS_TOOLS) | frozenset(LOKI_TOOLS)

_REJECT_PROMQL = re.compile(r"\b(or|unless|by|without)\b", re.IGNORECASE)
_IDENT = re.compile(r"[A-Za-z_:][A-Za-z0-9_:]*")
_MATCHER = re.compile(r'([A-Za-z_][A-Za-z0-9_]*)\s*(=~|!~|=|!=)\s*"([^"]*)"')


class MCPError(Exception):
    def __init__(self, kind: str):
        self.kind = kind


class _CappedStream(httpx.AsyncByteStream):
    def __init__(self, inner: httpx.AsyncByteStream, limit: int):
        self._inner = inner
        self._limit = limit

    async def __aiter__(self):
        seen = 0
        async for chunk in self._inner:
            seen += len(chunk)
            if seen > self._limit:
                raise MCPError("too_large")
            yield chunk

    async def aclose(self) -> None:
        await self._inner.aclose()


class CappedTransport(httpx.AsyncBaseTransport):
    """读取响应时计数，超过上限立刻停止，不等待剩余字节。"""

    def __init__(self, inner: httpx.AsyncBaseTransport, limit: int):
        self._inner = inner
        self._limit = limit

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        response = await self._inner.handle_async_request(request)
        response.stream = _CappedStream(response.stream, self._limit)
        return response

    async def aclose(self) -> None:
        await self._inner.aclose()


class HTTPSource:
    def __init__(self, url: str, token: str, timeout: float, max_bytes: int):
        self.url = url
        self.token = token
        self.timeout = timeout
        self.max_bytes = max_bytes

    def call(self, name: str, args: dict | None) -> str:
        try:
            return asyncio.run(self._call(name, args or {}))
        except MCPError:
            raise
        except Exception as exc:
            raise MCPError(_kind(exc)) from None

    async def _call(self, name: str, args: dict) -> str:
        from mcp import ClientSession
        from mcp.client.streamable_http import streamable_http_client

        if name not in APPROVED_TOOLS:
            raise MCPError("missing_tool")
        headers = {"Authorization": "Bearer " + self.token} if self.token else None
        timeout = self.timeout
        client = httpx.AsyncClient(
            transport=CappedTransport(httpx.AsyncHTTPTransport(), self.max_bytes),
            headers=headers,
            timeout=httpx.Timeout(timeout, read=timeout),
        )

        try:
            async with client:
                async with streamable_http_client(self.url, http_client=client) as (read, write, _session_id):
                    async with ClientSession(read, write) as session:
                        await session.initialize()
                        if name not in await _tool_names(session):
                            raise MCPError("missing_tool")
                        result = await session.call_tool(
                            name, args, read_timeout_seconds=timedelta(seconds=timeout),
                        )
        except MCPError:
            raise
        except Exception as exc:
            raise MCPError(_kind(exc)) from None
        return _text(result, self.max_bytes)


class MCPSources:
    def __init__(self, prometheus: HTTPSource | None, loki: HTTPSource | None):
        self.prometheus = prometheus
        self.loki = loki

    def call(self, name: str, args: dict | None) -> str:
        source = self.prometheus if name in PROMETHEUS_TOOLS else self.loki
        if source is None:
            raise MCPError("malformed")
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


def scope_hint(settings, service_id: str | None) -> str:
    prom = selector_for(getattr(settings, "prometheus_service_selectors", ""), service_id or "")
    loki = selector_for(getattr(settings, "loki_service_selectors", ""), service_id or "")
    parts = []
    if prom:
        parts.append(f"Prometheus matcher for this service: {prom}.")
    if loki:
        parts.append(f"Loki selector for this service: {loki}.")
    window = int(getattr(settings, "mcp_max_window_seconds", 3600) or 3600)
    if not parts:
        return f"No service selector is configured. Do not expect Prometheus or Loki evidence. Window at most {window}s."
    return " ".join(parts) + f" Window at most {window}s."


def selector_for(raw: str, service_id: str) -> str | None:
    text = str(raw or "").strip()
    if not text or not service_id:
        return None
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        return None
    if not isinstance(parsed, dict):
        return None
    value = parsed.get(service_id)
    if not isinstance(value, str) or not value.strip():
        return None
    return value.strip()


def scope_promql(query: str, matcher: str) -> str | None:
    text = (query or "").strip()
    if not text or not matcher or _REJECT_PROMQL.search(text) or text.count("{") > 1:
        return None
    if text.count("{") != text.count("}"):
        return None
    metrics = []
    for match in _IDENT.finditer(text):
        if text[match.end():].lstrip().startswith("("):
            continue
        metrics.append(match)
    if len(metrics) != 1:
        return None
    if "{" in text:
        open_at = text.index("{")
        close_at = text.index("}")
        inner = text[open_at + 1:close_at].strip()
        if matcher in inner:
            return text
        merged = f"{inner},{matcher}" if inner else matcher
        return text[:open_at] + "{" + merged + "}" + text[close_at + 1:]
    metric = metrics[0]
    return text[:metric.end()] + "{" + matcher + "}" + text[metric.end():]


def scope_logql(query: str, selector: str) -> str | None:
    selector = (selector or "").strip()
    if not (selector.startswith("{") and selector.endswith("}")) or not _MATCHER.search(selector):
        return None
    text = (query or "").strip()
    if not text:
        return selector
    if "{" not in text:
        return f"{selector} {text}"
    if not text.startswith("{") or text.count("{") != 1 or "}" not in text:
        return None
    close_at = text.index("}")
    inner = text[1:close_at]
    merged = _merge_matchers(inner, selector[1:-1])
    if not merged:
        return None
    return "{" + merged + "}" + text[close_at + 1:]


def clamp_window(occurred: int, start: int | None, end: int | None, max_window: int) -> tuple[int, int]:
    end_at = occurred if end is None else int(end)
    if end_at > occurred:
        end_at = occurred
    earliest = end_at - max(int(max_window), 0)
    start_at = earliest if start is None else int(start)
    if start_at < earliest:
        start_at = earliest
    if start_at > end_at:
        start_at = earliest
    return start_at, end_at


def summarize_result(text: str, source: str, max_rows: int) -> dict:
    """抽出可引用的短字段。超限的样本或日志行不进入返回值。"""
    stripped = (text or "").strip()
    if stripped.lower() in {"", "[]", "{}", "null", "none", "no data"}:
        raise MCPError("empty")
    parsed = _json_or_none(stripped)
    rows, value = _rows_and_value(parsed, stripped)
    if not rows and value is None:
        raise MCPError("empty")
    kept = rows[: max(int(max_rows), 0)] if rows else []
    if rows and not kept:
        raise MCPError("empty")
    short = value if value is not None else _short_value(kept[0] if kept else stripped)
    if short is None:
        raise MCPError("empty")
    count = len(kept) if rows else 1
    payload = {"value": short}
    if source == "loki":
        payload["lines"] = count
    else:
        payload["samples"] = count
    if rows and len(rows) > len(kept):
        payload["truncated"] = True
    return payload


def _source(url: str, token: str, settings) -> HTTPSource | None:
    url = str(url or "").strip()
    if not url:
        return None
    timeout = float(getattr(settings, "mcp_timeout_seconds", 10) or 10)
    max_bytes = int(getattr(settings, "mcp_max_response_bytes", 65536) or 65536)
    return HTTPSource(url, str(token or ""), timeout, max_bytes)


async def _tool_names(session) -> set[str]:
    names: set[str] = set()
    cursor = None
    for _ in range(5):
        listed = await session.list_tools(cursor) if cursor else await session.list_tools()
        names.update(tool.name for tool in listed.tools or [])
        cursor = getattr(listed, "nextCursor", None)
        if not cursor:
            break
    return names


def _text(result, max_bytes: int) -> str:
    if getattr(result, "isError", False):
        message = _collect(result, max_bytes).lower()
        if any(mark in message for mark in ("429", "rate limit", "too many requests")):
            raise MCPError("rate_limit")
        if any(mark in message for mark in ("unauthorized", "forbidden")):
            raise MCPError("auth")
        raise MCPError("malformed")
    text = _collect(result, max_bytes)
    if _is_empty_payload(text):
        raise MCPError("empty")
    return text


def _is_empty_payload(text: str) -> bool:
    stripped = (text or "").strip()
    if stripped.lower() in {"", "[]", "{}", "null", "none", "no data"}:
        return True
    parsed = _json_or_none(stripped)
    if isinstance(parsed, dict):
        for key in ("result", "streams", "values", "logs"):
            if key in parsed and parsed[key] in ([], {}, None, ""):
                return True
        data = parsed.get("data")
        if isinstance(data, dict) and data.get("result") in ([], {}, None, ""):
            return True
        if data in ([], {}, None, ""):
            return True
    return False


def _collect(result, max_bytes: int) -> str:
    used = 0
    parts: list[str] = []
    saw_non_text = False
    for block in getattr(result, "content", None) or []:
        text = getattr(block, "text", None)
        if not isinstance(text, str):
            if block is not None:
                saw_non_text = True
            continue
        encoded = text.encode()
        if used + len(encoded) > max_bytes:
            raise MCPError("too_large")
        parts.append(text)
        used += len(encoded)
    if not parts and saw_non_text:
        raise MCPError("malformed")
    return "".join(parts)


def _kind(exc: BaseException) -> str:
    for current in _chain(exc):
        if isinstance(current, MCPError):
            return current.kind
    for current in _chain(exc):
        if isinstance(current, (TimeoutError, asyncio.TimeoutError, httpx.TimeoutException)):
            return "timeout"
        status = getattr(getattr(current, "response", None), "status_code", None)
        if status == 429:
            return "rate_limit"
        if status in (401, 403):
            return "auth"
        if isinstance(current, (httpx.NetworkError, httpx.RemoteProtocolError, ConnectionError, ConnectionResetError)):
            return "disconnect"
        text = str(current).lower()
        if any(mark in text for mark in ("too many requests", "rate limit", "429")):
            return "rate_limit"
        if any(mark in text for mark in ("unauthorized", "forbidden", "401", "403")):
            return "auth"
        if "timeout" in text or "timed out" in text:
            return "timeout"
        if any(mark in text for mark in ("connection refused", "connection reset", "disconnected", "connect error")):
            return "disconnect"
    return "malformed"


def _chain(exc: BaseException | None):
    stack = [exc]
    seen: set[int] = set()
    while stack:
        current = stack.pop()
        if current is None or id(current) in seen:
            continue
        seen.add(id(current))
        yield current
        stack.append(current.__cause__)
        stack.append(current.__context__)
        nested = getattr(current, "exceptions", None)
        if nested:
            stack.extend(nested)


def _merge_matchers(inner: str, mapped: str) -> str | None:
    found: dict[str, str] = {}
    order: list[str] = []
    for source in (inner, mapped):
        matches = list(_MATCHER.finditer(source))
        if source.strip() and not matches:
            return None
        for match in matches:
            key = match.group(1)
            piece = f'{key}{match.group(2)}"{match.group(3)}"'
            if key not in found:
                order.append(key)
            found[key] = piece
    if not found:
        return None
    return ",".join(found[key] for key in order)


def _json_or_none(text: str):
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        return None


def _rows_and_value(parsed, text: str) -> tuple[list, str | None]:
    if parsed is None:
        return [text], _clip_value(text)
    if parsed in ([], {}, None, ""):
        return [], None
    if isinstance(parsed, list):
        return parsed, _short_value(parsed[0]) if parsed else None
    if isinstance(parsed, dict):
        for key in ("result", "streams", "values", "logs", "data"):
            if key not in parsed:
                continue
            value = parsed[key]
            if isinstance(value, dict) and "result" in value:
                value = value["result"]
            if isinstance(value, list):
                return value, _short_value(value[0]) if value else None
            if value in ([], {}, None, ""):
                return [], None
        return [parsed], _short_value(parsed)
    return [parsed], _clip_value(parsed)


def _short_value(item) -> str | None:
    if isinstance(item, dict):
        if "value" in item:
            return _sample_value(item["value"])
        if "line" in item:
            return _clip_value(item["line"])
        if "values" in item and isinstance(item["values"], list) and item["values"]:
            return _sample_value(item["values"][0])
        for key in ("metric", "stream"):
            if key in item:
                continue
        return _clip_value(next((value for value in item.values() if not isinstance(value, (dict, list))), None))
    if isinstance(item, (list, tuple)) and item:
        return _sample_value(item[-1] if len(item) > 1 else item[0])
    return _clip_value(item)


def _sample_value(value) -> str | None:
    if isinstance(value, (list, tuple)) and value:
        return _clip_value(value[-1])
    return _clip_value(value)


def _clip_value(value) -> str | None:
    if value is None or isinstance(value, (dict, list)):
        return None
    text = str(value).strip()
    if not text:
        return None
    return text[:200]
