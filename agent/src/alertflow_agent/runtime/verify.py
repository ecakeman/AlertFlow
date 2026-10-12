import json

from alertflow_agent.runtime.schemas import LLMReport

_MAX_FACTS = 60
_MAX_DEPTH = 4
_MAX_LIST = 8
_MAX_TEXT = 200


def build_catalog(evidence: dict) -> list[dict]:
    """Scalar facts the model may cite. The model does not choose path or value."""
    rows = []
    for ident, item in evidence.items():
        payload = _catalog_payload(ident, item or {})
        if payload is None:
            continue
        for path, value in _scalars(payload):
            rows.append({"evidence_id": ident, "path": path, "value": value})
    rows.sort(key=lambda row: (_priority(row["evidence_id"], evidence), row["evidence_id"], row["path"]))
    catalog = []
    for index, row in enumerate(rows[:_MAX_FACTS], start=1):
        catalog.append({"fact_id": f"f{index}", **row})
    return catalog


def _priority(ident: str, evidence: dict) -> int:
    kind = (evidence.get(ident) or {}).get("kind")
    if ident == "evt:resolve" or kind == "resolve":
        return 0
    if kind == "analysis":
        return 1
    if kind == "tls":
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
                assembled.setdefault("unknowns", []).append({"text": f"Coverage gap {gap}.", "code": gap, "evidence_ids": []})
    return {"passed": not findings, "findings": findings, "warnings": [], "draft": _redact(assembled, tokens)}


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


def _catalog_payload(ident: str, item: dict):
    kind = item.get("kind")
    data = item.get("data") or {}
    if ident == "evt:resolve" or kind == "resolve":
        return data
    if kind in {"http_check", "analysis", "tls"}:
        return data
    if kind == "metric_series":
        return data.get("stats") or {}
    return None


def _scalars(data, prefix: str = "", depth: int = 0) -> list[tuple[str, object]]:
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
    if isinstance(data, bool) or isinstance(data, (int, float, str)):
        if isinstance(data, float) and data != data:
            return []
        if isinstance(data, str) and len(data) > _MAX_TEXT:
            return []
        if prefix:
            return [(prefix, data)]
    return []


def _assemble(parsed: dict, by_id: dict, evidence: dict, findings: list) -> dict:
    observations = []
    for obs in parsed.get("observations") or []:
        facts = []
        for fact_id_value in obs.get("fact_ids") or []:
            row = by_id.get(fact_id_value)
            if not row:
                findings.append({"rule": "fact_ref", "detail": fact_id_value})
                continue
            facts.append({"evidence_id": row["evidence_id"], "path": row["path"], "value": row["value"]})
        observations.append({
            "id": obs.get("id"),
            "text": obs.get("text"),
            "evidence_ids": _known_ids(obs.get("evidence_ids") or [], evidence, findings),
            "facts": facts,
        })
    hypotheses = []
    for hyp in parsed.get("hypotheses") or []:
        ids = _known_ids(hyp.get("evidence_ids") or [], evidence, findings)
        status = hyp.get("status")
        if status in {"supported", "ruled_out"} and not ids:
            findings.append({"rule": "conclusion", "detail": hyp.get("id")})
        supporting = ids if status in {"supported", "possible", "undetermined"} else []
        weakening = ids if status in {"weakened", "ruled_out"} else []
        hypotheses.append({
            "id": hyp.get("id"),
            "text": hyp.get("text"),
            "layer": hyp.get("layer"),
            "status": status,
            "confidence": None,
            "supporting_evidence_ids": supporting,
            "weakening_evidence_ids": weakening,
            "rationale": "",
            "missing_info": [],
        })
    unknowns = []
    for unknown in parsed.get("unknowns") or []:
        unknowns.append({
            "text": unknown.get("text"),
            "code": unknown.get("code"),
            "evidence_ids": _known_ids(unknown.get("evidence_ids") or [], evidence, findings),
        })
    return {
        "summary": parsed.get("summary"),
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
