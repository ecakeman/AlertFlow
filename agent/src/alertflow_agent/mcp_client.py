import httpx


class McpClient:
    """连接 Java MCP Gateway。目录为空时调用方仍可继续诊断。"""

    def __init__(self, endpoint: str, token: str, client: httpx.AsyncClient):
        self.endpoint = endpoint
        self.token = token
        self.client = client
        self._id = 0

    async def list_tools(self) -> list[dict]:
        await self._rpc("initialize", {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "alertflow-main-agent", "version": "0.1.0"}})
        listed = await self._rpc("tools/list", {})
        tools = []
        for item in listed.get("tools") or []:
            name = item.get("name") or ""
            if "." in name:
                tools.append({"name": name, "description": item.get("description") or ""})
        return tools

    async def call_tool(self, name: str, arguments: dict) -> str:
        result = await self._rpc("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            raise RuntimeError("mcp tool failed")
        content = result.get("content") or []
        return content[0].get("text", "") if content else ""

    async def _rpc(self, method: str, params: dict) -> dict:
        self._id += 1
        response = await self.client.post(self.endpoint, headers={"Authorization": f"Bearer {self.token}"}, json={
            "jsonrpc": "2.0", "id": self._id, "method": method, "params": params,
        })
        response.raise_for_status()
        body = response.json()
        if body.get("error"):
            raise RuntimeError(body["error"].get("message") or "mcp error")
        return body.get("result") or {}
