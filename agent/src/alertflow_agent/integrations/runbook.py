"""One A2A consult per run. An empty URL does not construct a client."""

from __future__ import annotations

import asyncio
import json
import time


def consult(url: str, payload: dict, message_id: str, timeout: float = 30, token: str = "") -> dict:
    """一次 consult：payload 为 RunbookQueryV1；token 非空时带 Bearer 鉴权。"""
    text = json.dumps(payload, ensure_ascii=False)
    return asyncio.run(_send(url, text, message_id, timeout, token))


async def _send(url: str, text: str, message_id: str, timeout: float, token: str = "") -> dict:
    import httpx
    from a2a.client import ClientConfig, create_client
    from a2a.types.a2a_pb2 import GetTaskRequest, Message, Part, Role, SendMessageRequest, TaskState
    from a2a.utils.constants import TransportProtocol

    terminal = {
        TaskState.TASK_STATE_COMPLETED,
        TaskState.TASK_STATE_FAILED,
        TaskState.TASK_STATE_CANCELED,
    }
    headers = {"Authorization": f"Bearer {token}"} if token else None
    async with httpx.AsyncClient(timeout=timeout, headers=headers) as http:
        client = await create_client(
            url,
            client_config=ClientConfig(
                streaming=False,
                polling=False,
                httpx_client=http,
                supported_protocol_bindings=[TransportProtocol.HTTP_JSON],
                use_client_preference=True,
            ),
        )
        message = Message(
            message_id=message_id,
            role=Role.ROLE_USER,
            parts=[Part(text=text, media_type="application/json")],
        )
        task_id = ""
        async for event in client.send_message(SendMessageRequest(message=message)):
            if event.HasField("task") and event.task.id:
                task_id = event.task.id
        if not task_id:
            await client.close()
            raise RuntimeError("runbook agent returned no task")
        deadline = time.monotonic() + timeout
        task = None
        while True:
            task = await client.get_task(GetTaskRequest(id=task_id))
            if task.status.state in terminal:
                break
            if time.monotonic() >= deadline:
                await client.close()
                raise TimeoutError("runbook agent timed out")
            await asyncio.sleep(0.05)
        await client.close()
    if task is None or task.status.state != TaskState.TASK_STATE_COMPLETED:
        raise RuntimeError("runbook agent failed")
    if not task.artifacts or not task.artifacts[-1].parts:
        raise RuntimeError("runbook agent returned no artifact")
    body = json.loads(task.artifacts[-1].parts[0].text)
    if not isinstance(body, dict):
        raise RuntimeError("runbook agent returned an invalid artifact")
    if body.get("error"):
        # 保留适配器给出的错误语义（invalid / lookup failed），便于上游区分 gap
        raise RuntimeError(str(body["error"]))
    return body
