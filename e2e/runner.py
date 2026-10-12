"""端到端 Runner：消费 Fault Lab 的场景目录，驱动全栈，逐条跑类型化断言。

引擎是**场景无关**的：它不知道"db-pool-exhaustion"是什么，只知道"注入 → 观测 → 按注册表
跑断言"。场景与期望都来自 Fault Lab 的目录，断言来自 checks 注册表。

边界：Runner 是独立进程，不属于 Fault Lab 运行时，也不属于 AlertFlow——它只是这三者的客户端。
"""

from __future__ import annotations

import json
import subprocess
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

from .checks import run_checks
from .models import Mode, Observed, ScenarioResult, SuiteReport


def sh(cmd: str) -> str:
    return subprocess.run(cmd, shell=True, capture_output=True, text=True).stdout.strip()


def call(method: str, url: str, headers=None, body=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        request.add_header(key, value)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read().decode("utf-8", "replace")
            return response.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as exc:
        return exc.code, None
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {exc}"


def _container(pattern: str) -> str:
    """按名字找容器，不写死项目前缀——合部署后 fault-lab 归 AlertFlow 的 compose 管。"""
    return sh(f"docker ps --filter name={pattern} --format '{{{{.Names}}}}' | head -1").strip()


class Stack:
    """三个系统的客户端 + 从容器里取令牌（Runner 跑在 WSL 上）。"""

    def __init__(self, fault_lab="http://localhost:8090", alertflow="http://localhost:8080",
                 agent="http://localhost:8000") -> None:
        self.fault_lab = fault_lab.rstrip("/")
        self.alertflow = alertflow.rstrip("/")
        self.agent = agent.rstrip("/")
        fl = _container("fault-lab") or "fault-lab-fault-lab-1"
        af = _container("postgres") or "alertflow-postgres-1"
        ag = _container("agent") or "alertflow-agent-1"
        self.fl_token = sh(f"docker exec {fl} printenv SIMULATOR_CONTROL_TOKEN")
        self.af_cookie = sh(f"docker exec {af} psql -U alertflow -d alertflow -Atc "
                            "\"select token from sessions order by expires_at desc limit 1\"")
        self.agent_token = sh(f"docker exec {ag} printenv AGENT_API_TOKEN")
        assert self.fl_token and self.af_cookie and self.agent_token, \
            f"无法取得令牌（容器: {fl} / {af} / {ag}）"

    # ---- 便捷方法 ----
    def catalog(self) -> list[dict]:
        _, data = call("GET", f"{self.fault_lab}/scenarios")
        return (data or {}).get("scenarios", []) if isinstance(data, dict) else []

    def inject(self, state: dict) -> int:
        status, _ = call("PUT", f"{self.fault_lab}/control/v1/state",
                         {"Authorization": f"Bearer {self.fl_token}"}, state)
        return status

    def reset(self) -> None:
        call("POST", f"{self.fault_lab}/control/v1/reset", {"Authorization": f"Bearer {self.fl_token}"}, {})

    def incidents(self) -> list[dict]:
        _, data = call("GET", f"{self.alertflow}/api/incidents?limit=50",
                       {"Cookie": f"upmonitor_session={self.af_cookie}"})
        return (data or {}).get("incidents", []) if isinstance(data, dict) else []

    def ongoing(self) -> dict | None:
        return next((inc for inc in self.incidents() if inc["status"] == "ongoing"), None)

    def runs(self) -> list[dict]:
        _, data = call("GET", f"{self.agent}/api/v1/runs", {"Authorization": f"Bearer {self.agent_token}"})
        return (data or {}).get("runs", []) if isinstance(data, dict) else []

    def run_events(self, run_id: str) -> list[dict]:
        _, data = call("GET", f"{self.agent}/api/v1/runs/{run_id}/events",
                       {"Authorization": f"Bearer {self.agent_token}"})
        return (data or {}).get("events", []) if isinstance(data, dict) else []

    def evidence(self, run_id: str) -> dict:
        _, data = call("GET", f"{self.agent}/api/v1/runs/{run_id}/evidence",
                       {"Authorization": f"Bearer {self.agent_token}"})
        return (data or {}).get("evidence", {}) if isinstance(data, dict) else {}

    def report(self, run_id: str) -> dict:
        _, data = call("GET", f"{self.agent}/api/v1/runs/{run_id}/report",
                       {"Authorization": f"Bearer {self.agent_token}"})
        return (data or {}).get("report", {}) if isinstance(data, dict) else {}

    def set_memory(self, enabled: bool) -> None:
        """切换 Agent 的记忆开关（通过 AlertFlow 的公开设置接口，不经手数据库）。"""
        headers = {"Cookie": f"upmonitor_session={self.af_cookie}"}
        _, current = call("GET", f"{self.alertflow}/api/settings", headers)
        agent_section = (current or {}).get("agent") or {}
        agent_section["memoryEnabled"] = enabled
        call("PUT", f"{self.alertflow}/api/settings", headers, {"agent": agent_section})


class Runner:
    def __init__(self, stack: Stack, poll_s: float = 5.0) -> None:
        self.stack = stack
        self.poll_s = poll_s

    def run(self, mode: Mode, only: list[str] | None = None, limit: int | None = None) -> SuiteReport:
        report = SuiteReport(mode=mode)
        catalog = self.stack.catalog()
        if only:
            catalog = [scenario for scenario in catalog if scenario["id"] in only]
        if limit:
            catalog = catalog[:limit]
        report.catalog_size = len(catalog)
        self.stack.set_memory(mode == "memory_on")

        for scenario in catalog:
            result = self._run_scenario(scenario, mode)
            report.results.append(result)
            print(f"[{mode}] {scenario['id']:24} {'PASS' if result.passed else 'FAIL'}", flush=True)
            for check in result.checks:
                if not check.passed:
                    print(f"        ✗ {check.name}: {check.detail}", flush=True)
        report.summarize()
        return report

    # ------------------------------------------------------------------ 一场景 ----

    def _run_scenario(self, scenario: dict, mode: Mode) -> ScenarioResult:
        expect = scenario.get("expect") or {}
        result = ScenarioResult(scenario_id=scenario["id"], name=scenario.get("name", ""),
                                tags=scenario.get("tags") or [],
                                expect_alert=bool((expect.get("alert") or {}).get("should_fire")))
        observed = result.observed
        try:
            self.stack.reset()
            self._wait_no_ongoing()
            before = {inc["id"] for inc in self.stack.incidents()}
            started = time.time()
            status = self.stack.inject(scenario["inject"])
            if status != 200:
                result.error = f"注入失败: HTTP {status}"
                return result

            if not (expect.get("alert") or {}).get("should_fire"):
                time.sleep(60)
                observed.alerted = any(inc["id"] not in before for inc in self.stack.incidents())
            else:
                incident, latency = self._wait_new_incident(before)
                if incident:
                    observed.alerted = True
                    observed.detection_latency_s = round(latency or (time.time() - started), 1)
                    observed.severity = incident.get("severity")
                    observed.incident_id = incident["id"]
                    self._collect_agent(observed, incident["id"])
                else:
                    observed.note = "超时未见告警"
        except Exception as exc:  # noqa: BLE001
            result.error = f"{type(exc).__name__}: {exc}"
        result.checks = run_checks(scenario, observed, mode)
        return result

    def _wait_no_ongoing(self, timeout: float = 130) -> None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            if not self.stack.ongoing():
                return
            time.sleep(self.poll_s)

    def _wait_new_incident(self, before: set, timeout: float = 180):
        started = time.time()
        while time.time() - started < timeout:
            for incident in self.stack.incidents():
                if incident["id"] not in before:
                    return incident, time.time() - started
            time.sleep(self.poll_s)
        return None, None

    def _collect_agent(self, observed: Observed, incident_id: int) -> None:
        deadline = time.time() + 300
        run = None
        while time.time() < deadline:
            run = next((r for r in self.stack.runs() if r.get("incident_id") == incident_id
                        and r.get("status") not in ("running", "queued", "pending")), None)
            if run:
                break
            time.sleep(self.poll_s)
        if not run:
            return
        observed.agent_status = run.get("status")
        observed.agent_tokens = (run.get("usage") or {}).get("tokens")

        report = self.stack.report(run["id"])
        observed.conclusion = report.get("conclusion")
        observed.hypotheses = report.get("hypotheses") or []
        observed.unknowns = report.get("unknowns") or []
        cited = set()
        for hypothesis in observed.hypotheses:
            cited.update(hypothesis.get("supporting_evidence_ids") or [])
            cited.update(hypothesis.get("weakening_evidence_ids") or [])
        if observed.conclusion:
            cited.update(observed.conclusion.get("evidence_ids") or [])
        observed.citation_count = len(cited)

        evidence = self.stack.evidence(run["id"])
        kinds = set()
        for item in evidence.values():
            data = item.get("data") if isinstance(item.get("data"), dict) else {}
            kind = data.get("kind") or item.get("kind")
            if kind and kind != "service_info":
                kinds.add(str(kind))
        observed.evidence_kinds = sorted(kinds)

        events = self.stack.run_events(run["id"])
        for event in events:
            if event.get("type") == "memory.recalled":
                payload = event.get("payload") or {}
                observed.prior_hits = int(payload.get("hits") or 0)
                observed.prior_block_present = bool(payload.get("has_profile")) or observed.prior_hits > 0
            elif event.get("type") == "context.built":
                payload = event.get("payload") or {}
                tokens = int(payload.get("tokens") or 0)
                observed.context_calls += 1
                observed.context_max_tokens = max(observed.context_max_tokens or 0, tokens)
                if payload.get("budget"):
                    observed.context_budget = int(payload["budget"])
                if payload.get("floor") is not None:
                    observed.context_floor = max(observed.context_floor or 0, int(payload["floor"]))
                if payload.get("over_floor"):
                    observed.context_over_floor = True
                if payload.get("trimmed"):
                    observed.context_trimmed = True
                for name in payload.get("dropped") or []:
                    if name not in observed.context_dropped:
                        observed.context_dropped.append(name)
                for name in payload.get("degraded") or []:
                    if name not in observed.context_degraded:
                        observed.context_degraded.append(name)
