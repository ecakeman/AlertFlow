import math


def _nearest_rank(values: list[float], p: float) -> float | None:
    if len(values) < 20:
        return None
    ordered = sorted(values)
    index = math.ceil(p * len(ordered)) - 1
    return ordered[max(0, index)]


def _median(values: list[float]) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    mid = len(ordered) // 2
    if len(ordered) % 2:
        return ordered[mid]
    return (ordered[mid - 1] + ordered[mid]) / 2


def latency_stats(rows: list[dict], series: list[dict], lookback_from: int) -> dict:
    online = [r["latency_ms"] for r in rows if r.get("status") == "online" and r.get("latency_ms") is not None]
    warning = [r["latency_ms"] for r in rows if r.get("status") == "warning" and r.get("latency_ms") is not None]
    baseline_vals = []
    for point in series:
        ts = point.get("ts")
        avg = point.get("avg_latency")
        if ts is None or avg is None:
            continue
        if int(ts) < lookback_from:
            baseline_vals.append(float(avg))
    baseline = None
    ratio = None
    if len(baseline_vals) >= 8:
        baseline = {"n_buckets": len(baseline_vals), "median_ms": _median(baseline_vals), "basis": "bucketed_avg"}
        recent_median = _median([float(v) for v in online])
        if recent_median is not None and baseline["median_ms"]:
            ratio = recent_median / baseline["median_ms"]
    return {
        "basis": "online_only",
        "recent": {
            "n": len(online),
            "mean_ms": (sum(online) / len(online)) if online else None,
            "median_ms": _median([float(v) for v in online]),
            "p95_ms": _nearest_rank([float(v) for v in online], 0.95),
            "min_ms": min(online) if online else None,
            "max_ms": max(online) if online else None,
        },
        "baseline": baseline,
        "median_ratio": ratio,
        "warning_rows": {"n": len(warning), "median_ms": _median([float(v) for v in warning])},
        "low_samples": len(online) < 20,
        "no_baseline": len(baseline_vals) < 8,
    }


def error_stats(rows: list[dict], classify) -> dict:
    classes = {k: 0 for k in ("timeout", "connection_refused", "connection_reset", "dns", "tls", "http_status", "unknown")}
    texts: dict[str, int] = {}
    codes: dict[str, int] = {}
    attempts: dict[str, int] = {}
    for row in rows:
        if row.get("status") not in ("offline", "warning") and not row.get("error"):
            continue
        kind = classify(row.get("error") or "", row.get("status_code"))
        classes[kind] = classes.get(kind, 0) + 1
        text = (row.get("error") or "").strip()
        if text:
            texts[text] = texts.get(text, 0) + 1
        if row.get("status_code") is not None:
            key = str(row["status_code"])
            codes[key] = codes.get(key, 0) + 1
        attempts[str(row.get("attempts") or 1)] = attempts.get(str(row.get("attempts") or 1), 0) + 1
    top = [{"text": k, "count": v} for k, v in sorted(texts.items(), key=lambda kv: (-kv[1], kv[0]))[:5]]
    return {"classes": classes, "top_errors": top, "status_codes": codes, "attempts_hist": attempts}
