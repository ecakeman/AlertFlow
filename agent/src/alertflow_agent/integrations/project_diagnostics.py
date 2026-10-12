import os
from datetime import datetime, timezone

import httpx
import yaml

_STATUSES = {"healthy", "degraded", "unhealthy", "unknown"}
_ORDER = ("not_configured", "unavailable", "invalid", "partial", "stale", "empty", "ok")
# v1 and v2 are both accepted; v2 adds internal-state kinds and structured detail.
_SCHEMA_VERSIONS = {"v1", "v2"}
_KINDS = {
    "component_health",
    "dependency_health",
    "recent_error",
    # v2 — internal state
    "resource",
    "saturation",
    "recent_change",
    "slow_operation",
}
_STATUS_KINDS = {"component_health", "dependency_health", "resource"}
_SUMMARY_KINDS = {"recent_error", "recent_change", "slow_operation"}
_MAX_DETAIL_KEYS = 24
_MAX_DETAIL_DEPTH = 3
_MAX_DETAIL_TEXT = 400


def load_config(path: str) -> dict | None:
    if not path or not os.path.exists(path):
        return None
    try:
        with open(path, encoding="utf-8") as handle:
            data = yaml.safe_load(handle) or {}
    except Exception:
        return {"_invalid": True}
    if not isinstance(data, dict) or not _http_url(data.get("url")):
        return {"_invalid": True}
    return data


def _http_url(value) -> bool:
    return isinstance(value, str) and value.startswith(("http://", "https://"))


def fetch(cfg: dict | None, since: str, until: str, event_time: datetime | None = None) -> dict:
    if not cfg:
        return {"coverage": "not_configured", "items": [], "dropped": 0, "service": None}
    if cfg.get("_invalid"):
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    if since == until:
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    headers = {}
    token_env = cfg.get("token_env")
    if token_env and os.environ.get(token_env):
        headers["Authorization"] = f"Bearer {os.environ[token_env]}"
    try:
        response = httpx.get(
            cfg["url"],
            params={"since": since, "until": until},
            headers=headers,
            timeout=5,
            follow_redirects=False,
        )
    except Exception:
        return {"coverage": "unavailable", "items": [], "dropped": 0, "service": None}
    if 300 <= response.status_code < 400:
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    if response.status_code < 200 or response.status_code >= 300:
        return {"coverage": "unavailable", "items": [], "dropped": 0, "service": None}
    if len(response.content) > 512 * 1024:
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    try:
        body = response.json()
    except Exception:
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    return validate(body, event_time)


def validate(body, event_time: datetime | None = None) -> dict:
    if (
        not isinstance(body, dict)
        or body.get("schema_version") not in _SCHEMA_VERSIONS
        or not isinstance(body.get("items"), list)
    ):
        return {"coverage": "invalid", "items": [], "dropped": 0, "service": None}
    items = body["items"]
    kept = []
    dropped = max(0, len(items) - 100)
    stale = False
    for item in items[:100]:
        parsed = _item(item)
        if parsed is None:
            dropped += 1
            continue
        if event_time and _is_stale(parsed["observed_at"], event_time):
            stale = True
            parsed["limitations"] = ["stale snapshot, not the state at the event"]
        kept.append(parsed)
    if not kept and (dropped or not body["items"]):
        coverage = "invalid" if dropped else "empty"
    elif dropped:
        coverage = "partial"
    elif stale:
        coverage = "stale"
    elif not kept:
        coverage = "empty"
    else:
        coverage = "ok"
    return {"coverage": _pick(coverage), "items": kept, "dropped": dropped, "service": _service(body.get("service"))}


def _service(raw) -> dict | None:
    if not isinstance(raw, dict) or not raw.get("name"):
        return None
    allowed = ("name", "version", "env", "instance", "region", "uptime_s", "build")
    clean = {k: raw[k] for k in allowed if k in raw and isinstance(raw[k], (str, int, dict))}
    return clean or None


def _pick(coverage: str) -> str:
    return coverage if coverage in _ORDER else "invalid"


def _clean_detail(value, depth: int = 0):
    """Bound a kind-specific ``detail`` object; returns None when unusable."""
    if depth > _MAX_DETAIL_DEPTH:
        return None
    if isinstance(value, dict):
        if len(value) > _MAX_DETAIL_KEYS:
            return None
        out = {}
        for key, item in value.items():
            if not isinstance(key, str) or len(key) > 64:
                return None
            cleaned = _clean_detail(item, depth + 1)
            if cleaned is None:
                return None
            out[key] = cleaned
        return out
    if isinstance(value, list):
        if len(value) > _MAX_DETAIL_KEYS:
            return None
        out = []
        for item in value:
            cleaned = _clean_detail(item, depth + 1)
            if cleaned is None:
                return None
            out.append(cleaned)
        return out
    if isinstance(value, str):
        return value if len(value) <= _MAX_DETAIL_TEXT else value[:_MAX_DETAIL_TEXT]
    if value is None or isinstance(value, (int, float, bool)):
        return value
    return None


def _item(item) -> dict | None:
    if not isinstance(item, dict):
        return None
    kind = item.get("kind")
    observed = _aware(item.get("observed_at"))
    if observed is None or kind not in _KINDS or not item.get("name"):
        return None
    if kind in _STATUS_KINDS and item.get("status") not in _STATUSES:
        return None
    if kind in _SUMMARY_KINDS and not item.get("summary"):
        return None
    attributes = item.get("attributes") or {}
    if not isinstance(attributes, dict) or len(attributes) > 20:
        return None
    if any(len(str(value)) > 200 for value in attributes.values()):
        return None
    detail = item.get("detail") or {}
    if not isinstance(detail, dict):
        return None
    detail = _clean_detail(detail)
    if detail is None:
        return None
    cleaned = {
        "kind": kind,
        "name": str(item.get("name")),
        "observed_at": observed.isoformat(),
        "summary": str(item.get("summary") or item.get("status") or ""),
        "status": item.get("status"),
        "detail": detail,
        "attributes": {str(key): str(value)[:200] for key, value in attributes.items()},
    }
    return cleaned


def _aware(value) -> datetime | None:
    if not isinstance(value, str):
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed.astimezone(timezone.utc)


def _is_stale(observed: str, event_time: datetime) -> bool:
    parsed = datetime.fromisoformat(observed)
    if event_time.tzinfo is None:
        return False
    return (event_time - parsed).total_seconds() > 300
