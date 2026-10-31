"""P4：Chat 与 Lead 共用同一套上下文契约（六区块），而不是另拼一段字符串。"""

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from alertflow_agent.runtime import chat
from alertflow_agent.runtime.context import SECTION_ORDER, build_chat_bundle
from tests.fakes import event_row

_TITLES = {
    "## current_task": "current_task",
    "## service_timeline": "service_timeline",
    "## current_evidence": "current_evidence",
    "## historical_experience": "historical_experience",
    "## expert_knowledge": "expert_knowledge",
    "## investigation_state": "investigation_state",
}


def _settings(**overrides):
    base = dict(alertflow_grpc_target="alertflow:9090", alertflow_agent_grpc_token="t", llm_api_key="k")
    base.update(overrides)
    return Settings(**base)


def _store_with_run():
    store = MemoryStore()
    row = event_row("agent")
    store.insert_run(row)
    store.save_report(row["id"], 1, {
        "summary": "probe 503",
        "hypotheses": [{"title": "h1", "supporting_evidence_ids": ["chk:11"]}],
        "conclusion": {"text": "probe", "evidence_ids": ["chk:11"]},
    })
    return store, row["id"]


def _sections(text):
    return [title for title in _TITLES if title in text]


def test_chat_bundle_uses_the_same_six_sections_in_the_same_order():
    store, _ = _store_with_run()
    text, _ = chat.build_prompt(_settings(), store, None, 1, [], "why 503?")
    sections = _sections(text)
    assert "## current_task" in sections
    assert "## current_evidence" in sections
    order = [SECTION_ORDER.index(_TITLES[title]) for title in sections]
    assert order == sorted(order), sections


def test_chat_prompt_uses_the_chat_node_prompt_not_the_lead_one():
    """chat.md 与 lead.md 各自成块：Chat 拿到的是解释型提示词，不是调查型提示词。"""
    from alertflow_agent.runtime.context import _read
    assert "answering an engineer's question" in _read("prompts/chat.md")
    assert "answering an engineer's question" not in _read("prompts/lead.md")
    store, _ = _store_with_run()
    text, _ = chat.build_prompt(_settings(), store, None, 1, [], "why?")
    assert "investigating one monitoring incident" not in text


def test_chat_history_and_question_land_in_the_context():
    store, _ = _store_with_run()
    text, _ = chat.build_prompt(
        _settings(), store, None, 1,
        [{"role": "user", "text": "先看探针"}, {"role": "assistant", "text": "探针 503"}],
        "那恢复了吗？")
    assert "那恢复了吗？" in text
    assert "先看探针" in text and "探针 503" in text


def test_chat_without_a_run_does_not_crash():
    text, cited = chat.build_prompt(_settings(), MemoryStore(), None, 99, [], "?")
    assert "## current_task" in text
    assert cited == []


def test_chat_prior_block_marks_conclusions_as_past_judgements():
    store, _ = _store_with_run()

    class Repo:
        def get_profile(self, service_id):
            return {"baseline": "p95<300ms"}

        def list_recent(self, service_id, days, limit):
            return [{"id": "ep-1", "started_at": "2026-10-10T00:00:00Z", "conclusion": {"text": "上次是进程崩溃"}}]

    text, _ = chat.build_prompt(_settings(), store, Repo(), 1, [], "?")
    assert "past judgements" in text
    assert "上次是进程崩溃" in text


def test_build_chat_bundle_returns_a_fit():
    fit = build_chat_bundle({"event": {"id": 1, "event": "incident_start", "service_id": "api"},
                             "evidence": {}, "question": "?"})
    assert fit.text and fit.tokens > 0 and fit.budget > 0


def test_cited_evidence_comes_from_the_stored_report():
    store, _ = _store_with_run()
    _, cited = chat.build_prompt(_settings(), store, None, 1, [], "?")
    assert "chk:11" in cited
