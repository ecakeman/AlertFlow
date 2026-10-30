import threading

from alertflow_agent.config import Settings
from alertflow_agent.runtime.guards import fresh_guard
from alertflow_agent.runtime.model import call_model


class Store:
    def __init__(self):
        self.events = []

    def append_event(self, run_id, kind, payload):
        self.events.append((kind, payload))


class ScriptModel:
    def __init__(self, script):
        self.script = list(script)
        self.calls = 0

    def complete(self, node, state, prompt):
        self.calls += 1
        item = self.script.pop(0)
        if isinstance(item, Exception):
            raise item
        return item


def _state():
    return {"run_id": "run-budget", "event": {}, "guard": fresh_guard()}


def _ok(usage=None):
    return ({"tool": "finish_investigation", "args": {}, "text": ""}, usage or {"source": "estimated"})


def test_failed_attempt_then_retry_counts_both():
    model = ScriptModel([RuntimeError("secret-token down"), _ok({"total_tokens": 12, "source": "provider"})])
    state = _state()
    settings = Settings(max_llm_calls=4, llm_api_key="secret-token")
    parsed = call_model(model, "lead", state, settings, Store())
    assert parsed["tool"] == "finish_investigation"
    assert model.calls == 2
    assert state["guard"]["llm_attempts"] == 2
    assert state["guard"]["llm_calls"] == 2
    assert state["guard"]["llm_successes"] == 1
    fresh = _state()
    call_model(ScriptModel([_ok({"total_tokens": 12, "source": "provider"})]), "lead", fresh, Settings(max_llm_calls=4), Store())
    assert fresh["guard"]["tokens"] == 12
    assert fresh["guard"]["llm_attempts"] == 1


def test_two_failures_do_not_count_as_success():
    store = Store()
    model = ScriptModel([RuntimeError("secret-token down"), RuntimeError("secret-token still")])
    state = _state()
    parsed = call_model(model, "lead", state, Settings(max_llm_calls=4, llm_api_key="secret-token"), store)
    assert parsed["tool"] == "finish_investigation"
    assert state["guard"]["llm_attempts"] == 2
    assert state["guard"]["llm_successes"] == 0
    failed = [payload for kind, payload in store.events if kind == "llm.attempt_failed"]
    assert len(failed) == 2
    assert "secret-token" not in str(failed)


def test_retry_stops_when_the_attempt_budget_is_spent():
    model = ScriptModel([RuntimeError("down"), _ok()])
    state = _state()
    parsed = call_model(model, "lead", state, Settings(max_llm_calls=1), Store())
    assert parsed["tool"] == "finish_investigation"
    assert model.calls == 1
    assert state["guard"]["llm_attempts"] == 1
    assert state["stop_reason"] == "budget_llm_calls"


def test_second_call_stops_when_tokens_are_exhausted():
    state = _state()
    settings = Settings(max_llm_calls=4, max_total_tokens=150000)
    assert call_model(ScriptModel([_ok()]), "lead", state, settings, Store())["tool"] == "finish_investigation"
    settings.max_total_tokens = state["guard"]["tokens"]
    again = call_model(ScriptModel([_ok()]), "lead", state, settings, Store())
    assert again is None
    assert state["stop_reason"] == "budget_tokens"
    assert state["guard"]["llm_attempts"] == 1


def test_two_workers_share_one_attempt_budget():
    state = _state()
    settings = Settings(max_llm_calls=2)

    def worker():
        call_model(ScriptModel([_ok()]), "lead", state, settings, Store())

    threads = [threading.Thread(target=worker) for _ in range(2)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert state["guard"]["llm_attempts"] == 2
    assert state["guard"]["llm_successes"] == 2
    assert state["guard"]["llm_calls"] == 2


def _report(usage=None):
    return ({
        "summary": "ok",
        "observations": [{"id": "o1", "text": "seen"}],
        "hypotheses": [{"id": "h1", "text": "maybe", "layer": "unknown", "status": "undetermined"}],
    }, usage or {"source": "estimated"})


def _split(**overrides):
    values = {"max_llm_calls": 8, "max_revisions": 0, "max_total_tokens": 150000, "context_budget_tokens": 24000}
    values.update(overrides)
    return Settings(**values)


def test_investigation_cannot_spend_the_report_reserve():
    state = _state()
    settings = _split(max_llm_calls=6, max_revisions=1)
    for _ in range(2):
        assert call_model(ScriptModel([_ok()]), "lead", state, settings, Store())["tool"] == "finish_investigation"
    blocked = call_model(ScriptModel([_ok()]), "lead", state, settings, Store())
    assert blocked is None
    assert state["guard"]["llm_attempts"] == 2
    parsed = call_model(ScriptModel([_report()]), "write_report", state, settings, Store())
    assert parsed["summary"] == "ok"
    assert state["guard"]["llm_attempts"] == 3
    assert state["guard"]["report_attempts"] == 1
    assert state["guard"]["llm_calls"] == 3
    assert state["guard"]["llm_successes"] == 3


def test_report_attempts_stop_at_the_reserve():
    state = _state()
    settings = _split()
    assert call_model(ScriptModel([_report()]), "write_report", state, settings, Store())["summary"] == "ok"
    assert call_model(ScriptModel([_report()]), "write_report", state, settings, Store())["summary"] == "ok"
    blocked = call_model(ScriptModel([_report()]), "write_report", state, settings, Store())
    assert blocked is None
    assert state["stop_reason"] == "budget_llm_calls"
    assert state["failure_reason"] == "budget_llm_calls"
    assert state["guard"]["report_attempts"] == 2
    assert state["guard"]["llm_attempts"] == 2
    assert state["guard"]["llm_successes"] == 2


def test_report_tokens_stop_at_the_reserve_and_the_global_cap(monkeypatch):
    monkeypatch.setattr("alertflow_agent.runtime.model.estimate_tokens", lambda _text: 100)
    reserved = _state()
    settings = _split(context_budget_tokens=100, max_total_tokens=10000)
    assert call_model(ScriptModel([_report()]), "write_report", reserved, settings, Store())["summary"] == "ok"
    blocked = call_model(ScriptModel([_report()]), "write_report", reserved, settings, Store())
    assert blocked is None
    assert reserved["failure_reason"] == "budget_tokens"
    assert reserved["guard"]["report_attempts"] == 1
    global_cap = _state()
    tight = _split(context_budget_tokens=1000, max_total_tokens=150)
    assert call_model(ScriptModel([_report()]), "write_report", global_cap, tight, Store())["summary"] == "ok"
    blocked = call_model(ScriptModel([_report()]), "write_report", global_cap, tight, Store())
    assert blocked is None
    assert global_cap["failure_reason"] == "budget_tokens"
    assert global_cap["guard"]["llm_attempts"] == 1


def test_report_retries_count_and_two_callers_share_the_reserve():
    state = _state()
    settings = _split()
    parsed = call_model(
        ScriptModel([RuntimeError("down"), _report()]),
        "write_report", state, settings, Store(),
    )
    assert parsed["summary"] == "ok"
    assert state["guard"]["report_attempts"] == 2
    assert state["guard"]["llm_successes"] == 1
    assert state["guard"]["llm_calls"] == 2
    blocked = call_model(ScriptModel([_report()]), "write_report", state, settings, Store())
    assert blocked is None
    shared = _state()

    def worker():
        call_model(ScriptModel([_report()]), "write_report", shared, settings, Store())

    threads = [threading.Thread(target=worker) for _ in range(4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert shared["guard"]["report_attempts"] == 2
    assert shared["guard"]["llm_attempts"] == 2
    assert shared["guard"]["llm_successes"] == 2
