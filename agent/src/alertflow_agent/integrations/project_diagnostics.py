import os
from datetime import datetime, timezone

import httpx
import yaml


def load_config(path: str) -> dict | None:
    if not path or not os.path.exists(path):
        return None
    with open(path, encoding="utf-8") as fh:
        data = yaml.safe_load(fh) or {}
    if not data.get("url"):
        return None
    return data


def fetch(cfg: dict | None, since: str, until: str) -> dict:
    if not cfg:
        return {"coverage": "not_configured", "items": [], "dropped": 0}
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
        if response.status_code < 200 or response.status_code >= 300:
            return {"coverage": "unavailable", "items": [], "dropped": 0}
        if len(response.content) > 512 * 1024:
            return {"coverage": "invalid", "items": [], "dropped": 0}
        body = response.json()
    except Exception:
        return {"coverage": "unavailable", "items": [], "dropped": 0}
    return validate(body)


def validate(body: dict) -> dict:
    if body.get("schema_version") != "v1" or not isinstance(body.get("items"), list):
        return {"coverage": "invalid", "items": [], "dropped": 0}
    kept = []
    dropped = 0
    stale = False
    for item in body["items"][:100]:
        if not _ok(item):
            dropped += 1
            continue
        kept.append(item)
    if dropped and not kept:
        coverage = "invalid"
    elif dropped:
        coverage = "partial"
    elif not kept:
        coverage = "empty"
    else:
        coverage = "ok"
    if stale:
        coverage = "stale"
    return {"coverage": coverage, "items": kept, "dropped": dropped, "observed_at": body.get("observed_at")}


def _ok(item: dict) -> bool:
    kind = item.get("kind")
    if kind in {"component_health", "dependency_health"}:
        return all(item.get(k) for k in ("name", "status", "observed_at")) and item["status"] in {
            "healthy", "degraded", "unhealthy", "unknown"
        }
    if kind == "recent_error":
        return all(item.get(k) for k in ("name", "observed_at", "summary"))
    return False


def mark_stale(result: dict, event_time: datetime) -> dict:
    if result["coverage"] not in {"ok", "partial", "empty"}:
        return result
    for item in result["items"]:
        observed = datetime.fromisoformat(str(item["observed_at"]).replace("Z", "+00:00"))
        if observed.tzinfo is None:
            observed = observed.replace(tzinfo=timezone.utc)
        if (event_time - observed).total_seconds() > 300:
            result["coverage"] = "stale"
            break
    return result
