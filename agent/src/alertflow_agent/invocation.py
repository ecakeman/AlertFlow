import uuid

from google.protobuf.json_format import MessageToDict
from google.protobuf.struct_pb2 import Struct

from alertflow_agent.gen.alertflow.integration.v1 import invocation_pb2, invocation_pb2_grpc

NAMESPACE = uuid.UUID("6f0c1a4e-1b3a-4b8e-9c1a-0000000000aa")


def invocation_id_for(task_id: str) -> str:
    return str(uuid.uuid5(NAMESPACE, task_id))


class AgentInvocationClient:
    def __init__(self, channel):
        self.stub = invocation_pb2_grpc.AgentInvocationServiceStub(channel)

    async def invoke(self, state: dict) -> dict:
        payload = Struct()
        payload.update({
            "schemaVersion": "1.0",
            "invocationId": invocation_id_for(state["task_id"]),
            "skill": "knowledge-retrieval",
            "knowledgeSpaceIds": ["operations-manual"],
            "query": state.get("service_name") or "故障现象",
            "context": {
                "serviceName": state.get("service_name") or "",
                "episodeSummary": f"输入版本 {state.get('input_version_from')}-{state.get('input_version_to')}",
                "knownFacts": [str(item.get("content"))[:300] for item in (state.get("timeline_records") or [])[:4]],
            },
            "constraints": {"maxHops": 3, "topK": 8, "requireCitations": True},
        })
        response = await self.stub.InvokeAgent(invocation_pb2.InvokeAgentRequest(
            invocation_id=invocation_id_for(state["task_id"]),
            target_agent_id="knowledge-agent",
            skill_id="knowledge-retrieval",
            input=payload,
            timeout_ms=30000,
        ), timeout=35, metadata=(("x-caller-agent", "alertflow-main-agent"),))
        if response.status != "COMPLETED":
            raise RuntimeError(response.safe_message or response.error_code or response.status)
        result = MessageToDict(response.result)
        if "results" not in result:
            raise RuntimeError("knowledge result schema is invalid")
        return result
