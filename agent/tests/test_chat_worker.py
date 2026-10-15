from alertflow_agent.chat_worker import ChatWorker
from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore
from tests.fakes import event_row


class StubModel:
    def __init__(self, reply="看 chk:11，是探针侧返回了 503。"):
        self.reply = reply

    def answer(self, prompt: str) -> str:
        return self.reply


class StubRunner:
    """只提供 chat worker 会用到的东西：settings / model / memory。"""

    def __init__(self, settings, model=None, memory=None):
        self.settings = settings
        self.model = model or StubModel()
        self.memory = memory


def _settings(**overrides):
    base = dict(agent_callback_token="cb", alertflow_base_url="http://af", llm_api_key="k")
    base.update(overrides)
    return Settings(**base)


def test_enqueue_chat_request_is_idempotent():
    store = MemoryStore()
    row = {"request_id": "chat-1", "incident_id": 1, "message": "why?", "history": []}
    assert store.enqueue_chat_request(row) is True
    assert store.enqueue_chat_request(row) is False
    assert len(store.chat_rows()) == 1


def test_chat_worker_answers_and_queues_the_callback():
    store = MemoryStore()
    store.insert_run(event_row("agent"))
    settings = _settings()
    store.enqueue_chat_request({"request_id": "chat-1", "incident_id": 1, "message": "为什么？", "history": []})

    worker = ChatWorker(settings, store, StubRunner(settings))
    assert worker.drain() == 1

    row = store.get_chat_request("chat-1")
    assert row["status"] == "answered"
    assert "探针" in row["reply"]

    callbacks = store.callback_rows()
    assert len(callbacks) == 1
    assert callbacks[0]["message_type"] == "chat.completed"
    assert callbacks[0]["payload"]["payload"]["request_id"] == "chat-1"
    assert callbacks[0]["payload"]["payload"]["reply"] == row["reply"]


def test_chat_worker_fails_without_a_model():
    store = MemoryStore()
    store.insert_run(event_row("agent"))
    settings = _settings(llm_api_key="", chat_max_attempts=1)
    store.enqueue_chat_request({"request_id": "chat-2", "incident_id": 1, "message": "?", "history": []})

    worker = ChatWorker(settings, store, StubRunner(settings))
    assert worker.drain() == 0

    row = store.get_chat_request("chat-2")
    assert row["status"] == "failed"
    assert "model" in row["error"]
    assert store.callback_rows()[0]["message_type"] == "chat.failed"


def test_chat_worker_retries_then_gives_up():
    store = MemoryStore()
    settings = _settings(chat_max_attempts=2)

    class Boom:
        def answer(self, prompt):
            raise RuntimeError("upstream 500")

    store.enqueue_chat_request({"request_id": "chat-3", "incident_id": 1, "message": "?", "history": []})
    worker = ChatWorker(settings, store, StubRunner(settings, model=Boom()))

    # 第一次失败：进入 retry_wait，还没有回调。
    assert worker.drain() == 0
    assert store.get_chat_request("chat-3")["status"] == "retry_wait"
    assert store.callback_rows() == []

    # 退避还没到期，先把时间推过去（测试里直接改内存行，不真的等）。
    for row in store.chats:
        row["next_attempt_at"] = 0.0

    # 第二次仍然失败：到达上限，落 failed 并排队回调。
    assert worker.drain() == 0
    assert store.get_chat_request("chat-3")["status"] == "failed"
    assert store.callback_rows()[0]["message_type"] == "chat.failed"


def test_chat_worker_is_idle_with_nothing_queued():
    settings = _settings()
    worker = ChatWorker(settings, MemoryStore(), StubRunner(settings))
    assert worker.drain() == 0
