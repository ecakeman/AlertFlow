"""Context Manager：唯一的检索入口（进程内逻辑 Manager，规格 §7.7.2/§7.7.5）。

边界是刻意的：
- 它**不调 LLM、不做决策**，只把 ``RetrievalRequest`` 翻译成各来源的只读查询；
- 时间窗只来自 ``request.scope.window``——adapter 不得自己再推导（四种旧算法已收口到
  ``runtime/window.py``）；
- 一个来源失败不取消其他来源；失败/空/不可用/超时/超限分别映射到
  ``RetrievalResult.status`` 与 ``CoverageGap.code``，不折叠成一句"没数据"；
- 返回 ``RetrievalBatch``，**不生成 evidence_id**——登记由 EvidenceManager 负责。
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging

from ..schemas.context import (
    AlertFlowQueryRequest,
    BudgetUsage,
    CoverageGap,
    InvestigationScope,
    LokiQueryRequest,
    PrometheusQueryRequest,
    RetrievalBatch,
    RetrievalRequest,
    RetrievalResult,
    RetrievedItem,
    SourceError,
    TimeRange,
    TruncationInfo,
    utc_now,
)
from ..schemas.memory import TimelineRecallRequest
from .window import parse_time

log = logging.getLogger("alertflow_agent")

# AlertFlow 资源 -> (source_type, record_type)
_RESOURCE_MAP = {
    "checks": ("checks", "check"),
    "incident": ("diagnostic", "timeline_event"),
    "diagnostics": ("diagnostic", "check"),
    "service_timeline": ("timeline", "timeline_event"),
    "kubernetes_state": ("kubernetes", "kubernetes_object_state"),
    "aggregated_metrics": ("go_metric", "prometheus_sample"),
}
# MCPError.kind -> SourceError.category（前者用于 gap 码，后者受 schema 约束）
_MCP_CATEGORY = {
    "timeout": "timeout",
    "disconnect": "transport",
    "auth": "auth",
    "rate_limit": "budget",
    "budget": "budget",
    "missing_tool": "malformed",
    "too_large": "budget",
    "empty": "malformed",
    "malformed": "malformed",
    "unmapped": "config",
    "unscoped": "config",
}
_TIMELINE_SOURCES = ["application_diagnostics", "kubernetes", "incident", "agent"]


def _digest(value) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, default=str).encode()).hexdigest()[:16]


def _iso(value) -> str | None:
    return value.isoformat().replace("+00:00", "Z") if value is not None else None


def _matcher(selector: dict[str, str]) -> str:
    return "{" + ",".join(f'{k}="{v}"' for k, v in sorted(selector.items())) + "}"


class ContextManager:
    """四适配器：AlertFlow gRPC / Prometheus MCP / Loki MCP / Memory timeline。"""

    def __init__(self, *, alertflow=None, mcp=None, memory=None, settings=None) -> None:
        self.alertflow = alertflow
        self.mcp = mcp
        self.memory = memory
        self.settings = settings

    # ---- Protocol：async retrieve ----
    async def retrieve(self, request: RetrievalRequest) -> RetrievalBatch:
        batch = RetrievalBatch(batch_id=f"rb:{request.request_id}", run_id=request.scope.run_id,
                               scope=request.scope, started_at=utc_now())
        results: list[RetrievalResult] = []
        for source in request.source_requests:
            try:
                if isinstance(source, AlertFlowQueryRequest):
                    results.extend(await self._alertflow(request, source))
                elif isinstance(source, PrometheusQueryRequest):
                    results.append(await self._prometheus(request, source))
                elif isinstance(source, LokiQueryRequest):
                    results.append(await self._loki(request, source))
                elif isinstance(source, TimelineRecallRequest):
                    results.append(await asyncio.to_thread(self._memory_recall, request, source))
                else:  # pragma: no cover - 联合类型已由 schema 保证
                    results.append(self._failed(request, "checks", "alertflow_grpc",
                                                "INVALID_RESPONSE", "unsupported source request"))
            except Exception as exc:  # 单个来源失败不得取消其他来源
                log.warning("retrieval source failed: %s", exc)
                source_type, source_system = self._source_of(source)
                results.append(self._failed(request, source_type, source_system, "unknown", str(exc)))
        # 与 RetrievalBatch 的固定排序一致（schema 校验器只在构造时跑一次）。
        order = ["checks", "diagnostic", "timeline", "kubernetes", "go_metric",
                 "prometheus", "loki", "memory_reference", "runbook"]
        batch.results = sorted(results, key=lambda item: order.index(item.source_type))
        batch.completed_at = utc_now()
        batch.overall_coverage = self._coverage(results)
        batch.budget_usage = BudgetUsage(used_calls=len(results), used_bytes=0)
        return batch

    def retrieve_now(self, request: RetrievalRequest) -> RetrievalBatch:
        """同步包装：工具层是同步执行体，不另造第二套协议。"""
        return asyncio.run(self.retrieve(request))

    # ---- 结果构造 ----
    def _result(self, request, source_type, source_system, *, status="ok", items=None,
                gaps=None, error=None, truncation=None, observed_range=None, metadata=None):
        return RetrievalResult(
            request_id=self._request_id(request), run_id=request.scope.run_id,
            service_id=self._service_id(request), source_type=source_type,
            source_system=source_system, status=status, items=items or [],
            coverage_gaps=gaps or [], error=error, truncation=truncation or TruncationInfo(),
            observed_range=observed_range, query_metadata=metadata or {},
        )

    def _failed(self, request, source_type, source_system, category, message):
        return self._result(request, source_type, source_system, status="failed",
                            error=SourceError(category=category, message=message, retryable=False),
                            gaps=[CoverageGap(code="SOURCE_UNAVAILABLE", source_type=source_type,
                                              detail=message[:300])])

    def _disabled(self, request, source_type, source_system, message):
        return self._result(request, source_type, source_system, status="disabled",
                            gaps=[CoverageGap(code="SOURCE_DISABLED", source_type=source_type,
                                              detail=message)])

    @staticmethod
    def _source_of(source):
        if isinstance(source, PrometheusQueryRequest):
            return "prometheus", "prometheus_mcp"
        if isinstance(source, LokiQueryRequest):
            return "loki", "loki_mcp"
        if isinstance(source, TimelineRecallRequest):
            return "memory_reference", "agent_memory"
        return "checks", "alertflow_grpc"

    @staticmethod
    def _request_id(request) -> str:
        return request.request_id

    @staticmethod
    def _service_id(request) -> str:
        return request.scope.service_id

    # ---- AlertFlow gRPC ----
    async def _alertflow(self, request, source: AlertFlowQueryRequest) -> list[RetrievalResult]:
        return [await asyncio.to_thread(self._alertflow_resource, request, source, resource)
                for resource in source.resources]

    def _alertflow_resource(self, request, source, resource) -> RetrievalResult:
        source_type, record_type = _RESOURCE_MAP.get(resource, ("checks", "check"))
        if self.alertflow is None:
            return self._disabled(request, source_type, "alertflow_grpc", "alertflow client is not configured")
        start, end = source.window_start, source.window_end
        meta = {"resource": resource, "window_start": _iso(start), "window_end": _iso(end)}
        try:
            from ..integrations.alertflow import collect_checks, normalize_check

            if resource == "checks":
                rows = collect_checks(self.alertflow, source.service_id, int(start.timestamp()),
                                      int(end.timestamp()), source.page_size)
                items = [RetrievedItem(
                    source_record_id=str(row.get("id")), record_type="check", service_id=source.service_id,
                    occurred_at=parse_time(row.get("ts")), observed_at=parse_time(row.get("ts")),
                    summary=str(row.get("error") or row.get("status") or ""),
                    data=normalize_check(row), source_locator={"table": "checks", "id": row.get("id")},
                    content_digest=_digest(row),
                ) for row in rows]
            elif resource == "incident":
                if source.incident_id is None:
                    return self._result(request, source_type, "alertflow_grpc", status="invalid_request",
                                        gaps=[CoverageGap(code="SELECTOR_MISSING", source_type=source_type,
                                                          detail="incident_id is required for incident resource")])
                detail = self.alertflow.incident(int(source.incident_id))
                items = [RetrievedItem(
                    source_record_id=str(source.incident_id), record_type=record_type,
                    service_id=source.service_id, occurred_at=parse_time(detail.get("startedAt")),
                    observed_at=parse_time(detail.get("startedAt")),
                    recorded_at=parse_time(detail.get("updatedAt") or detail.get("createdAt")),
                    summary=str(detail.get("title") or ""), data=detail,
                    source_locator={"table": "incidents", "id": source.incident_id},
                    content_digest=_digest(detail),
                )]
            elif resource == "service_timeline":
                page = self.alertflow.timeline(source.service_id, int(start.timestamp()),
                                               int(end.timestamp()), _TIMELINE_SOURCES, source.page_size)
                items = [RetrievedItem(
                    source_record_id=str(event.get("eventKey") or event.get("id")),
                    record_type="timeline_event", service_id=source.service_id,
                    occurred_at=parse_time(event.get("occurredAt")),
                    observed_at=parse_time(event.get("observedAt")),
                    recorded_at=parse_time(event.get("recordedAt")),
                    summary=str(event.get("title") or event.get("summary") or ""),
                    data=event, source_locator={"table": "timeline", "id": event.get("id")},
                    content_digest=_digest(event),
                ) for event in (page.get("events") or [])]
                if page.get("coverage"):
                    meta["coverage"] = page["coverage"]
            elif resource == "kubernetes_state":
                objects = self.alertflow.kubernetes_state(source.service_id).get("objects") or []
                items = [RetrievedItem(
                    source_record_id=str(obj.get("uid") or obj.get("name")),
                    record_type="kubernetes_object_state", service_id=source.service_id,
                    occurred_at=parse_time(obj.get("lastSeenAt")),
                    observed_at=parse_time(obj.get("lastSeenAt")),
                    summary=str(obj.get("summary") or f"{obj.get('kind')}/{obj.get('name')}"),
                    data=obj, source_locator={"cluster_id": obj.get("clusterId"),
                                              "namespace": obj.get("namespace"), "kind": obj.get("kind"),
                                              "name": obj.get("name")},
                    content_digest=_digest(obj),
                ) for obj in objects]
            elif resource == "aggregated_metrics":
                series = self.alertflow.metrics(source.service_id, range_="24h").get("series") or []
                items = [RetrievedItem(
                    source_record_id=str(point.get("ts")), record_type=record_type,
                    service_id=source.service_id, occurred_at=parse_time(point.get("ts")),
                    observed_at=parse_time(point.get("ts")), summary=f"avg_latency={point.get('avg_latency')}",
                    data=point, source_locator={"table": "metrics"}, content_digest=_digest(point),
                ) for point in series[: source.page_size]]
            else:  # diagnostics 随触发上下文送达，不单列 gRPC 查询
                return self._result(request, source_type, "alertflow_grpc", status="unavailable",
                                    gaps=[CoverageGap(code="SOURCE_UNAVAILABLE", source_type=source_type,
                                                      detail="diagnostics arrive with the triggering context")],
                                    metadata=meta)
        except Exception as exc:
            return self._failed(request, source_type, "alertflow_grpc", "transport", str(exc))

        truncation = TruncationInfo(truncated=len(items) > source.page_size,
                                    limit=source.page_size, returned=len(items))
        status = "truncated" if truncation.truncated else ("ok" if items else "empty")
        gaps = [] if items else [CoverageGap(code="NO_MATCHING_DATA", source_type=source_type,
                                             detail=f"no {resource} in window")]
        return self._result(request, source_type, "alertflow_grpc", status=status, items=items,
                            gaps=gaps, truncation=truncation,
                            observed_range=TimeRange(start=start, end=end), metadata=meta)

    async def _mcp_passthrough(self, request, source_type, source_system, tool, args) -> RetrievalResult:
        """MCP 原生工具调用：工具层整形，ContextManager 统一调用/超时/错误语义。"""
        try:
            text = await asyncio.to_thread(self.mcp.call, tool, dict(args or {}))
        except TimeoutError:
            return self._result(request, source_type, source_system, status="timeout",
                                error=SourceError(category="timeout", message=f"{source_type} query timed out", retryable=True),
                                gaps=[CoverageGap(code="QUERY_TIMEOUT", source_type=source_type,
                                                  detail=f"{source_type} query timed out")])
        except Exception as exc:  # MCPError.kind 原样放进 metadata，供工具层映射 gap 码
            kind = str(getattr(exc, "kind", "malformed") or "malformed")
            category = _MCP_CATEGORY.get(kind, "unknown")
            return self._result(request, source_type, source_system, status="failed",
                                error=SourceError(category=category, message=str(exc)),
                                metadata={"tool": tool, "mcp_kind": kind})
        if not isinstance(text, str):
            return self._result(request, source_type, source_system, status="failed",
                                error=SourceError(category="malformed", message="mcp returned non-text result"),
                                metadata={"tool": tool, "mcp_kind": "malformed"})
        return self._result(request, source_type, source_system, status="ok",
                            metadata={"tool": tool, "sent": dict(args or {}), "text": text})

    # ---- Prometheus MCP ----
    async def _prometheus(self, request, source: PrometheusQueryRequest) -> RetrievalResult:
        if self.mcp is None:
            return self._disabled(request, "prometheus", "prometheus_mcp", "prometheus mcp is not configured")
        # 工具层已把参数整形为 MCP 原生调用；这里只负责调用与状态映射，
        # 保证 Prometheus 只经唯一检索入口访问。
        if source.tool:
            return await self._mcp_passthrough(request, "prometheus", "prometheus_mcp", source.tool, source.tool_args)
        if source.query_mode == "instant":
            tool, sent = "query", {"query": source.promql, "timestamp": _iso(source.end)}
        else:
            step = source.step_seconds
            tool = "range_query"
            sent = {"query": source.promql, "start_time": _iso(source.start),
                    "end_time": _iso(source.end), "step": step}
        try:
            text = await asyncio.to_thread(self.mcp.call, tool, sent)
            payload = json.loads(text) if isinstance(text, str) else text
        except TimeoutError:
            return self._result(request, "prometheus", "prometheus_mcp", status="timeout",
                                error=SourceError(category="timeout", message="prometheus query timed out", retryable=True),
                                gaps=[CoverageGap(code="QUERY_TIMEOUT", source_type="prometheus",
                                                  detail="prometheus query timed out")])
        except Exception as exc:
            return self._failed(request, "prometheus", "prometheus_mcp", "transport", str(exc))
        rows = payload if isinstance(payload, list) else (payload or {}).get("data", {}).get("result", [])
        items = [RetrievedItem(
            source_record_id=None, record_type="prometheus_sample", service_id=source.service_id,
            occurred_at=parse_time(row.get("value", [None])[0]) if isinstance(row, dict) and isinstance(row.get("value"), list) else None,
            summary=json.dumps(row, ensure_ascii=False)[:200], data=row if isinstance(row, dict) else {"value": row},
            source_locator={"selector": source.selector, "query_mode": source.query_mode},
            content_digest=_digest(row),
        ) for row in (rows or [])[: source.max_samples]]
        status = "ok" if items else "empty"
        truncation = TruncationInfo(truncated=len(rows or []) > source.max_samples,
                                    limit=source.max_samples, returned=len(items))
        return self._result(request, "prometheus", "prometheus_mcp", status=status, items=items,
                            truncation=truncation, observed_range=TimeRange(start=source.start, end=source.end),
                            metadata={"tool": tool, "selector": source.selector},
                            gaps=[] if items else [CoverageGap(code="NO_MATCHING_DATA", source_type="prometheus",
                                                              detail="no samples in window")])

    # ---- Loki MCP ----
    async def _loki(self, request, source: LokiQueryRequest) -> RetrievalResult:
        if self.mcp is None:
            return self._disabled(request, "loki", "loki_mcp", "loki mcp is not configured")
        if source.tool:
            return await self._mcp_passthrough(request, "loki", "loki_mcp", source.tool, source.tool_args)
        limit = source.limit
        if self.settings is not None:
            limit = min(limit, self.settings.mcp_max_log_lines)
        sent = {"query": source.logql, "start": _iso(source.start), "end": _iso(source.end),
                "limit": limit, "direction": source.direction}
        try:
            payload = await asyncio.to_thread(self.mcp.call, "loki_query", sent)
            if isinstance(payload, str):
                payload = json.loads(payload)
            rows = payload if isinstance(payload, list) else (payload or {}).get("lines") or (payload or {}).get("data") or []
        except TimeoutError:
            return self._result(request, "loki", "loki_mcp", status="timeout",
                                error=SourceError(category="timeout", message="loki query timed out", retryable=True),
                                gaps=[CoverageGap(code="QUERY_TIMEOUT", source_type="loki", detail="loki query timed out")])
        except Exception as exc:
            return self._failed(request, "loki", "loki_mcp", "transport", str(exc))
        items = []
        for row in list(rows or [])[:limit]:
            data = row if isinstance(row, dict) else {"line": row}
            stamp = data.get("timestamp") or data.get("time")
            items.append(RetrievedItem(
                source_record_id=str(data.get("id") or ""), record_type="loki_log_line",
                service_id=source.service_id, occurred_at=parse_time(stamp),
                observed_at=parse_time(stamp),
                summary=str(data.get("line") or "")[:400], data=data,
                source_locator={"labels": data.get("labels") or {}, "pod": data.get("pod")},
                content_digest=_digest(data),
            ))
        truncation = TruncationInfo(truncated=len(rows or []) > limit, limit=limit, returned=len(items))
        return self._result(request, "loki", "loki_mcp", status="ok" if items else "empty", items=items,
                            truncation=truncation, observed_range=TimeRange(start=source.start, end=source.end),
                            metadata={"selector": source.selector, "logql": source.logql},
                            gaps=[] if items else [CoverageGap(code="NO_MATCHING_DATA", source_type="loki",
                                                              detail="no log lines in window")])

    # ---- Memory timeline ----
    def _memory_recall(self, request, source: TimelineRecallRequest) -> RetrievalResult:
        if self.memory is None:
            return self._disabled(request, "memory_reference", "agent_memory", "memory is not configured")
        try:
            nodes = self.memory.timeline_nodes(source.service_id, 365, source.top_k) \
                if hasattr(self.memory, "timeline_nodes") else []
            attachments = self.memory.list_attachments(source.service_id, limit=source.top_k) \
                if hasattr(self.memory, "list_attachments") else []
        except Exception as exc:
            return self._failed(request, "memory_reference", "agent_memory", "unknown", str(exc))
        items = []
        for node in nodes:
            data = node.model_dump(mode="json") if hasattr(node, "model_dump") else dict(node)
            items.append(RetrievedItem(
                source_record_id=data.get("node_id"), record_type="memory_attachment",
                service_id=source.service_id, occurred_at=parse_time(data.get("occurred_at")),
                observed_at=parse_time(data.get("observed_at")),
                recorded_at=parse_time(data.get("recorded_at")),
                summary=str(data.get("summary") or "")[:400], data=data,
                source_locator=data.get("source_locator") or {}, content_digest=_digest(data),
            ))
        if source.include_attachments:
            for item in attachments:
                items.append(RetrievedItem(
                    source_record_id=str(item.get("attachment_id")), record_type="memory_attachment",
                    service_id=source.service_id, occurred_at=parse_time(item.get("occurred_at")),
                    recorded_at=parse_time(item.get("recorded_at")),
                    summary=str(item.get("summary") or "")[:400], data=item,
                    source_locator={"anchor_type": item.get("anchor_type"),
                                    "anchor_id": item.get("anchor_id")},
                    content_digest=str(item.get("content_digest") or _digest(item)),
                ))
        start = parse_time(source.time_start) or request.scope.window.window_start
        end = parse_time(source.time_end) or request.scope.window.window_end
        return self._result(request, "memory_reference", "agent_memory",
                            status="ok" if items else "empty", items=items[: source.top_k],
                            observed_range=TimeRange(start=start, end=end),
                            metadata={"time_order": "desc", "semantic_rank_used": False},
                            gaps=[] if items else [CoverageGap(code="NO_MATCHING_DATA",
                                                              source_type="memory_reference",
                                                              detail="no memory in window")])

    @staticmethod
    def _coverage(results) -> str:
        if not results:
            return "gap"
        bad = {"failed", "unavailable", "timeout", "invalid_request", "disabled"}
        if any(result.status in bad for result in results):
            return "gap"
        if any(result.status in {"empty", "truncated"} for result in results):
            return "partial"
        return "complete"


__all__ = ["ContextManager"]
