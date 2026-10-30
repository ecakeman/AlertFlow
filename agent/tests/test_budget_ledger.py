"""批次5-3 预算账本：reserve/commit/refund/resume 的一致性。

核心不变量：一次 attempt 只记一次；失败/超时照样消耗；恢复时不重复记账。
"""

from __future__ import annotations

from alertflow_agent.config import Settings
from alertflow_agent.runtime import budget
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


def _ok():
    return ({"tool": "finish_investigation", "args": {}, "text": ""}, {"source": "estimated"})


def test_consecutive_failed_attempts_are_counted_against_the_cap():
    state = _state()
    model = ScriptModel([RuntimeError("down"), RuntimeError("still down")])
    assert call_model(model, "lead", state, Settings(max_llm_calls=4), Store())["tool"] == "finish_investigation"
    assert state["guard"]["llm_attempts"] == 2
    assert state["guard"]["llm_successes"] == 0


def test_timeout_consumes_exactly_one_attempt():
    state = _state()
    model = ScriptModel([TimeoutError("provider slow")])
    assert call_model(model, "lead", state,
                      Settings(max_llm_calls=1), Store())["tool"] == "finish_investigation"
    assert model.calls == 1
    assert state["guard"]["llm_attempts"] == 1
    assert state["guard"]["llm_successes"] == 0
    assert state["guard"]["tokens"] > 0
    assert state["guard"].get("llm_inflight") is None


def test_resume_marks_the_inflight_attempt_indeterminate_without_recounting():
    guard = fresh_guard()
    guard["llm_attempts"] = 1
    guard["llm_calls"] = 1
    guard["tokens"] = 500
    budget.begin_call(guard, "lead", 500)
    # 模拟崩溃后恢复：进入下一次调用前先收敛
    state = {"run_id": "run-resume", "event": {}, "guard": guard}
    assert call_model(ScriptModel([_ok()]), "lead", state, Settings(max_llm_calls=4), Store())["tool"] == "finish_investigation"
    assert guard["indeterminate_attempts"] == 1
    assert guard["indeterminate"] is True
    assert guard["llm_attempts"] == 2  # 旧 attempt 保留 + 新 attempt 一次
    assert "llm_inflight" not in guard


def test_refund_only_returns_pre_delivery_reservations():
    state = _state()
    guard = state["guard"]
    settings = Settings(max_llm_calls=4)
    assert budget._reserve(guard, settings, "lead", 100) is None
    assert guard["llm_attempts"] == 1
    budget.refund(guard, settings, "lead", 100)
    assert guard["llm_attempts"] == 0
    assert guard["tokens"] == 0
    budget.refund(guard, settings, "lead", 100)  # 幂等，不出现负数
    assert guard["llm_attempts"] == 0
    assert guard["tokens"] == 0
