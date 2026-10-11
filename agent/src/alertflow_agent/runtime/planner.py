"""Plan diff rules. An illegal plan does not replace the one already stored."""


def validate_plan_diff(old: dict | None, new: dict, evidence: dict) -> bool:
    if not isinstance(new, dict) or not new.get("hypotheses"):
        return False
    hypotheses = new["hypotheses"]
    if len(hypotheses) > 6:
        return False
    ids = [item.get("id") for item in hypotheses]
    if any(not item for item in ids) or len(ids) != len(set(ids)):
        return False
    if old is None:
        return new.get("version") == 1
    if int(new.get("version") or 0) <= int(old.get("version") or 0):
        return False
    previous = {item["id"]: item for item in old.get("hypotheses") or []}
    for ident, hyp in previous.items():
        if ident not in ids:
            return False
        current = next(item for item in hypotheses if item["id"] == ident)
        if current.get("layer") != hyp.get("layer"):
            return False
    for hyp in hypotheses:
        if hyp.get("status") in {"supported", "weakened", "ruled_out"}:
            refs = (hyp.get("evidence_for") or []) + (hyp.get("evidence_against") or [])
            if not refs or any(ref not in evidence for ref in refs):
                return False
    return True
