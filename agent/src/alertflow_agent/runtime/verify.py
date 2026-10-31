import json

from alertflow_agent.runtime.schemas import LLMReport

_MAX_FACTS = 100
_MAX_DEPTH = 4
_MAX_LIST = 8
_MAX_TEXT = 200

# ---- P3：来源级事实提取 ------------------------------------------------------
# fact 只允许来自 _FACT_FIELDS 显式列出的字段。payload 里的任意字符串不再被递归成
# 事实：日志原文、模型摘要、HTML 之类的自由文本没有进入 catalog 的入口。
# 白名单字段的值如果是 dict/list，仍用 _scalars 做有界展开（如 detail.pool_depth），
# 但只有白名单字段可以被展开。

# Loki 日志行允许提取的结构化键；顺序即事实顺序，保证确定性。
_LOG_LINE_FIELDS = (
    "ts", "time", "timestamp",
    "level", "severity",
    "status", "statusCode", "status_code",
    "code", "error_code",
    "method", "path", "uri",
    "duration_ms", "latency_ms",
    "pod", "namespace", "container",
)
# 这些字段里如果装的是 JSON 对象字符串（结构化日志行），解析后按上面的白名单提取。
_LOG_TEXT_KEYS = ("line", "message", "msg", "log")
_LOG_JSON_MAX_CHARS = 4096
_LOG_LINE_MAX_KEYS = 5   # 单行最多 5 条事实
_LOG_MAX_LINES = 20      # 最多检查前 20 行
_LOG_MAX_FACTS = 30      # 单条 evidence 的日志事实总上限

_FACT_FIELDS: dict[str, tuple[str, ...]] = {
    # 单条 check 行；evt:resolve 复用 alertflow_incident，因此带上 resolve 字段。
    "checks": (
        "id", "kind", "name", "status", "statusCode", "status_code",
        "error", "error_code", "latencyMs", "latency_ms", "duration_ms", "attempts",
        "severity", "title", "ts", "occurred_at", "observed_at", "recorded_at",
        "resolved_at", "duration_s",
    ),
    # 内部诊断 / analysis 聚合 / subagent review。
    "diagnostic": (
        "id", "kind", "name", "status", "status_code", "error", "error_code",
        "severity", "summary", "suggestion",
        "occurred_at", "observed_at", "recorded_at", "duration_s", "resolved_at",
        "version", "env", "instance", "region", "uptime_s",
        "detail", "attributes",
        "window", "total", "online", "warning", "offline",
        "recent", "baseline", "median_ratio", "warning_rows", "low_samples",
        "no_baseline", "basis",
        "classes", "top_errors", "status_codes", "attempts_hist",
        "segments", "longest_offline_s", "transitions",
        "last_success_before", "first_failure", "first_success_after",
        "from", "to", "first_ts", "last_ts", "returned", "expected", "gaps",
    ),
    # Go 指标：series 永远不进 catalog，只暴露聚合 stats 与窗口。
    "go_metric": (
        "range", "samples", "from", "to",
        "min_avg_latency", "max_avg_latency", "last_avg_latency", "stats",
    ),
    "timeline": (
        "id", "kind", "name", "uid", "source", "event_type", "eventType",
        "severity", "title", "summary", "status", "duration_s", "resolved_at",
        "occurred_at", "observed_at", "recorded_at",
        "cluster_id", "namespace", "resource_kind", "resource_name", "resource_uid",
        "details", "sources", "query_from", "query_to",
    ),
    "kubernetes": (
        "kind", "name", "namespace", "uid", "cluster_id", "status", "active",
        "restarts", "ready", "resource_version", "fingerprint", "summary",
        "labels", "annotations", "occurred_at", "observed_at", "recorded_at",
    ),
    # Prometheus：查询与窗口必须可回溯，值只取明确字段。
    "prometheus": (
        "query", "selector", "limit", "start", "end", "timestamp",
        "query_type", "lookback_seconds", "budget_seconds", "window_truncated",
        "value", "samples", "metric", "labels", "truncated",
    ),
    # Loki：value 是日志原文，绝不进 catalog；lines 只按 _LOG_LINE_FIELDS 提取。
    "loki": (
        "query", "selector", "limit", "start", "end", "timestamp",
        "query_type", "lookback_seconds", "budget_seconds", "window_truncated",
        "samples", "metric", "labels", "truncated", "lines",
    ),
    "runbook": (
        "schema", "schema_version", "content", "summary",
        "citations_available", "source", "citations",
    ),
    "memory_reference": (
        "id", "service_id", "incident_id", "started_at", "ended_at",
        "card", "status", "summary", "occurred_at", "observed_at", "recorded_at",
    ),
}

# 没有 source_type 的旧 evidence 用 kind 兜底；未知来源不产生事实（不猜）。
_KIND_SOURCES: dict[str, str] = {
    "check": "checks",
    "http_check": "checks",
    "analysis": "diagnostic",
    "comment": "diagnostic",
    "service_info": "diagnostic",
    "history_review": "diagnostic",
    "tls": "diagnostic",
    "coverage": "timeline",
    "event": "timeline",
    "incident": "timeline",
    "resolve": "timeline",
    "metric_series": "go_metric",
    "runbook": "runbook",
    "episode": "memory_reference",
}


def build_catalog(evidence: dict) -> list[dict]:
    """Scalar facts the model may cite. The model does not choose path or value."""
    rows = []
    for ident, item in evidence.items():
        item = item or {}
        for path, value in fact_rows(item.get("source_type"), item.get("data"), item.get("kind")):
            rows.append({"evidence_id": ident, "path": path, "value": value})
    rows.sort(key=lambda row: (_priority(row["evidence_id"], evidence), row["evidence_id"], row["path"]))
    catalog = []
    for index, row in enumerate(rows[:_MAX_FACTS], start=1):
        catalog.append({"fact_id": f"f{index}", **row})
    return catalog


def canonical_source(source_type: object, kind: object = None) -> str | None:
    """把旧 source_type / kind 归一到 _FACT_FIELDS 的 key；认不出来就返回 None。"""
    source = str(source_type or "")
    if source in _FACT_FIELDS:
        return source
    if source:
        # 旧 registry 的 source_type → SourceType 映射只在 evidence.SOURCE_MAP 里维护
        # 一份，这里延迟导入避免 evidence.py ↔ verify.py 的循环依赖。
        from alertflow_agent.runtime.evidence import SOURCE_MAP

        mapped = SOURCE_MAP.get(source)
        if mapped is not None and mapped[0] in _FACT_FIELDS:
            return mapped[0]
    fallback = _KIND_SOURCES.get(str(kind or ""))
    return fallback if fallback in _FACT_FIELDS else None


def fact_rows(source_type: object, payload: object, kind: object = None) -> list[tuple[str, object]]:
    """单一事实提取入口：EvidenceManager 与 build_catalog 共用，避免两条路径松紧不一。"""
    source = canonical_source(source_type, kind)
    if source is None or not isinstance(payload, dict):
        return []
    rows: list[tuple[str, object]] = []
    for field in _FACT_FIELDS[source]:
        if field not in payload:
            continue
        value = payload[field]
        if source == "loki" and field == "lines":
            rows.extend(_log_rows(value))
        elif isinstance(value, (dict, list)):
            rows.extend(_scalars({field: value}))
        elif _is_fact_scalar(value):
            rows.append((field, value))
    return rows


def _is_fact_scalar(value: object) -> bool:
    if isinstance(value, (bool, int)):
        return True
    if isinstance(value, float):
        return value == value  # NaN 不是事实
    if isinstance(value, str):
        return 0 < len(value) <= _MAX_TEXT
    return False


def _log_rows(lines: object) -> list[tuple[str, object]]:
    """日志只提结构化字段；普通文本行不提事实（原文仍可通过 evidence_id 引用）。"""
    if not isinstance(lines, list):
        # MCP 摘要里 lines 是命中行数，保留成一条计数事实。
        return [("lines", lines)] if _is_fact_scalar(lines) else []
    rows: list[tuple[str, object]] = []
    budget = _LOG_MAX_FACTS
    for index, entry in enumerate(lines):
        if index >= _LOG_MAX_LINES or budget <= 0:
            break
        fields = _structured_log_fields(entry)
        taken = 0
        for key in _LOG_LINE_FIELDS:
            if taken >= _LOG_LINE_MAX_KEYS or budget <= 0:
                break
            if key not in fields or not _is_fact_scalar(fields[key]):
                continue
            rows.append((f"lines.{index}.{key}", fields[key]))
            taken += 1
            budget -= 1
    return rows


def _structured_log_fields(entry: object) -> dict:
    """提取一行日志里白名单内的结构化键；普通文本行返回空 dict。"""
    if isinstance(entry, str):
        return _json_fields(entry)
    if not isinstance(entry, dict):
        return {}
    fields = {key: entry[key] for key in _LOG_LINE_FIELDS if key in entry}
    for text_key in _LOG_TEXT_KEYS:
        text = entry.get(text_key)
        if not isinstance(text, str):
            continue
        for key, value in _json_fields(text).items():
            fields.setdefault(key, value)
        break
    return fields


def _json_fields(text: str) -> dict:
    snippet = text.strip()
    if not snippet.startswith("{") or len(snippet) > _LOG_JSON_MAX_CHARS:
        return {}
    try:
        parsed = json.loads(snippet)
    except (TypeError, ValueError):
        return {}
    if not isinstance(parsed, dict):
        return {}
    return {key: parsed[key] for key in _LOG_LINE_FIELDS if key in parsed}


def _priority(ident: str, evidence: dict) -> int:
    item = evidence.get(ident) or {}
    kind = item.get("kind")
    if ident == "evt:resolve" or kind == "resolve":
        return 0
    if kind == "analysis":
        return 1
    if kind == "tls" or item.get("source_type") == "project_diagnostic":
        return 2
    if kind == "metric_series":
        return 3
    return 4


def fact_id(catalog: list[dict], evidence_id: str, path: str) -> str | None:
    for row in catalog:
        if row["evidence_id"] == evidence_id and row["path"] == path:
            return row["fact_id"]
    return None


def verify(draft: dict, evidence: dict, tokens: list[str], gaps: list[str] | None = None, catalog: list[dict] | None = None) -> dict:
    findings = []
    catalog = catalog if catalog is not None else build_catalog(evidence)
    by_id = {row["fact_id"]: row for row in catalog}
    try:
        parsed = LLMReport.model_validate(draft).model_dump()
    except Exception:
        findings.append({"rule": "schema", "detail": "schema"})
        parsed = draft if isinstance(draft, dict) else {}
        assembled = parsed
    else:
        assembled = _assemble(parsed, by_id, evidence, findings)
        present = {item.get("code") for item in assembled.get("unknowns") or []}
        for gap in gaps or []:
            if gap not in present:
                assembled.setdefault("unknowns", []).append({"text": _gap_text(gap), "code": gap, "evidence_ids": []})
    return {"passed": not findings, "findings": findings, "warnings": [], "draft": _redact(assembled, tokens)}


def _gap_text(gap: str) -> str:
    if gap.startswith("PROMETHEUS_"):
        return "Prometheus has no evidence."
    if gap.startswith("LOKI_"):
        return "Loki has no evidence."
    return f"Coverage gap {gap}."


def cited(report: dict) -> set[str]:
    ids = set()
    for obs in report.get("observations") or []:
        ids.update(obs.get("evidence_ids") or [])
        for fact in obs.get("facts") or []:
            ids.add(fact.get("evidence_id"))
    for hyp in report.get("hypotheses") or []:
        ids.update(hyp.get("supporting_evidence_ids") or [])
        ids.update(hyp.get("weakening_evidence_ids") or [])
        ids.update(hyp.get("evidence_ids") or [])
    for unk in report.get("unknowns") or []:
        ids.update(unk.get("evidence_ids") or [])
    ids.discard(None)
    return ids


def _scalars(data, prefix: str = "", depth: int = 0) -> list[tuple[str, object]]:
    """有界展开单个白名单字段；调用方已经决定哪些字段可以进来。"""
    if depth > _MAX_DEPTH:
        return []
    if isinstance(data, dict):
        found = []
        for key, value in data.items():
            if key in {"series", "preview"}:
                continue
            path = f"{prefix}.{key}" if prefix else str(key)
            found.extend(_scalars(value, path, depth + 1))
        return found
    if isinstance(data, list):
        if len(data) > _MAX_LIST:
            return []
        found = []
        for index, value in enumerate(data):
            path = f"{prefix}.{index}" if prefix else str(index)
            found.extend(_scalars(value, path, depth + 1))
        return found
    if prefix and _is_fact_scalar(data):
        return [(prefix, data)]
    return []


def _facts(ids, by_id: dict, findings: list) -> tuple[list[str], list[dict]]:
    """事实引用必须能在本 run 的 catalog 中定位；解析结果原样带回 draft。"""
    kept, facts = [], []
    for fact_id_value in ids or []:
        row = by_id.get(fact_id_value)
        if not row:
            findings.append({"rule": "fact_ref", "detail": fact_id_value})
            continue
        kept.append(fact_id_value)
        facts.append({"evidence_id": row["evidence_id"], "path": row["path"], "value": row["value"]})
    return kept, facts


def _assemble(parsed: dict, by_id: dict, evidence: dict, findings: list) -> dict:
    # 报告级 fact_ids：只校验可定位性，具体事实仍从 catalog 取
    _facts(parsed.get("fact_ids") or [], by_id, findings)
    observations = []
    for obs in parsed.get("observations") or []:
        _, facts = _facts(obs.get("fact_ids") or [], by_id, findings)
        observations.append({
            "id": obs.get("id"),
            "text": obs.get("text"),
            "evidence_ids": _known_ids(obs.get("evidence_ids") or [], evidence, findings),
            "facts": facts,
        })
    hypotheses = []
    for hyp in parsed.get("hypotheses") or []:
        supporting = _known_ids(hyp.get("supporting_evidence_ids") or [], evidence, findings)
        weakening = _known_ids(hyp.get("weakening_evidence_ids") or [], evidence, findings)
        supporting_ids, supporting_facts = _facts(hyp.get("supporting_fact_ids") or [], by_id, findings)
        weakening_ids, weakening_facts = _facts(hyp.get("weakening_fact_ids") or [], by_id, findings)
        status = hyp.get("status")
        if status == "supported" and not supporting and not supporting_facts:
            findings.append({"rule": "conclusion", "detail": hyp.get("id")})
        if status in {"weakened", "ruled_out"} and not weakening and not weakening_facts:
            findings.append({"rule": "conclusion", "detail": hyp.get("id")})
        hypotheses.append({
            "id": hyp.get("id"),
            "text": hyp.get("text"),
            "layer": hyp.get("layer"),
            "status": status,
            "confidence": None,
            "supporting_evidence_ids": supporting,
            "weakening_evidence_ids": weakening,
            "supporting_fact_ids": supporting_ids,
            "weakening_fact_ids": weakening_ids,
            "supporting_facts": supporting_facts,
            "weakening_facts": weakening_facts,
            "rationale": "",
            "missing_info": [],
        })
    basis = parsed.get("conclusion_basis_hypothesis_id")
    if basis and basis not in {str(item.get("id")) for item in hypotheses}:
        findings.append({"rule": "conclusion", "detail": str(basis)})
    unknowns = []
    for unknown in parsed.get("unknowns") or []:
        unknowns.append({
            "text": unknown.get("text"),
            "code": unknown.get("code"),
            "evidence_ids": _known_ids(unknown.get("evidence_ids") or [], evidence, findings),
        })
    conclusion = parsed.get("conclusion") or {}
    if not isinstance(conclusion, dict) or not conclusion.get("text"):
        # 兜底：模型没给结论时，取最强的一条 supported / possible 假设，
        # 这样"结论"永远存在，且来源可解释。
        best = next((h for h in hypotheses if h.get("status") == "supported"), None) or \
            next((h for h in hypotheses if h.get("status") == "possible"), None)
        conclusion = {
            "text": (best or {}).get("text") or "证据不足以形成结论",
            "layer": (best or {}).get("layer") or "unknown",
            "confidence": "medium" if best and best.get("status") == "supported" else "low",
            "evidence_ids": (best or {}).get("supporting_evidence_ids") or [],
            "recurrence_of": None,
        }
    conclusion["evidence_ids"] = _known_ids(conclusion.get("evidence_ids") or [], evidence, findings)
    return {
        "summary": parsed.get("summary"),
        "conclusion": conclusion,
        "observations": observations,
        "hypotheses": hypotheses,
        "unknowns": unknowns,
        "recommended_next_steps": parsed.get("recommended_next_steps") or [],
    }


def _known_ids(ids, evidence, findings) -> list[str]:
    kept = []
    for ident in ids:
        if ident not in evidence:
            findings.append({"rule": "evidence_ref", "detail": ident})
            continue
        kept.append(ident)
    return kept


def _redact(draft: dict, tokens: list[str]) -> dict:
    if not isinstance(draft, dict):
        return {}
    text = json.dumps(draft)
    for token in tokens:
        if token:
            text = text.replace(token, "[REDACTED]")
    return json.loads(text)
