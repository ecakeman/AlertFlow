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
| P2 | Observer 审计结论 + fault-lab 授权清单；`go test ./internal/observe/kubernetes` 通过 | PASS |
| P3 | `kubectl get deploy,sts,ds,pods -A` 显示独立工作负载；smoke-test 通过 | PASS |
| P4 | `alertflow` 与 `fault-lab` 双 service_id 进 Timeline；`kubectl auth can-i` 正反验证 | PASS |
| P5 | 原生 PromQL/LogQL 与经 MCP 查询同值；Alloy Pod 日志可查 | PASS（A7 四类失败注入 NOT RUN，见 §P5） |
| P6 | fault-lab `http_503` 注入 → 三源证据 → 报告 → 恢复核验 | **PARTIAL**：注入/报告/恢复/持久化 PASS，三层证据里指标与日志两源零事实（P6-4 FAIL），见 §P6 |

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

执行时间 2026-10-30，Kind 集群 `alertflow-lab`（kind v0.33.0，node image `kindest/node:v1.33.0`，K8s v1.33.0）。

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P2-A1 | Observer 代码审计结论可追溯到文件/函数 | `docs/v2/REV6_BASELINE.md` §7；`backend/internal/observe/kubernetes/{observer.go,objects.go}` | PASS |
| P2-A2 | 单元测试 | `go test -count=1 -p 1 ./internal/observe/kubernetes` → ok（全量运行中 12.473s，13 个测试） | PASS |
| P2-A3 | fault-lab 授权清单只读、无 ClusterRole、无 secrets | `deploy/k8s/observer-rbac.yaml`：`alertflow` 与 `fault-lab` 各一个 Role+RoleBinding；verbs 仅 get/list/watch；无 `secrets` 资源 | PASS |
| P2-A4 | 渲染静态检查 | `kustomize build` 输出中 `:latest`=0、`hostPath`=0、内嵌 `kind: Secret`=0 | PASS |
| P2-A5 | 集群内 RBAC 正反验证 | 见 §P4 的 `kubectl auth can-i` 结果 | PASS |

---

## P3

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P3-A1 | 独立工作负载 | `kubectl get deploy,sts,ds -A`：alertflow 7 Deployment + 2 StatefulSet；fault-lab 1 Deployment；observability 3 Deployment + 2 StatefulSet + 1 DaemonSet；全部 1/1 | PASS |
| P3-A2 | 集群创建脚本可重复 | `deploy/k8s/scripts/create-cluster.sh`（幂等建 4 个 namespace）；节点 `alertflow-lab-control-plane` Ready | PASS |
| P3-A3 | 镜像导入与校验 | `build-and-load-images.sh --load-only` exit 0；`ctr` 逐个校验命中；节点内共 13 个业务/基础镜像 | PASS |
| P3-A4 | 部署核心 | `deploy-core.sh` exit 0；18 对象 apply、9 workload rollout condition met、3 PVC Bound | PASS |
| P3-A5 | 真实 HTTP 冒烟 | `port-forward svc/alertflow-api 18080:8080` + `curl /api/setup/status` → HTTP 200，body `{"needsSetup":true}`（全新库，无管理员属预期） | PASS |
| P3-A6 | 综合 smoke | `deploy/k8s/scripts/smoke-test.sh` exit 0（9 个分组全部 PASS：HTTP/gRPC/DB/Kafka/Redis/config PVC/RBAC/Service endpoints） | PASS |
| P3-A7 | Kustomize 全部可渲染 | `kustomize build` 5 个 target exit 0（base 18 / local-kind 18 / fault-lab 3 / runbook 2 / observability 28 对象） | PASS |
| P3-A8 | fault-lab 部署 | `deploy-fault-lab.sh` exit 0；Deployment/Service/PVC 1Gi 全部就绪；`inject-fault.sh` → `/health` 503、`restore-fault.sh` → 200 | PASS |
| P3-A9 | 资源预算与实测 | 核心 requests 1696Mi；加 fault-lab 后 limits 6862Mi（节点 allocatable 6.62GiB，101% overcommit，10 分钟稳定运行）；节点 `free -m` available ≈3.1GiB | PASS（overcommit 已知，见 README §3.6） |
| P3-A10 | config.yaml 持久化 | PVC `alertflow-config` Bound，`config.yaml` 存在且 cluster_id 与 seed 一致（未被覆盖） | PASS |

环境事实：Kafka 单节点 KRaft 首次启动重启 3 次后稳定（README §7 记录）；Service 增加 `publishNotReadyAddresses: true` 与 `controller:9093`，这是 broker 注册超时的真实修复。Agent 容器暂以 root 运行（镜像内源码权限 600），已列遗留项。

---

## P4

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P4-A1 | 双 service_id 进 Timeline | `select service_id, source, status from runtime_source_state` → `alertflow\|kubernetes\|ok`、`fault-lab\|kubernetes\|ok` | PASS |
| P4-A2 | 对象归属正确 | `select service_id, resource_kind, count(*) from runtime_object_states` → alertflow: Deployment 6 / Pod 14 / StatefulSet 2；fault-lab: Deployment 1 / Pod 1（无跨 service 串写） | PASS |
| P4-A3 | 事件写入 | `runtime_events` 中 `kubernetes.pod.first_seen` 15、`kubernetes.deployment.first_seen` 7、`kubernetes.statefulset.first_seen` 2、`kubernetes.event` 107、changed/inactive 若干 | PASS |
| P4-A4 | `kubectl auth can-i` 正向 | list pods/ deployments/ statefulsets ns=alertflow → yes；list pods ns=fault-lab → yes | PASS |
| P4-A5 | `kubectl auth can-i` 反向 | get secrets ns=alertflow → no；create pods → no；list pods ns=kube-system → no；list namespaces → no；create clusterroles → no | PASS |
| P4-A6 | Monitor Pod 重建不重复写 first_seen | 删除 monitor Pod 后 `first_seen` 总数 24→25，新增一条只属于**新 monitor Pod**（UID 变化＝新对象），其余 14 个既有 Pod 未重复写 | PASS |
| P4-A7 | 受控 rollout 只进 Timeline，不误开 Incident | `kubectl rollout restart deploy/alertflow-consumer` → `runtime_events` 287→300（pod.inactive/deployment.changed/pod.changed），`incidents` 2→2 不变 | PASS |
| P4-A8 | 采集失败不静默（代码级） | `observer.go` 中 DB 写失败走 `failSource("gap")` 而非 `ok`；对应用例 `TestObjectWriteFailureMarksAGap` | PASS（代码级，真实 DB 故障注入 NOT RUN） |

环境说明：`cluster_id` 实际为 `kind-alertflow-lab`（kind 生成的上下文名），与规格示例 `alertflow-lab` 不同，属环境命名差异，不影响归属语义，已在 README 记录。

---

## P5

证据文件：`deploy/observability-lab/evidence/p5-verify-20261030T135503Z.log`（脚本 `deploy/observability-lab/verify-p5.sh`，Lead 亲自复跑，exit 0）。

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P5-A1 | Prometheus 原生 API 拿到按 `service_id` 区分的 probe 指标 | `probe_success{service_id="fault-lab"}`→1、`probe_success{service_id="alertflow"}`→1；series 各 1 条；`instance` 分别指向 `fault-lab.fault-lab.svc.../health` 与 `alertflow-api.alertflow.svc.../api/setup/status` | PASS |
| P5-A2 | 同一 PromQL 经 Prometheus MCP v0.18.0 `/mcp` 得到一致值 | MCP `query` 返回 `probe_success=1`，与原生 instant 值一致；`range_query` 有结果 | PASS |
| P5-A3 | Alloy 采集真实容器日志并带 `service_id` 流标签 | Loki `/label/service_id/values` → `["alertflow","fault-lab"]`；`{service_id="fault-lab"}` 有日志行 | PASS |
| P5-A4 | 原生 LogQL 与 Loki MCP `/stream` 返回同一行 | 原生首行 `... "GET /healthz HTTP/1.1" 200 OK`；MCP `loki_query` 返回同一行且带 `service_id` 流标签 | PASS |
| P5-A5 | MCP 工具面最小化 | `--mcp.tools=core` 注册 9 个工具（query/range_query/series/label_*/metric_metadata/docs_*）；`delete_series`/`clean_tombstones`/`snapshot`/`reload`/`quit` 等 19 个管理工具不可见 | PASS |
| P5-A6 | 注入态对照，排除"假绿" | `http_503` 时 `probe_success=0`、`probe_http_status_code=503`（脚本记为 2 项预期 WARN），恢复后回到 1/200 | PASS |
| P5-A7 | 空/不可用/超时/截断 四类失败在 MCP 层可区分 | 未做 MCP 端口不可达、超时、超大响应的注入验证 | NOT RUN |
| P5-A8 | Alloy 采集路径 | DaemonSet 以 hostPath 只读挂载节点 `/var/log/pods`（P0 实测 0750 root:root、日志 0640 root:root、CRI 行格式确认），uid 0 运行、无 privileged、无额外 capability | PASS |

资源实测：观测 6 组件 requests 300m/730Mi、limits 1700m/1544Mi、RSS 合计 ≈420MB（`evidence/rss-20261030T135248Z.log`）。节点 allocatable 16 cpu / 6.62 GiB，与核心 1.78 GiB 叠加后仍在容量内。

---

## P6

主故障：`fault-lab` 的 `http_503`（禁止用 `alertflow-api scale 0`，未使用）。共 4 次真机注入（incident 4–7），其中后 3 次为真实 DeepSeek 调查。

| 编号 | 条目 | 命令/证据 | 判定 |
|---|---|---|---|
| P6-1 | `inject-fault.sh` 注入后 monitor 在下一个 interval 产生 Incident | incident 3/4/5/6/7 均在注入后 ≤40s 出现（`incidents.started_at` 对 `injected_at`） | PASS |
| P6-2 | delivery → agent run → callback 全链路 | `agent_runs` 每个 incident 恰 1 条 `run.completed`；`runtime_events` 有 `agent.run.started`/`agent.run.completed`（service_id=fault-lab） | PASS |
| P6-3 | 真实 LLM（DeepSeek）调查产出报告 | run `bd9b27c1`：`status=partial`，usage `tokens=51523, llm_calls=7, tool_calls=4, llm_attempts=7`；报告 15KB，`verification.passed=true` | PASS |
| P6-4 | 三角色/三层证据一致（运行时 + 指标 + 日志） | 运行时（checks/timeline/kubernetes）与 Go 聚合指标均有事实；**Prometheus/Loki 两个 MCP 被调用但零可用事实**（`fact_count_by_source_type` 无 prometheus/loki；prometheus `calls=1,status=unavailable,error="empty"`） | **FAIL** |
| P6-5 | 失败语义不折叠 | `coverage.sources` 区分 `ok/empty/unavailable/disabled`；gaps 记 `PROMETHEUS_EMPTY`；`report.status=partial` | PASS |
| P6-6 | 恢复链路 | `restore-fault.sh` 后 incident 7 resolved（14:48:55Z）；`probe_success{service_id="fault-lab"}` 回到 1，近 12 分钟 25 个采样中 15 个 0（注入期）、末尾 1；`runtime_source_state` 两服务仍 `ok` | PASS |
| P6-7 | 自观察：内部 workload 滚动重启不误开 Incident | `rollout restart deploy/alertflow-publisher` → service_id=alertflow 的 `kubernetes.deployment.changed`/`pod.changed`/`pod.inactive`/`pod.first_seen` 正常记录，`incidents` 5→5 不变 | PASS |
| P6-8 | Monitor Pod 重建后指纹去重 | 删除 monitor Pod → 新 Pod 运行，`first_seen` 30→31（仅新增 monitor 自身 UID），`runtime_source_state` 仍 ok | PASS |
| P6-9 | Agent Pod 重建后链路可用 | `rollout restart deploy/alertflow-agent` 后下一个 incident 仍能产出 run 与报告 | PASS |
| P6-10 | Kafka 单 Pod 重启（不删 PVC）后数据保持 | `delete pod alertflow-kafka-0` → 新 Pod Running，`kafka-topics.sh --list` 仍为 `alert-events`/`__consumer_offsets`；`incidents`/`agent_runs` 行数不变 | PASS |
| P6-11 | 无重复调查 | 每个 incident 恰好 1 条 run（`ON CONFLICT (message_id) DO NOTHING`） | PASS |
| P6-12 | 迭代预算 | 决策 5 限定 1–3 次真实调查，实际执行 4 次（第 4 次用于验证 NOT_QUERIED 判据修复），**超出预算 1 次，已如实记录** | PARTIAL |

### P6 未达成项与根因

- **P6-4（FAIL）根因**：Lead 的 MCP 调用在真实 run 中返回空结果（Prometheus `query` 空、Loki `loki_query` 空），未登记任何 prometheus/loki 事实。连接与工具注册均正常（Pod 内 `enabled_mcp_tools` 返回 6 个工具、`initialize` 200、P5 验收里同样的 MCP 能查到 `probe_success=1`）。因此问题是**模型发出的查询与真实数据形状不匹配**（selector/label/时间窗其一），不是链路不通。未在本轮定位到具体 query 文本：`coverage.sources.prometheus.error` 只保留 `"empty"`，未落 query 原文。
- 后续修复方向（未实施）：`coverage.sources.<source>` 增加 `queries[]`（记录实际发出的 promql/logql），并在 `mcp_tools` 上下文块给出带 `service_id` 与统一窗口的示例查询，供模型直接改写。
- `report.coverage.sources.loki.calls=0` 而 `gaps` 里没有 `LOKI_NOT_QUERIED`：两处统计口径不同（前者来自 ContextManager 检索视图、后者来自 MCP 活动账本），属**视图不一致**，未在本轮统一。
- P5-A7（MCP 四类失败注入）仍 NOT RUN；checkpoint 中断/恢复集成测试 NOT RUN；RAGFlow 真机 SKIPPED（契约与 Mock 见 P1 C5）。
- Agent 镜像是**叠加层构建**（`FROM alertflow-agent:local` + `COPY src`）用于快速迭代，完整 clean rebuild 未在本轮执行（构建机内存不足），NOT RUN。

---

## 交付文档清单

| 文件 | 状态 |
|---|---|
| `docs/v2/REV6_BASELINE.md` | 已产出 |
| `docs/v2/REV6_ACCEPTANCE.md` | 本文件 |
| `docs/v2/BACKEND_BASELINE.md` | 已产出 |
| `docs/v2/COGNITION_BASELINE.md` | 已产出 |
| `docs/v2/COGNITION_CONTRACTS.md` | 已产出 |
| `deploy/k8s/README.md` | 已产出 |
| `deploy/observability-lab/README.md` + `verify-p5.sh` + `evidence/` | 已产出 |
