import json
import os
import shutil
import subprocess
from pathlib import Path


def _compose(tmp: Path, dotenv: str, shell: dict) -> str:
    shutil.copy(Path(__file__).resolve().parents[2] / "docker-compose.yml", tmp / "docker-compose.yml")
    if dotenv:
        (tmp / ".env").write_text(dotenv)
    env = os.environ.copy()
    for key in ("PROMETHEUS_MCP_URL", "LOKI_MCP_URL", "RAGFLOW_BASE_URL", "A2A_ADAPTER_URL"):
        env.pop(key, None)
    env.update(shell)
    completed = subprocess.run(
        ["docker", "compose", "--profile", "agent", "config", "--format", "json"],
        cwd=tmp,
        env=env,
        check=True,
        capture_output=True,
        text=True,
    )
    agent = json.loads(completed.stdout)["services"]["agent"]["environment"]
    return agent["PROMETHEUS_MCP_URL"]


def test_shell_overrides_dotenv(tmp_path):
    got = _compose(tmp_path, "PROMETHEUS_MCP_URL=http://from-dotenv/mcp\n", {"PROMETHEUS_MCP_URL": "http://from-shell/mcp"})
    assert got == "http://from-shell/mcp"


def test_dotenv_overrides_the_empty_default(tmp_path):
    got = _compose(tmp_path, "PROMETHEUS_MCP_URL=http://from-dotenv/mcp\n", {})
    assert got == "http://from-dotenv/mcp"


def test_unset_shell_and_dotenv_keep_the_empty_default(tmp_path):
    assert _compose(tmp_path, "", {}) == ""
