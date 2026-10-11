from __future__ import annotations

from datetime import datetime


def _epoch(value) -> int:
    if isinstance(value, (int, float)):
        return int(value)
    return int(datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp())


def _ts(row: dict) -> int:
    return _epoch(row["ts"])


def status_counts(rows: list[dict]) -> dict:
    counts = {"online": 0, "warning": 0, "offline": 0}
    for row in rows:
        status = row.get("status")
        if status in counts:
            counts[status] += 1
    total = sum(counts.values())

    def pack(name: str) -> dict:
        n = counts[name]
        pct = round(100 * n / total, 2) if total else 0
        return {"n": n, "pct": pct}

    window = {"from": None, "to": None}
    if rows:
        ordered = sorted(rows, key=_ts)
        window = {"from": ordered[0]["ts"], "to": ordered[-1]["ts"]}
    return {"window": window, "total": total, **{k: pack(k) for k in counts}}


def segments(rows: list[dict]) -> dict:
    ordered = sorted(rows, key=_ts)
    out = []
    transitions = 0
    last_success_before = None
    first_failure = None
    first_success_after = None
    longest = 0
    for row in ordered:
        status = row.get("status")
        ts = row["ts"]
        if not out or out[-1]["status"] != status:
            if out:
                transitions += 1
            out.append({"status": status, "start": ts, "end": ts, "count": 1})
        else:
            out[-1]["end"] = ts
            out[-1]["count"] += 1
        if status == "offline":
            if first_failure is None:
                first_failure = ts
            span = _epoch(out[-1]["end"]) - _epoch(out[-1]["start"])
            longest = max(longest, span)
        if status in ("online", "warning") and first_failure is None:
            last_success_before = ts
        if status in ("online", "warning") and first_failure is not None and first_success_after is None:
            first_success_after = ts
    return {
        "segments": out,
        "longest_offline_s": longest,
        "transitions": transitions,
        "last_success_before": last_success_before,
        "first_failure": first_failure,
        "first_success_after": first_success_after,
    }


def key_checks(rows: list[dict], limit: int = 20) -> list[dict]:
    ordered = sorted(rows, key=_ts)
    picked: list[dict] = []
    seen: set[int] = set()

    def add(row: dict | None) -> None:
        if row is None or len(picked) >= limit:
            return
        ident = int(row["id"])
        if ident in seen:
            return
        seen.add(ident)
        picked.append(row)

    last_ok = None
    first_bad = None
    last_bad = None
    first_ok_after = None
    for row in ordered:
        status = row.get("status")
        if status == "offline":
            if first_bad is None:
                first_bad = row
            last_bad = row
        elif first_bad is None:
            last_ok = row
        elif first_ok_after is None:
            first_ok_after = row
    add(last_ok)
    add(first_bad)
    add(last_bad)
    add(first_ok_after)
    by_error: dict[str, dict] = {}
    for row in ordered:
        text = (row.get("error") or "").strip().lower()
        if text and text not in by_error:
            by_error[text] = row
    for row in by_error.values():
        add(row)
    slow = sorted(
        [r for r in ordered if r.get("status") == "online" and r.get("latency_ms") is not None],
        key=lambda r: r["latency_ms"],
        reverse=True,
    )[:5]
    for row in slow:
        add(row)
    return picked


def coverage(rows: list[dict], window_from: int, window_to: int, interval: int | None) -> dict:
    ordered = sorted(rows, key=_ts)
    gaps = []
    first_ts = ordered[0]["ts"] if ordered else None
    last_ts = ordered[-1]["ts"] if ordered else None
    if ordered and _ts(ordered[0]) > window_from:
        gaps.append("HISTORY_TRIMMED")
    if interval and interval > 0 and len(ordered) > 1:
        for prev, nxt in zip(ordered, ordered[1:]):
            if _ts(nxt) - _ts(prev) > 2 * interval:
                gaps.append("SPARSE_CHECKS")
                break
    expected = None
    if interval and interval > 0 and window_to > window_from:
        expected = max(1, int((window_to - window_from) / interval))
    return {
        "from": window_from,
        "to": window_to,
        "first_ts": first_ts,
        "last_ts": last_ts,
        "returned": len(ordered),
        "expected": expected,
        "gaps": gaps,
    }
