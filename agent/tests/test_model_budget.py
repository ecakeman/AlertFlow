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
