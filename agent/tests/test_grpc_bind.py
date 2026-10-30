"""AGENT_GRPC_ADDR 归一化：省略 host 的旧写法不能把 agent 启动打挂。"""

from __future__ import annotations

import pytest

from alertflow_agent import grpc_tasks
from alertflow_agent.config import Settings


class FakeServer:
    def __init__(self):
        self.ports = []
        self.started = False

    def add_insecure_port(self, addr):
        self.ports.append(addr)
        return 9000

    def start(self):
        self.started = True


def _fake_grpc(monkeypatch):
    server = FakeServer()
    monkeypatch.setattr(grpc_tasks.grpc, "server", lambda *args, **kwargs: server)
    monkeypatch.setattr(
        grpc_tasks.agent_pb2_grpc,
        "add_AgentTaskServiceServicer_to_server",
        lambda servicer, target: None,
    )
    return server


def test_default_addr_is_bindable():
    assert Settings().agent_grpc_addr == "0.0.0.0:9000"


@pytest.mark.parametrize("value,expected", [
    (":9000", "0.0.0.0:9000"),
    ("", "0.0.0.0:9000"),
    ("127.0.0.1:9000", "127.0.0.1:9000"),
    ("0.0.0.0:9000", "0.0.0.0:9000"),
])
def test_bind_addr_normalizes(value, expected):
    assert grpc_tasks.bind_addr(value) == expected


def test_serve_tasks_with_legacy_addr_does_not_raise(monkeypatch):
    server = _fake_grpc(monkeypatch)
    returned = grpc_tasks.serve_tasks(Settings(agent_grpc_addr=":9000"), None)
    assert returned is server
    assert server.ports == ["0.0.0.0:9000"]
    assert server.started is True
