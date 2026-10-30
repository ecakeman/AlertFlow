"""EvidenceManager（C2 / 规格 §7.7.6）：批量登记、稳定 evidence_id、Fact Catalog。

与旧实现的区别：
- 旧 ``graph._add`` 直接把 dict 写进 ``state["evidence"]``，id 由调用方拼字符串；
  这里由父级统一从候选（candidate）生成 ``evidence_id``，**不接受模型自报**；
- 六个维度逐候选校验（service / incident / window / selector / rows / bytes / schema），
  非法候选只拒绝自己，同批其他合法候选照常入库；
- Fact Catalog 从**已登记**记录派生，fact 只允许标量；
- ``verify_report`` 只做确定性校验，``passed`` 不代表因果被证明。
"""

from __future__ import annotations

import hashlib
import json
from datetime import datetime
from typing import Iterable, Optional

from alertflow_agent.runtime.verify import _scalars, build_catalog, verify as _verify_report
from alertflow_agent.schemas.context import (
    CoverageGap,
    InvestigationScope,
    InvestigationWindow,
    SourceSystem,
    SourceType,
    TrustLevel,
)
from alertflow_agent.schemas.evidence import (
    EvidenceCandidate,
    EvidenceRecord,
    EvidenceRegistrationResult,
    FactCatalogEntry,
    RejectedCandidate,
    VerificationResult,
)

_MAX_FACTS = 100

# 旧证据 source_type -> (新 SourceType, SourceSystem)
# 旧 registry 的 key 仍保留为 source_locator["ledger_key"]，保证向前兼容。
SOURCE_MAP: dict[str, tuple[SourceType, SourceSystem]] = {
    "alertflow_incident": ("checks", "alertflow_grpc"),
    "alertflow_check": ("checks", "alertflow_grpc"),
    "alertflow_comment": ("diagnostic", "alertflow_grpc"),
    "alertflow_metric": ("go_metric", "alertflow_grpc"),
    "alertflow_service": ("diagnostic", "alertflow_grpc"),
    "alertflow_tls": ("diagnostic", "alertflow_grpc"),
    "analysis": ("diagnostic", "alertflow_grpc"),
    "application_diagnostics": ("timeline", "alertflow_grpc"),
    # tl:coverage 这类“来源覆盖状态”事件本身就是时间线事件，不应被当成未知来源拒绝
    "timeline": ("timeline", "alertflow_grpc"),
    "incident": ("timeline", "alertflow_grpc"),
    "agent": ("timeline", "alertflow_grpc"),
    "kubernetes": ("kubernetes", "alertflow_grpc"),
    "prometheus": ("prometheus", "prometheus_mcp"),
    "loki": ("loki", "loki_mcp"),
    "runbook": ("runbook", "ragflow_a2a"),
    "project_diagnostic": ("diagnostic", "alertflow_grpc"),
    "episodic_memory": ("memory_reference", "agent_memory"),
    "subagent_review": ("diagnostic", "alertflow_grpc"),
}

_TRUST: dict[str, TrustLevel] = {
    "runbook": "expert_advice",
    "memory_reference": "historical_experience",
    "subagent_review": "model_state",
}

_FACT_PRIORITY: dict[str, int] = {
    "diagnostic": 0,
    "runbook": 1,
    "checks": 2,
    "go_metric": 3,
    "prometheus": 4,
    "loki": 4,
    "timeline": 5,
    "kubernetes": 5,
    "memory_reference": 6,
}

# 行数限制按来源类型选择 payload 里的列表字段
_ROW_KEYS = ("rows", "lines", "samples", "events", "objects", "checks")


class _Reject(Exception):
    def __init__(self, code: str, summary: str = ""):
        super().__init__(code)
        self.code = code
        self.summary = summary


def canonical_digest(payload) -> str:
    text = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str, ensure_ascii=False)
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def make_evidence_id(
    run_id: str,
    source_type: str,
    source_locator: dict,
    window: InvestigationWindow,
    content_digest: str,
) -> str:
    """run-scoped 稳定 ID：来源定位 + 窗口 + payload 摘要。"""
    material = {
        "run_id": run_id,
        "source_type": source_type,
        "source_locator": source_locator,
        "window_start": window.start_epoch(),
        "window_end": window.end_epoch(),
        "window_kind": window.window_kind,
        "content_digest": content_digest,
    }
    return "ev:" + canonical_digest(material)[:16]


class EvidenceManager:
    """进程内逻辑 Manager：不检索、不调 LLM，只做登记与派生。"""

    def __init__(
        self,
        run_id: str,
        scope: InvestigationScope,
        *,
        max_rows: int = 500,
        max_bytes: int = 65536,
        max_facts: int = _MAX_FACTS,
    ):
        self.run_id = run_id
        self.scope = scope
        self.max_rows = int(max_rows)
        self.max_bytes = int(max_bytes)
        self.max_facts = int(max_facts)
        self.records: dict[str, EvidenceRecord] = {}
        self._order: list[str] = []
        self.facts: list[FactCatalogEntry] = []
        self.last_result: Optional[EvidenceRegistrationResult] = None
        self.last_draft: Optional[dict] = None

    # -- 登记 -------------------------------------------------------------
    def register_batch(self, candidates: Iterable[EvidenceCandidate]) -> EvidenceRegistrationResult:
        result = EvidenceRegistrationResult(run_id=self.run_id)
        for candidate in candidates:
            try:
                record = self._validate(candidate)
            except _Reject as exc:
                result.rejected_candidates.append(
                    RejectedCandidate(
                        candidate_id=getattr(candidate, "candidate_id", ""),
                        reason_code=exc.code,  # type: ignore[arg-type]
                        summary=exc.summary or exc.code,
                    )
                )
                continue
            evidence_id = make_evidence_id(
                self.run_id,
                record.source_type,
                record.source_locator,
                record.window,
                record.content_digest or canonical_digest(record.payload),
            )
            if evidence_id in self.records:
                result.duplicate_evidence_ids.append(evidence_id)
                continue
            record = EvidenceRecord(**record.model_dump(exclude={"evidence_id"}), evidence_id=evidence_id)
            self.records[evidence_id] = record
            self._order.append(evidence_id)
            result.accepted_evidence_ids.append(evidence_id)

        rows: list[dict] = []
        counts: dict[str, int] = {}
        for evidence_id in self._order:
            record = self.records[evidence_id]
            for path, value in _fact_rows(record):
                rows.append({"evidence_id": evidence_id, "path": path, "value": value,
                             "source_type": record.source_type, "record": record})
            counts[record.source_type] = counts.get(record.source_type, 0) + 1
        rows.sort(key=lambda row: (
            _FACT_PRIORITY.get(row["source_type"], 9), row["evidence_id"], row["path"],
        ))
        # 截断按来源配额：先保证每个来源都有保底名额，剩余名额再按来源优先级补，
        # 避免“先到先得”把后出现的来源整类挤掉（例如 loki/prometheus 永远是 0）。
        kept_rows, kept_by_source, dropped_by_source = _select_with_quota(rows, self.max_facts)
        self.facts = [_entry(f"f{index}", row) for index, row in enumerate(kept_rows, start=1)]
        truncated = bool(dropped_by_source)
        result.fact_count_by_source_type = counts
        result.facts_kept_by_source_type = kept_by_source
        result.catalog_truncated_by_source = dropped_by_source
        result.catalog_truncated = truncated
        if truncated:
            result.coverage_gaps.append(
                CoverageGap(code="RESULT_TRUNCATED",
                            detail="facts>" + str(self.max_facts) + " by source " + json.dumps(dropped_by_source))
            )
        self.last_result = result
        return result

    def _validate(self, candidate: EvidenceCandidate) -> EvidenceRecord:
        # 1) service
        if candidate.service_id != self.scope.service_id:
            raise _Reject("SERVICE_MISMATCH", "candidate service differs from scope")
        # 2) incident（两边都有且不一致才拒绝）
        if (
            candidate.incident_id is not None
            and self.scope.incident_id is not None
            and candidate.incident_id != self.scope.incident_id
        ):
            raise _Reject("INCIDENT_MISMATCH", "candidate incident differs from scope")
        # 3) window：adapter 不得自造窗口
        if (
            candidate.window.start_epoch() != self.scope.window.start_epoch()
            or candidate.window.end_epoch() != self.scope.window.end_epoch()
            or candidate.window.window_kind != self.scope.window.window_kind
        ):
            raise _Reject("WINDOW_MISMATCH", "candidate window differs from scope")
        # 4) selector：Prometheus/Loki 必须带精确 selector
        if candidate.source_type in ("prometheus", "loki"):
            selector = candidate.source_locator.get("selector") or candidate.query_metadata.get("selector")
            if not selector:
                raise _Reject("SELECTOR_MISSING", "prometheus/loki candidate has no selector")
        # 5) rows
        for key in _ROW_KEYS:
            rows = candidate.payload.get(key)
            if isinstance(rows, list) and len(rows) > self.max_rows:
                raise _Reject("ROWS_EXCEEDED", f"{key} > {self.max_rows}")
        # 6) bytes
        size = len(json.dumps(candidate.payload, default=str, ensure_ascii=False).encode("utf-8"))
        if size > self.max_bytes:
            raise _Reject("BYTES_EXCEEDED", f"payload > {self.max_bytes} bytes")
        # 7) schema
        if not candidate.payload_schema:
            raise _Reject("SCHEMA_INVALID", "missing payload_schema")
        if not isinstance(candidate.payload, dict):
            raise _Reject("SCHEMA_INVALID", "payload must be an object")
        if not candidate.payload:
            raise _Reject("PAYLOAD_EMPTY", "payload is empty")
        return EvidenceRecord(**candidate.model_dump(), evidence_id="pending")

    # -- 派生 -------------------------------------------------------------
    def catalog(self) -> list[dict]:
        """旧 verify()/prompt 期望的形状：fact_id + evidence_id + path + value。

        兼容说明：``evidence_id`` 优先使用旧 registry 的 ledger_key，
        这样 prompt 里的事实引用与当前 evidence 块展示的 id 一致；
        规范化后的 run-scoped id 放在 ``registered_evidence_id``，由 registry() 对外提供。
        """
        rows = []
        for entry in self.facts:
            record = self.records[entry.evidence_id]
            ledger_key = record.source_locator.get("ledger_key")
            rows.append({
                "fact_id": entry.fact_id,
                "evidence_id": ledger_key or entry.evidence_id,
                "registered_evidence_id": entry.evidence_id,
                "path": entry.path,
                "value": entry.value,
            })
        return rows

    def registry(self) -> dict[str, dict]:
        """紧凑登记表（可进 checkpoint/report），不含 payload 全文。"""
        return {
            evidence_id: {
                "source_type": record.source_type,
                "source_system": record.source_system,
                "ledger_key": record.source_locator.get("ledger_key", ""),
                "content_digest": record.content_digest,
            }
            for evidence_id, record in self.records.items()
        }

    def verify_report(self, draft: dict, evidence: dict, tokens: Optional[list[str]] = None,
                      gaps: Optional[list[str]] = None) -> VerificationResult:
        catalog = self.catalog() or build_catalog(evidence or {})
        result = _verify_report(draft, evidence, tokens or [], gaps, catalog)
        self.last_draft = result.get("draft")
        findings = result.get("findings") or []
        return VerificationResult(
            passed=bool(result.get("passed")),
            schema_errors=[f.get("detail") or "schema" for f in findings if f.get("rule") == "schema"],
            unknown_evidence_ids=[f.get("detail") for f in findings if f.get("rule") == "evidence_ref"],
            invalid_fact_refs=[f.get("detail") for f in findings if f.get("rule") == "fact_ref"],
            hypothesis_reference_errors=[f.get("detail") for f in findings if f.get("rule") == "conclusion"],
            warnings=list(result.get("warnings") or []),
            invalid_document_citations=self.invalid_citations(),
        )

    def invalid_citations(self) -> list[str]:
        """Runbook 引用必须同时有 document_id 与 chunk_id，否则不可追溯。"""
        bad: list[str] = []
        for evidence_id, record in self.records.items():
            if record.source_type != "runbook":
                continue
            for index, citation in enumerate(record.payload.get("citations") or []):
                if not isinstance(citation, dict):
                    bad.append(f"{evidence_id}:{index}")
                    continue
                if not citation.get("document_id") or not citation.get("chunk_id"):
                    bad.append(f"{evidence_id}:{index}")
        return bad


def _entry(fact_id: str, row: dict) -> FactCatalogEntry:
    value = row["value"]
    if isinstance(value, bool):
        value_type = "boolean"
    elif isinstance(value, (int, float)):
        value_type = "number"
    elif isinstance(value, datetime):
        value_type = "datetime"
    elif value is None:
        value_type = "null"
    else:
        value_type = "string"
    record: EvidenceRecord = row["record"]
    return FactCatalogEntry(
        fact_id=fact_id,
        evidence_id=row["evidence_id"],
        path=row["path"],
        value=value,
        value_type=value_type,
        fact_kind=_fact_kind(record.source_type),
        occurred_at=None,
        observed_at=record.collected_at,
        recorded_at=record.collected_at,
        service_id=record.service_id,
        trust_level=record.trust_level,
    )


def _fact_kind(source_type: str) -> str:
    return {
        "checks": "state",
        "diagnostic": "diagnostic",
        "timeline": "transition",
        "kubernetes": "state",
        "go_metric": "metric",
        "prometheus": "metric",
        "loki": "log",
        "runbook": "expert_advice",
        "memory_reference": "metadata",
    }.get(source_type, "metadata")


def _fact_payload(record: EvidenceRecord) -> dict:
    """按来源类型归一化出可派生标量的 payload（不复制原文列表）。"""
    data = dict(record.payload)
    if record.source_type in ("prometheus", "loki"):
        payload = {"query": data.get("query"), "limit": data.get("limit")}
        for key in ("start", "end", "timestamp", "query_type", "lookback_seconds",
                    "budget_seconds", "window_truncated", "value"):
            if data.get(key) is not None:
                payload[key] = data[key]
        if isinstance(data.get("samples"), list):
            payload["samples"] = data["samples"]
        if isinstance(data.get("lines"), list):
            payload["lines"] = data["lines"]
        return payload
    if record.source_type == "runbook":
        payload = {
            "schema": data.get("schema") or data.get("schema_version"),
            "content": data.get("content") or data.get("summary"),
            "citations_available": bool(data.get("citations_available")),
            "source": data.get("source"),
        }
        if data.get("citations_available"):
            payload["citations"] = data.get("citations") or []
        return payload
    if record.source_type == "kubernetes":
        return {
            key: data.get(key) for key in (
                "cluster_id", "namespace", "kind", "name", "uid", "active",
                "status", "resource_version", "fingerprint", "summary",
            ) if data.get(key) is not None
        }
    if record.source_type == "timeline":
        return {
            key: data.get(key) for key in (
                "source", "event_type", "severity", "title", "summary", "occurred_at",
                "observed_at", "recorded_at", "cluster_id", "namespace",
                "resource_kind", "resource_name", "resource_uid", "details",
            ) if data.get(key) is not None
        }
    return data


def _select_with_quota(rows: list[dict], max_facts: int) -> tuple[list[dict], dict[str, int], dict[str, int]]:
    """按来源配额挑选进入 Fact Catalog 的行。

    返回 (选中行, 按来源保留数, 按来源丢弃数)。配额规则：
    1) 每个出现的来源先拿 max_facts // 来源数 的保底名额（至少 1）；
    2) 剩余名额按来源优先级顺序补足；
    3) 选中行再按 (来源优先级, evidence_id, path) 排序，保证同一输入结果稳定。
    """
    if len(rows) <= max_facts:
        kept = {source: 0 for source in {row["source_type"] for row in rows}}
        for row in rows:
            kept[row["source_type"]] = kept.get(row["source_type"], 0) + 1
        return rows, kept, {}

    groups: dict[str, list[dict]] = {}
    for row in rows:
        groups.setdefault(row["source_type"], []).append(row)
    order = sorted(groups, key=lambda source: _FACT_PRIORITY.get(source, 9))
    quota = max(1, max_facts // max(1, len(order)))
    chosen = {source: min(quota, len(groups[source])) for source in order}
    if sum(chosen.values()) > max_facts:
        # 来源数多于名额时，按优先级只保留前 max_facts 个来源各 1 条，绝不超上限
        remaining = max_facts
        for source in order:
            keep = 1 if remaining > 0 else 0
            chosen[source] = keep
            remaining -= keep
    remaining = max_facts - sum(chosen.values())
    for source in order:
        if remaining <= 0:
            break
        extra = min(remaining, len(groups[source]) - chosen[source])
        chosen[source] += extra
        remaining -= extra

    picked: list[dict] = []
    kept_by_source: dict[str, int] = {}
    dropped_by_source: dict[str, int] = {}
    for source in order:
        picked.extend(groups[source][: chosen[source]])
        kept_by_source[source] = chosen[source]
        dropped = len(groups[source]) - chosen[source]
        if dropped > 0:
            dropped_by_source[source] = dropped
    picked.sort(key=lambda row: (
        _FACT_PRIORITY.get(row["source_type"], 9), row["evidence_id"], row["path"],
    ))
    return picked, kept_by_source, dropped_by_source


def _fact_rows(record: EvidenceRecord) -> list[tuple[str, object]]:
    try:
        payload = _fact_payload(record)
        return list(_scalars(payload))
    except Exception:
        return []


def candidates_from_evidence(
    state: dict,
    scope: InvestigationScope,
    window: Optional[InvestigationWindow] = None,
    *,
    max_rows: int = 500,
    max_bytes: int = 65536,
) -> tuple[list[EvidenceCandidate], list[RejectedCandidate]]:
    """把旧 ``state["evidence"]`` 投影成候选：ledger_key 保留旧 id 作为定位。

    旧来源类型没有契约映射时，不猜测，直接以 INVALID_SOURCE 拒绝并记录。
    """
    window = window or scope.window
    candidates: list[EvidenceCandidate] = []
    rejected: list[RejectedCandidate] = []
    for ledger_key, item in (state.get("evidence") or {}).items():
        item = item or {}
        raw_source = str(item.get("source_type") or "")
        mapped = SOURCE_MAP.get(raw_source)
        if mapped is None:
            rejected.append(RejectedCandidate(candidate_id=str(ledger_key), reason_code="INVALID_SOURCE",
                                              summary=f"unmapped source_type {raw_source}"))
            continue
        source_type, source_system = mapped
        payload = item.get("data")
        if not isinstance(payload, dict):
            payload = {"value": payload}
        candidates.append(EvidenceCandidate(
            candidate_id=f"legacy:{ledger_key}",
            run_id=scope.run_id,
            service_id=str(item.get("service_id") or scope.service_id),
            incident_id=scope.incident_id,
            source_type=source_type,
            source_system=source_system,
            source_locator={
                "ledger_key": str(ledger_key),
                "source_ref": str(item.get("source_ref") or ""),
                # MCP 证据把实际发出的 PromQL/LogQL 记录为 selector，供 SELECTOR_MISSING 校验
                **({"selector": str(payload.get("selector") or payload.get("query"))}
                   if (payload.get("selector") or payload.get("query"))
                   and source_type in ("prometheus", "loki") else {}),
            },
            query_metadata={"kind": str(item.get("kind") or "")},
            window=window,
            payload=payload,
            payload_schema=str(item.get("kind") or source_type),
            content_digest=canonical_digest(payload),
            truncated=bool(item.get("truncated")),
            trust_level=_TRUST.get(source_type, "observed_fact"),
        ))
    return candidates, rejected
