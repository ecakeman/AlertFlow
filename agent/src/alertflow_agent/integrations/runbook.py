"""One A2A consult per run. An empty URL does not construct a client."""

from __future__ import annotations

import asyncio
import json


def consult(url: str, payload: dict, message_id: str, timeout: float = 30) -> dict:
    text = json.dumps(payload, ensure_ascii=False)
    return asyncio.run(_send(url, text, message_id, timeout))


async def _send(url: str, text: str, message_id: str, timeout: float) -> dict:
    import httpx
    from a2a.client import ClientConfig, create_client
    from a2a.types.a2a_pb2 import Message, Part, Role, SendMessageRequest
    from a2a.utils.constants import TransportProtocol

    async with httpx.AsyncClient(timeout=timeout) as http:
        client = await create_client(
            url,
            client_config=ClientConfig(
                streaming=False,
                polling=True,
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
        request = SendMessageRequest(message=message)
        artifact = None
        async for event in client.send_message(request):
            if event.HasField("task") and event.task.artifacts:
                artifact = event.task.artifacts[-1]
            if event.HasField("artifact_update"):
                artifact = event.artifact_update.artifact
        await client.close()
    if artifact is None or not artifact.parts:
        raise RuntimeError("runbook agent returned no artifact")
    return json.loads(artifact.parts[0].text)
