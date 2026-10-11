import json
import math
import re
from datetime import datetime

from alertflow_agent.runtime.schemas import ReportDraft


def lookup(data, path: str):
    cur = data
    for part in path.split("."):
        if isinstance(cur, list):
            cur = cur[int(part)]
        elif isinstance(cur, dict):
            cur = cur[part]
        else:
            raise KeyError(path)
    return cur


def _same(left, right) -> bool:
    if isinstance(left, float) and not math.isfinite(left):
        return False
    if isinstance(right, float) and not math.isfinite(right):
        return False
    if isinstance(left, (int, float)) and isinstance(right, (int, float)):
        return abs(float(left) - float(right)) <= max(0.5, abs(float(right)) * 0.001)
    if isinstance(left, str) and isinstance(right, str):
        try:
            a = datetime.fromisoformat(left.replace("Z", "+00:00"))
            b = datetime.fromisoformat(right.replace("Z", "+00:00"))
            return a == b
        except ValueError:
            return left == right
    return left == right


def verify(draft: dict, evidence: dict, tokens: list[str], gaps: list[str] | None = None) -> dict:
    findings = []
    warnings = []
    try:
        parsed = ReportDraft.model_validate(draft).model_dump()
    except Exception:
        findings.append({"rule": "V1", "detail": "schema"})
        parsed = draft if isinstance(draft, dict) else {}
    for obs in parsed.get("observations") or []:
        _ids(obs.get("evidence_ids") or [], evidence, findings, "V2")
        for fact in obs.get("facts") or []:
            _fact(fact, evidence, findings)
    for hyp in parsed.get("hypotheses") or []:
        _ids(hyp.get("supporting_evidence_ids") or [], evidence, findings, "V2")
        _ids(hyp.get("weakening_evidence_ids") or [], evidence, findings, "V2")
        status = hyp.get("status")
        if status == "supported" and not hyp.get("supporting_evidence_ids"):
            findings.append({"rule": "V4", "detail": hyp.get("id")})
        if status in {"weakened", "ruled_out"} and not hyp.get("weakening_evidence_ids"):
            findings.append({"rule": "V4", "detail": hyp.get("id")})
        if status != "undetermined" and not hyp.get("rationale"):
            findings.append({"rule": "V4", "detail": "rationale"})
        _layer(hyp, evidence, findings)
        _confidence(hyp, evidence, findings)
    for unknown in parsed.get("unknowns") or []:
        _ids(unknown.get("evidence_ids") or [], evidence, findings, "V2")
    _numbers(parsed, evidence, warnings)
    present = {item.get("code") for item in parsed.get("unknowns") or []}
    parsed.setdefault("unknowns", [])
    for gap in gaps or []:
        if gap not in present:
            parsed["unknowns"].append({"text": f"Coverage gap {gap}.", "code": gap, "evidence_ids": []})
    redacted = _redact(parsed, tokens)
    return {"passed": not findings, "findings": findings, "warnings": warnings, "draft": redacted}


def _ids(ids, evidence, findings, rule) -> None:
    for ident in ids:
        if ident not in evidence:
            findings.append({"rule": rule, "detail": ident})


def _fact(fact, evidence, findings) -> None:
    ident = fact.get("evidence_id")
    item = evidence.get(ident)
    if not item:
        findings.append({"rule": "V3", "detail": ident})
        return
    try:
        got = lookup(item.get("data") or {}, fact["path"])
    except (KeyError, IndexError, ValueError, TypeError):
        findings.append({"rule": "V3", "detail": fact.get("path")})
        return
    if not _same(got, fact.get("value")):
        findings.append({"rule": "V3", "detail": f"{ident}.{fact.get('path')}"})


def _source(ident, evidence) -> str:
    return (evidence.get(ident) or {}).get("source_type", "")


def _layer(hyp, evidence, findings) -> None:
    layer = hyp.get("layer")
    status = hyp.get("status")
    support = hyp.get("supporting_evidence_ids") or []
    if status != "supported":
        return
    if layer in {"app_internal", "dependency"}:
        kinds = {_source(i, evidence) for i in support}
        if "project_diagnostic" not in kinds and "alertflow_external_alert" not in kinds:
            findings.append({"rule": "V5", "detail": layer})
    if layer == "tls":
        kinds = {_source(i, evidence) for i in support}
        if "alertflow_tls" not in kinds and not _cited_class(evidence, support, "tls"):
            findings.append({"rule": "V5", "detail": "tls"})
    if layer == "network_dns":
        if not _cited_class(evidence, support, "dns"):
            findings.append({"rule": "V5", "detail": "dns"})


def _confidence(hyp, evidence, findings) -> None:
    support = hyp.get("supporting_evidence_ids") or []
    if hyp.get("status") == "supported":
        kinds = {_source(i, evidence) for i in support}
        if kinds and kinds <= {"alertflow_comment", "subagent_review"}:
            findings.append({"rule": "V6", "detail": "comment-only"})
    if hyp.get("confidence") == "high":
        kinds = {_source(i, evidence) for i in support if _source(i, evidence) not in {"alertflow_comment", "subagent_review"}}
        if len(kinds) < 2:
            findings.append({"rule": "V6", "detail": "high"})


def _cited_class(evidence, support, name) -> int:
    total = 0
    for ident in support:
        if not str(ident).startswith("ana:errors@"):
            continue
        classes = ((evidence.get(ident) or {}).get("data") or {}).get("classes") or {}
        try:
            total += int(classes.get(name) or 0)
        except (TypeError, ValueError):
            continue
    return total


def _numbers(draft, evidence, warnings) -> None:
    cited = set()
    for obs in draft.get("observations") or []:
        cited.update(obs.get("evidence_ids") or [])
        for fact in obs.get("facts") or []:
            if fact.get("evidence_id"):
                cited.add(fact["evidence_id"])
    for hyp in draft.get("hypotheses") or []:
        cited.update(hyp.get("supporting_evidence_ids") or [])
        cited.update(hyp.get("weakening_evidence_ids") or [])
    for unknown in draft.get("unknowns") or []:
        cited.update(unknown.get("evidence_ids") or [])
    blob = json.dumps({
        "summary": draft.get("summary"),
        "observations": draft.get("observations"),
        "hypotheses": draft.get("hypotheses"),
    })
    present = set()
    for ident in cited:
        item = evidence.get(ident) or {}
        present.update(re.findall(r"\d+", json.dumps(item.get("data")) + " " + item.get("summary", "")))
    for number in re.findall(r"\d+", blob):
        if number not in present and number not in {"0", "1"}:
            warnings.append({"rule": "V8", "detail": number})


def _redact(draft: dict, tokens: list[str]) -> dict:
    text = json.dumps(draft)
    for token in tokens:
        if token:
            text = text.replace(token, "[REDACTED]")
    return json.loads(text)


def cited(report: dict) -> set[str]:
    ids = set()
    for obs in report.get("observations") or []:
        ids.update(obs.get("evidence_ids") or [])
        for fact in obs.get("facts") or []:
            ids.add(fact.get("evidence_id"))
    for hyp in report.get("hypotheses") or []:
        ids.update(hyp.get("supporting_evidence_ids") or [])
        ids.update(hyp.get("weakening_evidence_ids") or [])
    for unk in report.get("unknowns") or []:
        ids.update(unk.get("evidence_ids") or [])
    ids.discard(None)
    return ids
