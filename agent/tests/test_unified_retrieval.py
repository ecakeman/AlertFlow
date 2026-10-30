"""批次5-1 验收：工具层的三个检索路径必须真的经 ContextManager.retrieve。

判定方式：让 retrieve 直接抛错，若工具仍成功或仍触发底层 client，就说明存在绕过。
"""

from __future__ import annotations

import json

import pytest

from alertflow_agent.config import Settings
from alertflow_agent.runtime.context_manager import ContextManager
from alertflow_agent.runtime.tools import run_tool


class Recorder:
    def __init__(self):
        self.calls = []

    def timeline(self, *args, **kwargs):
        self.calls.append(("timeline", args, kwargs))
        return {"events": []}

    def collect_checks(self, *args, **kwargs):
        self.calls.append(("checks", args, kwargs))
        return []

    def call(self, name, args):
        self.calls.append((name, args))
        return "{}"


def _state():
    return {
        "event": {"service_id": "api", "occurred_at": "2026-10-11T10:00:00Z", "event": "incident_start"},
        "evidence": {},
        "coverage": {"from": 0, "to": 0, "gaps": []},
        "run_id": "run-1",
    }


def _settings(**extra):
    base = dict(
        prometheus_mcp_url="http://prometheus.example/mcp",
        prometheus_service_selectors=json.dumps({"api": 'service="api"'}),
        loki_mcp_url="http://loki.example/stream",
        loki_service_selectors=json.dumps({"api": '{service="api"}'}),
    )
    base.update(extra)
    return Settings(**base)


def _block_retrieve(monkeypatch):
    calls = []

    async def boom(self, request):
        calls.append(request)
        raise RuntimeError("retrieve must be the only entry")

    monkeypatch.setattr(ContextManager, "retrieve", boom)
    return calls


def test_timeline_tool_goes_through_retrieve(monkeypatch):
    calls = _block_retrieve(monkeypatch)
    client = Recorder()
    result = run_tool("query_service_timeline", {}, _state(), _settings(), {"alertflow": client})
    assert result["status"] == "error"
    assert len(calls) == 1
    assert client.calls == []


def test_history_tool_goes_through_retrieve(monkeypatch):
    calls = _block_retrieve(monkeypatch)
    client = Recorder()
    result = run_tool("get_check_history", {}, _state(), _settings(), {"alertflow": client})
    assert result["status"] == "error"
    assert len(calls) == 1
    assert client.calls == []


def test_mcp_tool_goes_through_retrieve(monkeypatch):
    calls = _block_retrieve(monkeypatch)
    mcp = Recorder()
    result = run_tool("query", {"query": "up"}, _state(), _settings(), {"mcp": mcp})
    assert result["status"] == "error"
    assert len(calls) == 1
    assert mcp.calls == []
