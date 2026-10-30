# 实施基线

日期：2026-10-30。只读审计，不改产品代码。

唯一实施规格是 `AlertFlow_V2_Cursor_Final_Spec.md`。本文件记录当前仓库相对该规格的现状。P0 之后才进入 P1。

## 当前提交

- 分支：`main`
- `HEAD`：`169ccabcc8881e88c166a3a05ea6bc29c8c6c818`
- `origin/main`：同一提交
- 工作区：审计开始时干净
- 本机 Go：`go1.26.6`。`backend/go.mod` 声明 `go 1.25.7`
- 本机 venv 已解析：`langgraph 1.2.11`、`langgraph-checkpoint-postgres 3.1.2`、`mcp 1.30.0`、`a2a-sdk 1.3.0`、`pydantic 2.13.4`
- `agent/pyproject.toml` 仍是 `langgraph>=0.2`，没有 lock 文件。Docker 镜像执行 `pip install -e .`，不消费 lock。P0 不创建 lock

## 已实现

- Gin `v1.10.1`，gRPC Go `v1.80.0`，client-go `v0.35.9`。检查完成与 outbox 同事务；consumer 用 `event.Decide` 开闭 Incident。Kubernetes 观察不调用 `event.Decide`。
- Timeline 三列时间：`occurred_at`、`observed_at`、`recorded_at`。检查驱动的开闭在 `backend/internal/db/apply.go` 的 `applyCheckTx`。
- Agent 主图是单 Lead 循环，见 `Runner._graph`（`agent/src/alertflow_agent/runtime/graph.py`）。节点顺序是 `bootstrap` → `collect_alertflow` → `collect_diagnostics` → `analyze` → `recall` → `select_skills` → `initial_plan` → `lead_model` / `lead_tools` / `replan` → `write_report` → `verify` → `persist`。没有 Runtime、Metrics、Logs 三个 Specialist，也没有并行 fan-out。
- `delegate_history_review` 是历史复核子流程，不是核心 Multi-Agent。`consult_runbook_agent` 和 Compose 的 `multi-agent` profile 只覆盖外部 A2A Runbook。
- Checkpointer：未传时 `Runner._graph` 使用 `MemorySaver`；`agent/src/alertflow_agent/main.py` 的 `_checkpointer` 在配置了 Agent 数据库时使用 Postgres checkpointer。当前图没有并行分支 reducer。
- MCP：`ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` 的 `/mcp`，Loki MCP 本地镜像 `v0.6.0` 的 `/stream`。即时查询的文本结果 `{"result","warnings"}` 不再被当成空结果（`integrations/mcp.py` 的 `_is_empty_payload`）。
- RAGFlow 镜像锁定 `infiniflow/ragflow:v0.27.3`。Valkey 在 Compose 网络 `ragflow` 上，主机名 `redis` 不与 AlertFlow Redis 冲突。
- A2A SDK `1.3.0`。客户端 `SendMessage` 之后轮询 `GetTask`（`integrations/runbook.py`）。Artifact MIME 是 `application/json`。没有成对 document id 和 chunk id 时 `citations_available` 为 false（`a2a_adapter/ragflow.py` 的 `parse_completion`）。
- Goose 迁移 `00001`–`00019`。`00018` 是 `a2a_adapter_tasks`，`00019` 是 `a2a_adapter_task_aliases`。

## 部分实现

- Observer 只映射 `WorkloadKind == "Deployment"`（`targetsOf`）。Informer 是 Deployment、ReplicaSet、Pod。没有 StatefulSet。`ApplyKubernetesObject` / `ApplyKubernetesNotice` 的错误被 `_ =` 丢掉，之后仍可能把 source 标成可用。
- `Connect` 失败只记下 `connErr`。之后 `Sync` 在 `client == nil` 时把 source 标成 `unavailable` 并返回，不会再次 `Connect`。连接失败会停在该状态，直到进程再次调用 `Connect`。
- A2A `claim` 写入 `working`，但 `RunbookExecutor.execute` 在返回前同步调用 `research`。重复 `message_id` 如果还不是 `completed` / `failed` / `canceled`，代码把它当成 `completed` 返回（`server.py` 里 `status = ... else "completed"`）。这不能证明执行中的任务会保持 `working`。
- `runtime/verify.py` 识别 `prometheus`、`loki`、`project_diagnostic`。没有 `source_type=runbook` 的 fact catalog 分支。
- `call_model` 最多调用两次 `model.complete`，但 `guard["llm_calls"]` 只在解析成功后增加。失败请求和失败后的重试不计入 `MAX_LLM_CALLS`。

## 未实现

- 固定的 Runtime、Metrics、Logs 三 Specialist，以及 Supervisor 并行调度。主图没有这条路径。
- `deploy/k8s/`。没有 Kind 或 Kustomize 基线。
- Kubernetes 里的 Prometheus、Blackbox、Alloy、Loki。Compose 的 Alloy 仍挂 Docker socket，不能采集 Kind Pod 日志。
- Fault Lab 端到端闭环。Sibling 仓库在 `/home/sancho/projects/fault-lab`。`http_503` 在 `fault_lab/schemas.py` 和 `routes_health.py`。恢复接口是 `POST /control/v1/reset`。本阶段没有启动它，P7 不能标成已验收。
- Python lock，以及 Docker / CI / 本地安装使用同一 lock。
- Compose profile 名仍是 `multi-agent`，不是规格要求的 `runbook`。`scripts/up-agent-ragflow.sh` 和 `scripts/up-all.sh` 仍传 `--profile multi-agent`。
- `deploy/ragflow/README.md` 末尾仍写「本阶段不增加 investigation tool 或 A2A adapter」，与已有的 `consult_runbook_agent` 和 `a2a_adapter` 矛盾。

## 事件时间

检查事件的投递快照在 `insertDeliveriesTx`。`apply_test.go` 的断言是：恢复快照 `StartedAt=1000`、`OccurredAt=1100`、`ResolvedAt=1100`。

- `incident_start`：`occurred_at` 和 `started_at` 都是这次检查的 `OccurredAt`。没有 `resolved_at`。
- `incident_resolve`：`occurred_at` 是恢复这次检查的时间，`started_at` 是原先打开 Incident 的时间，`resolved_at` 等于这次的 `occurred_at`。
- Timeline 行的 `occurred_at` 与 `observed_at` 都写成检查的 `OccurredAt`；`recorded_at` 是写入时的 `now`。

P3 的调查窗口应以上面两个事件的字段为输入。`incident_start` 的窗口下界是 `started_at`（与 `occurred_at` 相同）。`incident_resolve` 的下界是 `started_at`，上界是 `resolved_at`。缺任一端点时记 gap，不用 Agent 执行时间填上。P0 不改测试。

## 配置优先级

`scripts/up-agent-observability.sh` 和 `scripts/up-agent-ragflow.sh` 在变量未设置时 `export` 一个网络内 URL，然后再跑 Compose。Compose 里显式 shell 环境优先于根目录 `.env`，所以脚本导出的默认值会盖住 `.env` 里的自定义 URL。空 URL 仍不创建 MCP 或 A2A 客户端。P0 不改脚本。

`agent` 和 `a2a-adapter` 都加入了 `ragflow` 网络。主 Agent 只通过 `A2A_ADAPTER_URL` 找适配器，不直接调用 RAGFlow。适配器需要同时在默认网络和 `ragflow` 网络上。Agent 自己加入 `ragflow` 网络不是当前调用所必需的。留给 P1 核对，P0 不改 Compose。

## P1 缺陷核对

| 项 | 现状 | P0 动作 |
| --- | --- | --- |
| A2A working 重复请求与同步等待 | 未满足，见上文 | 只记录 |
| Runbook 进入 fact catalog | `verify.py` 无 runbook 分支 | 只记录 |
| RAGFlow README 旧句 | 仍在 | 只记录 |
| MCP 缺 `data` 被当成空 | 已修 | 不重复改 |
| `up` 不等于目标健康 | 代码未禁止把 `up` 当健康结论 | 只记录 |
| Loki 空结果与未配置分开 | 已有 `empty` / `unmapped` 等 kind | 不重复改 |
| 脚本默认 URL 盖住 `.env` | 仍在 | 只记录 |
| profile 改名 `runbook` | 仍叫 `multi-agent` | 只记录 |
| LLM attempt 记账 | 成功后才计数 | 只记录 |
| Python lock | 没有 | 只记录 |
| Redis 网络隔离 | `ragflow` 网络已分开 | 不重复改 |

## P2–P7 入口

- P2：`backend/internal/observe/kubernetes/observer.go` 的 `targetsOf`、`Connect`、`Sync`，以及 `ApplyKubernetesObject` 的错误处理。测试在 `observer_test.go`。
- P3：`agent/src/alertflow_agent/runtime/graph.py` 的 `Runner._graph`。新增三个 Specialist 子图，不把 `delegate_history_review` 改名充数。
- P4：新建 `deploy/k8s/`。当前目录不存在，没有可复用清单。
- P5：同一 Observer 对真实 Kind API。fake client 测试不能当端到端。
- P6：集群内 Blackbox → Prometheus MCP，Pod 日志 → Alloy → Loki MCP。不能沿用 Compose 的 Docker socket。
- P7：`/home/sancho/projects/fault-lab` 的 `http_503` 与 `POST /control/v1/reset`。本阶段未运行，状态是 `NOT RUN`，不是 `BLOCKED`（目录存在）。

## 本阶段不改的范围

产品代码、Compose、脚本、依赖、部署文件和测试都没有改。没有进入 P1。
