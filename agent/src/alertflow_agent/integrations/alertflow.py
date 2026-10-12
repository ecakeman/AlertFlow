from datetime import datetime, timezone

import httpx


def _epoch(value) -> int:
    if isinstance(value, (int, float)):
        return int(value)
    text = str(value).replace("Z", "+00:00")
    return int(datetime.fromisoformat(text).timestamp())


class AlertFlowClient:
    def __init__(self, base_url: str, token: str, timeout: float = 10) -> None:
        self.base = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _get(self, path: str, params: dict | None = None) -> dict:
        response = httpx.get(
            self.base + path,
            params=params,
            headers={"Authorization": f"Bearer {self.token}"},
            timeout=self.timeout,
        )
        response.raise_for_status()
        return response.json()

    def runtime_config(self) -> dict:
        """AlertFlow 侧的运行配置（模型端点/密钥/记忆开关）——UI 改完这里立刻生效。"""
        return self._get("/internal/agent/v1/config")

    def incidents(self, service_id: str, limit: int = 10) -> dict:
        return self._get("/internal/agent/v1/incidents", {"serviceId": service_id, "limit": limit})

    def incident(self, incident_id: int) -> dict:
        return self._get(f"/internal/agent/v1/incidents/{incident_id}")

    def metrics(self, service_id: str, range_: str = "24h") -> dict:
        return self._get(f"/internal/agent/v1/services/{service_id}/metrics", {"range": range_})

    def checks(self, service_id: str, before: int | None, limit: int) -> dict:
        params = {"limit": limit}
        if before:
            params["before"] = before
        return self._get(f"/internal/agent/v1/services/{service_id}/checks", params)


def normalize_check(row: dict) -> dict:
    return {
        "id": row.get("id"),
        "ts": _epoch(row["ts"]) if not isinstance(row.get("ts"), (int, float)) else int(row["ts"]),
        "status": row.get("status"),
        "latency_ms": row.get("latencyMs", row.get("latency_ms")),
        "status_code": row.get("statusCode", row.get("status_code")),
        "error": row.get("error") or "",
        "attempts": row.get("attempts") or 1,
    }


def collect_checks(client: AlertFlowClient, service_id: str, window_from: int, window_to: int, max_rows: int) -> list[dict]:
    rows = []
    before = None
    while len(rows) < max_rows:
        page = client.checks(service_id, before, min(500, max_rows - len(rows)))
        batch = page.get("checks") or []
        if not batch:
            break
        for raw in batch:
            row = normalize_check(raw)
            if row["ts"] < window_from:
                return [r for r in rows if window_from <= r["ts"] <= window_to]
            if row["ts"] <= window_to:
                rows.append(row)
        nxt = page.get("nextBefore")
        if not nxt:
            break
        before = int(nxt)
    return rows


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()
