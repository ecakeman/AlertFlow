from alertflow_agent.analysis.errors import classify_error
from alertflow_agent.analysis.stats import latency_stats
from alertflow_agent.analysis.timeline import key_checks, status_counts


def test_status_and_error_class():
    rows = [
        {"id": 1, "ts": 10, "status": "online", "latency_ms": 20},
        {"id": 2, "ts": 20, "status": "offline", "error": "connection refused", "status_code": None, "latency_ms": 5},
        {"id": 3, "ts": 30, "status": "offline", "error": "Service Unavailable", "status_code": 503, "latency_ms": 8},
    ]
    counted = status_counts(rows)
    assert counted["total"] == 3
    assert counted["offline"]["n"] == 2
    assert classify_error("connection refused") == "connection_refused"
    assert classify_error("Service Unavailable", 503) == "http_status"
    assert classify_error("deadline exceeded") == "timeout"


def test_latency_ignores_offline_and_marks_low_samples():
    rows = [{"status": "online", "latency_ms": 10}] * 5 + [{"status": "offline", "latency_ms": 999}]
    stats = latency_stats(rows, [{"ts": 1, "avg_latency": 10}] * 3, 100)
    assert stats["recent"]["n"] == 5
    assert stats["recent"]["p95_ms"] is None
    assert stats["low_samples"] is True
    assert stats["no_baseline"] is True


def test_key_checks_keep_boundaries():
    rows = [
        {"id": 1, "ts": 1, "status": "online", "latency_ms": 1, "error": ""},
        {"id": 2, "ts": 2, "status": "offline", "latency_ms": 2, "error": "connection refused"},
        {"id": 3, "ts": 3, "status": "online", "latency_ms": 50, "error": ""},
    ]
    picked = [row["id"] for row in key_checks(rows)]
    assert picked[:3] == [1, 2, 3]
