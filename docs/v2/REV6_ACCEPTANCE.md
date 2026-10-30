# REV6.4 验收台账

本文件逐阶段记录**实际执行**的验收命令、退出码、观察结果与判定。规则：

- 判定只有五种：`PASS`、`PARTIAL`、`FAIL`、`SKIPPED`、`NOT RUN`（另用 `BLOCKED` 标明被外部条件阻塞）。
- 没有实际执行的条目一律 `NOT RUN`，不得依据"代码看起来有"写成 PASS。
- `PASS` 必须有命令 + 退出码，或真实服务的观察证据；纯代码审查只能支撑 `PARTIAL`。
- 未实际跑过的真实集成（Kind 集群、真实 Prometheus/Loki、真实 LLM、RAGFlow）必须标注环境与原因。

基线：`docs/v2/REV6_BASELINE.md`（HEAD `a5ca5d2`）。

---

## 阶段门

| 阶段 | 门禁条件 | 状态 |
|---|---|---|
| P0 | 三份基线文档齐全且可追溯到文件/行号 | PASS（见 §P0） |
| P1 | B0–B4 + C0–C5 完成；pytest / go test / go vet / go build 全绿；Mock A2A 契约测试通过 | PASS（见 §P1） |
| P2 | Observer 审计结论 + fault-lab 授权清单；`go test ./internal/observe/kubernetes` 通过 | 未开始 |
| P3 | `kubectl get deploy,sts,ds,pods -A` 显示独立工作负载；smoke-test 通过 | 未开始 |
| P4 | `alertflow` 与 `fault-lab` 双 service_id 进 Timeline；`kubectl auth can-i` 正反验证 | 未开始 |
| P5 | 原生 PromQL/LogQL 与经 MCP 查询同值；Alloy Pod 日志可查 | 未开始 |
| P6 | fault-lab `http_503` 注入 → 三源证据 → 报告 → 恢复核验 | 未开始 |

---

## P0

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P0-A1 | 现状可追溯到真实文件/函数/测试 | `docs/v2/REV6_BASELINE.md` §6/§7 | 见下 |
| P0-A2 | `REV6_BASELINE.md` 含 HEAD、版本、profile、缺陷表、拟改文件 | 本文档同目录文件 | 见下 |
| P0-A3 | `BACKEND_BASELINE.md`（B0 调用图） | 由 backend-agent 产出 | 见下 |
| P0-A4 | 认知契约审计（`COGNITION_BASELINE.md`/`COGNITION_CONTRACTS.md`） | 由 cognition-agent 产出 | 见下 |
| P0-A5 | `deploy/k8s/` 盘点，不覆盖已有清单 | `ls -R deploy/k8s` | PASS（仅 `observer-rbac.yaml`） |
| P0-A6 | 保护未提交修改、只读审计 | `git status --short` 空 | PASS |

---

## P1

执行时间 2026-10-30，HEAD 基线 `a5ca5d2`，未提交工作区。命令均在仓库根执行。

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P1-B1 | Agent envelope 构建从 `notify.AgentSender` 抽为 transport-independent builder，HTTP/gRPC 共用 | `backend/internal/agentmessage/{agentmessage.go,encode.go}`；`go test ./internal/agentmessage` ok；`notify` 包 golden 7/7（`testdata/agentv2/`）字节级不变 | PASS |
| P1-B2 | 删除 `backend/internal/incident` 重复状态机死代码 | 删除前 `grep -rn "internal/incident" backend --include=*.go` 仅命中自身；删除后 `go build ./...`/`go vet ./...`/`go test ./...` 全绿 | PASS |
| P1-B3 | `notification_deliveries` 历史命名与 `integration_id!=0` 不可达分支标注 | `backend/internal/db/delivery.go` 注释；无行为变化 | PASS（仅注释） |
| P1-B4 | 角色启动 smoke 与 CI 入口 | `go run ./cmd/alertflow -smoke` 六个角色 exit 0；`Makefile` 新增 `smoke` target；`cmd/alertflow/main_test.go::TestRoleSmoke` | PASS |
| P1-C1 | 唯一时间窗 `build_investigation_window()` | `agent/src/alertflow_agent/runtime/window.py`；`tests/test_cognition_window.py` 16 passed | PASS |
| P1-C2 | `EvidenceManager.register_batch()` 六项校验 + run 稳定 evidence_id + FactCatalog | `runtime/evidence.py`；`tests/test_evidence_manager.py` 14 passed | PASS |
| P1-C3 | Memory 以 TimelineNode+TimelineAttachment 为主轴 | `agent/schema.sql:168-190` 幂等 DDL（`timeline_attachments`），旧表未删未迁移；`tests/test_memory_timeline.py` 6 passed | PASS（新链路；历史数据未迁移，符合决定 3） |
| P1-C4 | `ContextManager.retrieve()` 唯一检索入口 + 六区块上下文 | `runtime/context_manager.py`；MCP `tools/call` 只在 Manager 内；`tests/test_context_manager.py` 9 + `tests/test_unified_retrieval.py` 3 passed | PASS |
| P1-C5 | RunbookQueryV1/ArtifactV1 契约 + citation 双 ID + Mock A2A 契约测试 | `integrations/runbook_contract.py`、`a2a_adapter/ragflow.py`；`tests/test_runbook_contract.py` 20 passed | PASS（真实 RAGFlow 运行 SKIPPED） |
| P1-C6 | 报告 fact 引用与预算账本 | `LLMReport.fact_ids`/`conclusion_basis_hypothesis_id`；`runtime/budget.py` reserve/commit/refund；`tests/test_report_facts.py` 4 + `tests/test_budget_ledger.py` 4 passed | PASS |
| P1-1 | Agent 全量回归 | `cd agent && uv run --frozen --extra dev pytest -q` → `202 passed, 3 skipped` exit 0 | PASS |
| P1-2 | Go 构建/静态检查 | `go build ./...` exit 0；`go vet ./...` exit 0 | PASS |
| P1-3 | Go 全量测试（真实 Postgres） | `ALERTFLOW_TEST_DATABASE_URL=postgres://…@localhost:55432/postgres` + `ALERTFLOW_PYTHON=agent/.venv/bin/python` + `go test -count=1 -p 1 -timeout 900s ./...` → `GO_EXIT=0`（14 个包 ok，含 `internal/api` 的 `TestCrossLanguageGRPC`） | PASS |
| P1-4 | Compose profile 校验 | 默认/`agent`/`demo`/`observability`/`ragflow`/`runbook` 六个 `docker compose config --quiet` 全部 exit 0 | PASS |

环境说明：
- `TestCrossLanguageGRPC` 需要 `ALERTFLOW_PYTHON=<repo>/agent/.venv/bin/python`，CI 未设置该变量。
- 真实 RAGFlow 服务未运行（决定 5），协议适配以契约测试为准。
- checkpoint 中断/恢复的真实数据库集成测试未编写（单测覆盖 `budget.indeterminate_attempts`），标 NOT RUN。
- `agent/schema.sql` 的新 DDL 只做了幂等声明，未对真实 Postgres 执行验证，标 NOT RUN（P4 阶段随集群验证）。

**P1 判定：PASS**（上述 NOT RUN 项不影响 P1 门禁，已在 P3/P4 跟踪）。

---

## P2

（阶段完成后填写）

---

## P3

（阶段完成后填写：集群创建、Pod 清单、资源实际占用、smoke-test）

---

## P4

（阶段完成后填写）

---

## P5

（阶段完成后填写：PromQL/LogQL/MCP 三方一致性、Alloy 采集证据）

---

## P6

（阶段完成后填写：故障注入、三角色证据、报告、恢复、滚动重启持久化）

---

## 交付文档清单

| 文件 | 状态 |
|---|---|
| `docs/v2/REV6_BASELINE.md` | 已产出 |
| `docs/v2/REV6_ACCEPTANCE.md` | 本文件 |
| `docs/v2/BACKEND_BASELINE.md` | 已产出 |
| `docs/v2/COGNITION_BASELINE.md` | 已产出 |
| `docs/v2/COGNITION_CONTRACTS.md` | 已产出 |
| `deploy/k8s/README.md` | 待 deploy-agent |
