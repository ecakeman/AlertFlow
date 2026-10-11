import os
import threading
import uuid

from alertflow_agent.db import MemoryStore, PostgresStore, ensure_database
from tests.fakes import event_row

_URL = os.environ.get(
    "AGENT_TEST_DATABASE_URL",
    "postgres://alertflow:alertflow@127.0.0.1:5432/alertflow_agent_seq?sslmode=disable",
)


def test_memory_event_sequences_stay_unique():
    store = MemoryStore()
    row = event_row("agent")
    store.insert_run(row)

    def write():
        for _ in range(10):
            store.append_event(row["id"], "note", {"n": 1})

    threads = [threading.Thread(target=write) for _ in range(4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    seqs = [event["seq"] for event in store.list_events(row["id"])]
    assert sorted(seqs) == list(range(1, 41))


def test_postgres_event_sequences_and_resolve_are_atomic():
    pytest = __import__("pytest")
    pytest.importorskip("psycopg")
    try:
        ensure_database(_URL)
    except Exception as exc:
        pytest.skip(f"postgres unavailable: {exc}")
    left = PostgresStore(_URL)
    right = PostgresStore(_URL)
    row = event_row("agent")
    row["id"] = str(uuid.uuid4())
    row["dedupe_key"] = row["id"]
    left.insert_run(row)
    errors = []

    def write(store, name):
        try:
            for _ in range(15):
                store.append_event(row["id"], "note", {"writer": name})
        except Exception as exc:
            errors.append(exc)

    threads = [
        threading.Thread(target=write, args=(left, "left")),
        threading.Thread(target=write, args=(right, "right")),
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert errors == []
    seqs = [event["seq"] for event in left.list_events(row["id"])]
    assert sorted(seqs) == list(range(1, 31))

    import psycopg

    with psycopg.connect(_URL) as conn:
        conn.execute("SELECT id FROM runs WHERE id = %s FOR UPDATE", (row["id"],))
        conn.execute(
            "INSERT INTO run_events (run_id, seq, type, payload) VALUES (%s, 31, 'note', '{}'::jsonb)",
            (row["id"],),
        )
        conn.rollback()
    left.append_event(row["id"], "note", {"after": "rollback"})
    seqs = [event["seq"] for event in left.list_events(row["id"])]
    assert 31 in seqs
    assert seqs.count(31) == 1

    left.update(row["id"], status="completed")
    results = []

    def resolve(store):
        results.append(store.record_resolve(row["id"], "incident_resolve:1", "2026-10-11T10:05:00Z", True))

    threads = [threading.Thread(target=resolve, args=(store,)) for store in (left, right)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert sorted(results) == ["duplicate", "revise"]
    assert left.get(row["id"])["stage"] == "revise"
    assert left.get(row["id"])["input_event"]["event"] == "incident_resolve"
