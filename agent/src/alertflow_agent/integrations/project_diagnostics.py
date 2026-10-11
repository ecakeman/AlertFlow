import os
from datetime import datetime, timezone

import httpx
import yaml

_STATUSES = {"healthy", "degraded", "unhealthy", "unknown"}
_ORDER = ("not_configured", "unavailable", "invalid", "partial", "stale", "empty", "ok")


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
        return {"coverage": "not_configured", "items": [], "dropped": 0}
    if cfg.get("_invalid"):
        return {"coverage": "invalid", "items": [], "dropped": 0}
    if since == until:
        return {"coverage": "invalid", "items": [], "dropped": 0}
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
        return {"coverage": "unavailable", "items": [], "dropped": 0}
    if 300 <= response.status_code < 400:
        return {"coverage": "invalid", "items": [], "dropped": 0}
    if response.status_code < 200 or response.status_code >= 300:
        return {"coverage": "unavailable", "items": [], "dropped": 0}
    if len(response.content) > 512 * 1024:
        return {"coverage": "invalid", "items": [], "dropped": 0}
    try:
        body = response.json()
    except Exception:
        return {"coverage": "invalid", "items": [], "dropped": 0}
    return validate(body, event_time)


def validate(body, event_time: datetime | None = None) -> dict:
    if not isinstance(body, dict) or body.get("schema_version") != "v1" or not isinstance(body.get("items"), list):
        return {"coverage": "invalid", "items": [], "dropped": 0}
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
    return {"coverage": _pick(coverage), "items": kept, "dropped": dropped}


def _pick(coverage: str) -> str:
    return coverage if coverage in _ORDER else "invalid"


def _item(item) -> dict | None:
    if not isinstance(item, dict):
        return None
    kind = item.get("kind")
    observed = _aware(item.get("observed_at"))
    if observed is None or kind not in {"component_health", "dependency_health", "recent_error"}:
        return None
    if kind == "recent_error":
        if not item.get("name") or not item.get("summary"):
            return None
    else:
        if not item.get("name") or item.get("status") not in _STATUSES:
            return None
    attributes = item.get("attributes") or {}
    if not isinstance(attributes, dict) or len(attributes) > 20:
        return None
    if any(len(str(value)) > 200 for value in attributes.values()):
        return None
    cleaned = {
        "kind": kind,
        "name": str(item.get("name")),
        "observed_at": observed.isoformat(),
        "summary": str(item.get("summary") or item.get("status") or ""),
        "status": item.get("status"),
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
