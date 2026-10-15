"""异步回答工程师的排查问题。

聊天过去是同步代理：页面要等一次模型调用，Agent 掉线或模型慢就直接失败。现在问题先落到
chat_requests，由这条线程回答，答案经回调 outbox 回传。问题与答案都持久化，所以 Agent
重启后未回答的问题会被接着处理，而不是凭空消失。

回答仍然是**只读**的：chat.answer 没有工具、没有写操作，边界与一次性诊断完全一致。
"""

from __future__ import annotations

import logging
import threading

from alertflow_agent.callbacks import backoff_seconds, chat_callback, chat_failed_callback

log = logging.getLogger("alertflow_agent")

LEASE_SECONDS = 120


class ChatWorker:
    def __init__(self, settings, store, runner) -> None:
        self.settings = settings
        self.store = store
        self.runner = runner

    def run(self, stop: threading.Event) -> None:
        while not stop.is_set():
            try:
                handled = self.drain()
            except Exception:
                log.exception("chat worker failed")
                handled = 0
            stop.wait(0.2 if handled else self.settings.chat_poll_seconds)

    def drain(self) -> int:
        rows = self.store.claim_chat_requests(
            self.settings.chat_batch, LEASE_SECONDS, self.settings.chat_max_attempts,
        )
        answered = 0
        for row in rows:
            reply, evidence, error = self._answer(row)
            attempts = int(row.get("attempt_count") or 1)
            request_id = row["request_id"]
            incident_id = row.get("incident_id")
            if error is None:
                self.store.finish_chat_request(
                    request_id, "answered", reply=reply, evidence_ids=evidence,
                    callback=chat_callback(request_id, incident_id, reply, evidence),
                )
                answered += 1
            elif attempts >= self.settings.chat_max_attempts:
                self.store.finish_chat_request(
                    request_id, "failed", error=error,
                    callback=chat_failed_callback(request_id, incident_id, error),
                )
            else:
                self.store.finish_chat_request(
                    request_id, "retry_wait", error=error,
                    retry_after_seconds=backoff_seconds(attempts),
                )
        return answered

    def _answer(self, row: dict) -> tuple[str | None, list | None, str | None]:
        """返回 (reply, evidence_ids, error)；error 非空表示这次没答出来。"""
        settings = self.runner.settings
        if not settings.llm_api_key:
            # 没有模型就不该给出"回答"——如实失败，而不是回一句套话当作答案。
            return None, None, "the agent has no model configured"
        from alertflow_agent.runtime.chat import answer

        try:
            result = answer(
                settings, self.store,
                getattr(self.runner, "memory", None), getattr(self.runner, "model", None),
                int(row.get("incident_id") or 0),
                list(row.get("history") or []),
                str(row.get("message") or "")[:2000],
            )
        except Exception as exc:  # noqa: BLE001 - 失败要走重试，不是崩掉线程
            return None, None, type(exc).__name__
        return result.get("reply") or "", list(result.get("evidence_ids") or []), None
