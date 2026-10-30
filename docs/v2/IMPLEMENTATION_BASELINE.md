# ⚠️ 历史文档（已作废）

> 本文档基于 Rev6 及更早的 `AlertFlow_V2_Cursor_Final_Spec.md`，其中「P3 新增三个 Specialist / Supervisor fan-out」与 Rev6.4 §2.6、§13 直接冲突。
> **当前规划依据是** `docs/v2/REV6_BASELINE.md`。本文只作历史记录保留，不得作为实施或验收依据。

---

# 实施基线

日期：2026-10-30。P0.1 只读补充。不改产品代码。

唯一实施规格是 `AlertFlow_V2_Cursor_Final_Spec.md`。

## P1 已改

下面各节是相对产品基线 `169ccab` 的审计，保留作对照。P1 之后这些缺陷已改，验收编号在 `docs/v2/IMPLEMENTATION_ACCEPTANCE.md`。

- A2A：`RunbookExecutor.execute` 先保存 `working` Task，再在当前进程异步调用 RAGFlow。重复请求和 `read_task` 在 deadline 过后或进程内没有活任务时写成失败，不重跑。Card 输入 MIME 是 `application/json`。
- Runbook evidence 进入 `verify._catalog_payload`。`research` 在 URL、API key、agent id 任一为空时不发 HTTP。
- 三个启动脚本不再 `export` 默认 URL。Compose profile 名是 `runbook`。`agent` 只在 `default` 网络。
- `call_model` 在 `model.complete` 之前增加 `llm_attempts`。`llm_calls` 与它相同。`llm_successes` 只计解析成功。
- `agent/uv.lock` 被 Dockerfile 和 `.github/workflows/test.yml` 的 `uv export --frozen` 使用。直接依赖钉在 P0 记录的版本，包括 `langgraph==1.2.11`。
- 主图仍然没有三 Specialist。Prometheus 的 `up` 查询会带上 scrape limitation。真实 RAGFlow completion 和真实 Loki 查询这次没有跑，状态是 `SKIPPED`。

## 提交

两个 SHA 不是同一层：

- 产品代码基线：`169ccabcc8881e88c166a3a05ea6bc29c8c6c818`（`fix: 承认 Prometheus MCP 的文本结果，并隔开 RAGFlow 的 Redis`）。下面的行为都相对这一提交。
- 审计文档提交：`6b808dd4bbb522f9b2ff20a3ce334f3a5f1791d1`。它只新增本文件的第一版，没有改产品代码。本 P0.1 继续只改本文件。
- 分支：`main`，与 `origin/main` 在写本补充前一致。工作区没有产品代码改动。
- 本机 Go：`go1.26.6`。`backend/go.mod` 声明 `go 1.25.7`。Gin `v1.10.1`，gRPC Go `v1.80.0`，client-go `v0.35.9`。
- 本机 venv 已解析：`langgraph 1.2.11`、`langgraph-checkpoint-postgres 3.1.2`、`mcp 1.30.0`、`a2a-sdk 1.3.0`、`pydantic 2.13.4`。
- `agent/pyproject.toml` 仍是 `langgraph>=0.2`。没有 lock。`agent/Dockerfile` 与 `.github/workflows/test.yml` 执行 `pip install -e .` 或 `pip install -e ".[dev]"`。

## 已实现

- 检查完成与 outbox 同事务。consumer 用 `event.Decide` 开闭 Incident。Kubernetes 观察不调用 `event.Decide`。
- Timeline 三列时间在 `backend/internal/db/apply.go` 的 `applyCheckTx`。投递快照在 `insertDeliveriesTx`。`TestApplyCheckEventTransitions` 断言恢复快照 `StartedAt=1000`、`OccurredAt=1100`、`ResolvedAt=1100`。
- 主图是 `Runner._graph`（`agent/src/alertflow_agent/runtime/graph.py`）。节点是 `bootstrap` → `collect_alertflow` → `collect_diagnostics` → `analyze` → `recall` → `select_skills` → `initial_plan` → `lead_model` / `lead_tools` / `replan` → `write_report` → `verify` → `persist`。`GraphState` 是普通 `TypedDict`，没有 `Annotated` reducer，也没有 `Send`。
- Goose 迁移 `00001`–`00019`。`00014_runtime_timeline.sql` 建 Timeline 三表。`00018` / `00019` 是 A2A 任务表和别名表。
- MCP：`ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` 的 `/mcp`，Loki MCP 本地镜像 `v0.6.0` 的 `/stream`。`_is_empty_payload` 只在 JSON 含 `data` 键时才把空 `data` 当空结果。
- RAGFlow 镜像 `infiniflow/ragflow:v0.27.3`。Valkey 在 Compose 网络 `ragflow` 上。
- A2A 客户端 `integrations/runbook.py` 在 `SendMessage` 之后轮询 `GetTask`。Part 的 MIME 是 `application/json`。`parse_completion` 只有 document id 和 chunk id 成对时才把 `citations_available` 设为 true。

## 主调查、历史复核、外部 Runbook

这三样不是同一个东西。

- 主调查：`Runner._graph` 的 Lead 循环。没有 Runtime、Metrics、Logs 三个 Specialist，也没有并行 fan-out。
- 历史复核：`runtime/tools.py` 的 `delegate_history_review`。它增加 `guard["subagent_runs"]`，循环调用 `call_model(..., "subagent", ...)`，上限是 `settings.subagent_max_rounds`。证据 `source_type` 是 `subagent_review`。`MAX_SUBAGENT_RUNS` 和 `SUBAGENT_MAX_ROUNDS` 出现在 `docker-compose.yml`、`.env.example`、`agent/.env.example`、`config.py`。这不是三 Specialist。
- 外部 Runbook：工具 `consult_runbook_agent`，服务 `a2a-adapter`，profile 名 `multi-agent`。它只启动 `python -m alertflow_agent.a2a_adapter`。文档 `deploy/ragflow-a2a-adapter/README.md` 和 `docs/CONFIGURATION.md` 也使用这个旧名。

## PostgreSQL、pgvector、Kafka、Redis

Go 库：`backend/internal/db/db.go` 的 `Open` 在 ping 成功后调用 `migrate`。`migrate` 用进程内互斥和 advisory lock `0x414C4657`，再跑嵌入的 goose 文件。这些迁移不执行 `CREATE EXTENSION vector`。

Agent 库：`agent/src/alertflow_agent/db.py` 的 `ensure_database` 在有权限时对维护库 `postgres` 执行 `CREATE DATABASE`，然后 `apply_schema` 整份执行 `agent/schema.sql`。该文件含 `CREATE EXTENSION IF NOT EXISTS vector`、`episodes.embedding vector(1024)`、HNSW 索引 `episodes_embedding`（`vector_cosine_ops`）。`config.py` 的 `embed_dim` 默认 1024，注释要求与该列一致。

检索：`memory_store.py` 的 `MemoryRepo.search` 用 `1 - (embedding <=> %s::vector)`，并要求 `embedding IS NOT NULL`。`runtime/recall.py` 的 `recall_prior` 只在 `embed.ready` 时走向量；没有嵌入或召回失败时，用 `list_recent` 的最近记录，`vector_score` 为 `None`。`InMemoryMemoryRepo` 在 Python 里算余弦，不连 Postgres。

Compose 默认镜像是 `pgvector/pgvector:pg16`（`docker-compose.yml`）。这只说明镜像带扩展。本阶段没有查询任何库的 `pg_extension`。扩展是否已创建：`NOT RUN`。

Kafka 不是 PostgreSQL `LISTEN`。全仓 Go/Python/SQL 没有 `LISTEN` 或 `pg_notify`。分发是 `backend/internal/publish/publish.go` 的 `Publisher.publishOne`：`WriteMessages` 成功后才由 `PublishUnderServiceLock` 记下已发布；失败走 `fail` → `RecordOutboxFailure`，行留在 outbox。`backend/internal/consume/consume.go` 的 `Consumer.Run` 用 `kafka.NewReader` 的 `FetchMessage`，数据库事务成功后才 `CommitMessages`。数据库错误不提交 offset。

Redis：`backend/internal/cache/cache.go` 的 `New`。空 `REDIS_URL` 返回 `(nil, nil)`。URL 无法解析则返回错误，调用方丢掉缓存。`Ping` 失败只打日志。连续失败 5 次后熔断 5 秒，读请求直接走 PostgreSQL。`opTimeout` 是 250ms。缓存只给 query 层的读结果。包注释写明不存当前 Incident、投递状态或凭证。入口是 `cmd/alertflow/main.go` 和 `internal/api/server.go`。

## Kubernetes Observer 持久化

迁移：`backend/internal/db/migrations/00014_runtime_timeline.sql`。

- `runtime_events`：`event_key` 唯一。时间列 `occurred_at`、`observed_at`、`recorded_at`。对象列含 `resource_uid`。
- `runtime_source_state`：主键 `(service_id, source)`。列有 `enabled`、`status`、`last_attempt_at`、`last_success_at`、`last_error_safe`。
- `runtime_object_states`：主键 `(cluster_id, resource_uid)`。列有 `fingerprint`、`snapshot`、`resource_version`、`active`、`last_seen_at`。

写路径：`backend/internal/db/kubernetes.go` 的 `ApplyKubernetesObject`。

- 无行且 `Active=false`：提交空事务，不插入。
- 无行且仍活跃：`insertKubernetesObject`，Timeline 类型 `kubernetes.<kind>.first_seen`，`time_accuracy=first_seen`，source 写成 `ok`。
- 已有行且 fingerprint 与 `active` 都不变：只更新 `last_seen_at`，不写 Timeline，source 仍写成 `ok`。
- fingerprint 或 `active` 变化：覆盖该 `(cluster_id, resource_uid)`。`Active=false` 时事件是 `inactive`。`Live=false` 时 `time_accuracy=reconciled_after_gap`，否则是 `observed`。变化事件的 key 带 `resource_version`，同一版本重放被 `event_key` 挡住。
- `DeactivateUnseenKubernetes` 把本次 list 没见到的活跃 UID 标成 `active=false`，Timeline 为 `inactive`，`time_accuracy=reconciled_after_gap`。
- `ReleaseKubernetesMapping` / `DisableKubernetesCollection` 把该服务、集群、命名空间下的对象 `active=false`，source 写成 `enabled=false`、`status=disabled`。原因分别是 `mapping removed` 和 `collection disabled`。Timeline 行保留。

source 状态流转，写在 `upsertKubernetesSource` 和 `SetKubernetesSource`：

- 成功观察：`enabled=true`，`status=ok`，刷新 `last_success_at`。
- `onWatchError`：watch 过期（`IsResourceExpired` / `IsGone`）写 `gap`，文案 `watch expired`，然后 `requestReconcile`。权限错误写 `unavailable`，文案 `kubernetes permission denied`。其它 watch 错误不改 source。
- `Connect` 失败只记 `connErr`。之后 `Sync` 在 `client == nil` 时对每个 target 调用 `SetKubernetesSource(..., "unavailable", ...)`，不再次 `Connect`。
- 采集关闭：`status=disabled`，`enabled=false`。`upsertKubernetesSource` 在冲突更新时不改 `enabled` 以外的关闭路径；关闭走上面的 `stopKubernetesObservation`。

身份：对象行的身份是 `(cluster_id, resource_uid)`，不是名字。Pod 通过 ReplicaSet owner 找到 Deployment（`observer.go` 的 `involved` / `deploymentOfPod`）。`targetsOf` 只保留 `WorkloadKind == "Deployment"` 且 cluster、namespace、name 都非空的服务。没有 StatefulSet informer。

指纹：`objects.go` 的 `deploymentFingerprint`。调用点把错误丢掉：`observer.go` 第 401、424、450、467 行 `_ = ApplyKubernetesObject`，第 499 行 `_ = ApplyKubernetesNotice`。数据库写失败后，`listNamespace` 仍可能把 source 标成 `ok`（约第 378 行）。这是 P2 的修复入口。

测试：`observer_test.go` 的 `TestFakeClientRecordsMappedChangesOnly`、`TestWatchExpiryMarksAGap`、`TestConnectionFailureStaysUnavailable`、`TestWatchExpiryReconcilesWithoutALiveChange`、`TestRemovingTheMappingKeepsTimeline`、`TestDisabledCollectionClosesTheSource`、`TestPermissionErrorMarksUnavailable`。`kubernetes_test.go` 的 `TestKubernetesStateCanReturnWithoutDroppingTheEvent`、`TestListKubernetesObjectsReturnsBoundedStatus`、`TestKubernetesSourceWriteDropsWhenTheServiceIsGone`。这些用 fake client 或测试库。真实集群：`NOT RUN`。`deploy/k8s/` 不存在。仓库没有 Kind 或 Kustomize 清单。

## 事件时间与调查窗口

`incident_start`：`insertDeliveriesTx` 里 `occurred_at` 和 `started_at` 都是这次检查的 `OccurredAt`。没有 `resolved_at`。

`incident_resolve`：`occurred_at` 是恢复这次检查的时间，`started_at` 是打开时写入的时间，`resolved_at` 等于这次的 `occurred_at`。证据是 `TestApplyCheckEventTransitions`。

Timeline 行的 `occurred_at` 与 `observed_at` 都是检查的 `OccurredAt`，`recorded_at` 是写入时的 `now`。

P3 的 `build_investigation_window()` 现在不存在。归一化约定，供 P3 使用，本阶段不实现：

- 输入：一条 `incident_start` 或 `incident_resolve` 的 `occurred_at`、`started_at`、`resolved_at`。
- `incident_start`：下界和上界锚点都是 `started_at`。它与 `occurred_at` 相同。缺任一个则 gap，不用 Agent 启动时间填。
- `incident_resolve`：下界是 `started_at`，上界是 `resolved_at`。缺任一个则 gap。
- Prometheus 与 Loki 已用事件的 `occurred_at` 做锚点：`runtime/tools.py` 的 `_mcp_request` 取 `state["event"]["occurred_at"]`，再交给 `clamp_window` 和 `fit_promql_window`。它们还没有按 `started_at` / `resolved_at` 区分两类事件。

## 规格 6.1–6.8

### 6.1 A2A 状态

现状：未满足。

- `a2a_adapter/store.py` 的 `MemoryLedger.claim` 和 `PostgresLedger.claim` 插入 `status=working`。`claim` 不写 `task_json`。`PostgresLedger.claim` 的 INSERT 只有 `message_id, task_id, status, deadline_at`。
- `RunbookExecutor.execute` 在 `claim` 成功后同步调用 `ragflow.research`，返回前才 `_finish`。`SendMessage` 会等完检索。
- 重复 `message_id`：`claim` 返回 false 后，`status = prior["status"] if prior["status"] in {completed, failed, canceled} else "completed"`。执行中的 `working` 被回成 `completed`。过期 deadline 没有单独分支，也不会改成失败。
- `LedgerTaskStore.get` 只 `ParseDict` 已存的 `task_json`。`task_json` 为空则返回 `None`。它不把超时的 `working` 收敛成失败。
- 客户端 `integrations/runbook.py` 的 `consult` 轮询 `GetTask`，状态不是 `COMPLETED` 就抛错。因此 `FAILED` / `CANCELED` 不会被 `_consult_runbook` 写成成功 evidence。这条是客户端已有行为。服务端仍可能把未完成任务说成 `completed`。
- `build_card` 的 `default_input_modes` 是 `["text/plain"]`。客户端 Part 已是 `application/json`。两边不一致。
- 重启：working 行留在表里，没有活的进程任务，也没有读路径把它收敛。会悬空。

已有测试：`agent/tests/test_runbook.py` 的 `test_card_declares_only_runbook_research`、`test_repeat_message_does_not_call_ragflow_again`、`test_send_message_then_get_task_and_a_duplicate_skips_ragflow`、`test_failed_lookup_is_a_coverage_gap`、`test_uncited_completion_is_not_a_citation`。这些用进程内 Starlette 和 MemoryLedger。没有 Postgres 恢复测试。

缺失测试：working 期间重复提交仍返回 `working`；deadline 过后收敛为失败且不重跑 RAGFlow；`GetTask` 自己能收敛超时；`status` 与 `task_json` 一致；进程重启后的 Postgres 行；`FAILED` artifact 不进成功 evidence。

P1 入口：`RunbookExecutor.execute`、`LedgerTaskStore.get`、`store.py` 的 `claim` / `save_task`。不新增队列或扫描进程。

### 6.2 Runbook evidence

现状：未满足。

`_consult_runbook`（`runtime/tools.py`）把 `schema`、`citations_available`、`citations` 放进 evidence `data`。正文只进 `summary`，截到 400 字，不进 `data.content`。`source_type` 已是 `runbook`。

`runtime/verify.py` 的 `_catalog_payload` 对 `prometheus`、`loki`、`project_diagnostic` 有分支。没有 `runbook`。`build_catalog` 因此不会给 runbook 行分配 `fact_id`。

已有测试：`test_uncited_completion_is_not_a_citation` 只检查 `parse_completion`。`test_verify.py` 没有 runbook 用例。

缺失测试：有 document id 和 chunk id 时 catalog 出现 citation 路径；缺其中一个时 `citations_available=false` 且不能当文档引用；正文仍可保存。

P1 入口：`_consult_runbook` 和 `_catalog_payload`。不新建 citation 框架。

### 6.3 RAGFlow

现状：部分。

`ragflow.research` 在 URL 或 agent id 为空时抛 `ragflow is not configured`，发生在 `httpx.post` 之前。API key 为空时不设 `Authorization`，仍然 POST。缺的是 key 这一支。

请求体是 `completion_body`：`agent_id`、`query`、`stream: false`。路径是 `/api/v1/agents/chat/completions`。

`deploy/ragflow/README.md` 最后一句仍是 “This phase does not add an investigation tool or an A2A adapter.” 与已有工具和适配器矛盾。文档没有把容器启动、Elasticsearch ready、`GET /api/v1/system/healthz`、模型已配置、completion 成功分成五个状态。

Valkey 只在网络 `ragflow`。`agent` 和 `a2a-adapter` 都加入该网络。主 Agent 不直接调用 RAGFlow。

已有测试：`test_empty_adapter_url_does_not_call_the_client` 覆盖的是 A2A URL，不是 RAGFlow 三元组。没有“缺 API key 不发 HTTP”的测试。

P1 入口：`research` 在 post 前要求三项都非空；改 README。不改请求字段，不改镜像版本。

### 6.4 Prometheus

现状：解析和窗口已在代码里。`up` 与健康没有分开。

- 镜像和路径保持 v0.18.0 `/mcp`。工具参数在 `_mcp_request`。
- `_is_empty_payload` 已修。测试 `test_summarize_rejects_empty_and_keeps_a_short_value`。
- `scope_promql` 只接受一个 metric，并用 `_parse_matchers` 做标签相等。冲突 matcher 返回 `None`。测试 `test_conflicting_service_matcher_is_not_sent`。子串 `not-alertflow` 对不上 `alertflow` 的相等比较。
- `expression_lookback` 见到 `offset` 或 `@` 返回 `None`，`fit_promql_window` 随之拒绝。测试 `test_promql_time_modifiers_are_not_sent`。区间长度加上 incident 到窗口终点的 gap，超过 `mcp_max_window_seconds` 则拒绝或截断。测试 `test_range_lookback_truncates_to_the_incident_budget`。
- 次数、样本、字节、超时在 settings 和 `test_window_row_and_request_caps`、`test_call_budget_stops_before_another_connection`、`test_transport_stops_reading_past_the_byte_cap`。

缺失：没有任何函数在 metric 名为 `up` 时写入 “只表示 scrape 成功” 的 limitation。`probe_success` 也没有单独标记。查询结果原样进 evidence。P1 只补这个区分和测试，不重写解析器。

### 6.5 Loki

现状：状态码已分开，归属靠 selector。

`_mcp_request` 对 Loki 调 `scope_logql`。没有 selector 时 `MCPError("unmapped")`。空结果是 `empty`。超时、断开、超限各自一种 kind，文案在 `_MCP_NOTES`。测试 `test_unmapped_service_does_not_call_or_store`、`test_each_failure_kind_is_a_gap_and_not_evidence`、`test_one_source_can_fail_while_the_other_is_stored`。

缺失：没有单独断言 Loki 行必须带当前 service selector 才入库之外的额外归属字段。现有 `scope_logql` 拒绝未加 selector 的查询。P1 若测试已覆盖这些 kind，不改解析。真实 Loki 查询：`NOT RUN`。

### 6.6 配置与 profile

现状：未满足。

`scripts/up-all.sh`、`scripts/up-agent-ragflow.sh`、`scripts/up-agent-observability.sh` 在变量未设置时 `export` 内网 URL，然后启动 Compose。shell 环境盖住 `.env`。

Compose 里 Agent 的 `PROMETHEUS_MCP_URL`、`LOKI_MCP_URL`、`RAGFLOW_BASE_URL`、`A2A_ADAPTER_URL` 默认是空字符串（`${VAR:-}`）。空 URL 不建客户端。测试 `test_blank_urls_do_not_construct_a_client`、`test_default_compose_keeps_optional_urls_empty`。后者还断言 `a2a-adapter` 的 profile 是 `["multi-agent"]`。

`agent` 的 networks 是 `default` 和 `ragflow`。`a2a-adapter` 相同，profile 是 `multi-agent`。

没有三个优先级的 compose config 测试。没有 `*_ENABLED=false` 开关。

P1 入口：删掉三处 `export`；profile 改名 `runbook` 并改脚本、`.env.example`、`docs/CONFIGURATION.md`、`deploy/ragflow-a2a-adapter/README.md`、`test_default_compose_keeps_optional_urls_empty`；`agent` 去掉 `ragflow` 网络。

### 6.7 LLM 记账

现状：未满足。

`runtime/model.py` 的 `call_model` 在循环里最多两次 `model.complete`。`guard["llm_calls"]` 和 `guard["tokens"]` 只在 `model_validate` 成功后增加。失败尝试不计。预算检查在循环外，第一次失败后的第二次调用不再查剩余次数。

`runtime/guards.py` 的 `charge_llm` 会先加 `llm_calls`，但全仓没有调用它。它不是现用路径。

已有测试：`test_repair.py` 的 `test_budget_writes_partial` 把 `max_llm_calls=1`，走的是成功解析后的计数。没有“第一次失败、第二次成功仍算两次 attempt”的测试。

P1 入口：`call_model`。增加 `llm_attempts` 与 `llm_successes`。若保留 `llm_calls`，它必须等于 attempt 次数，并改掉 `graph.py` 里写入报告的那一处。不建外部预算服务。

### 6.8 Python lock

现状：未满足。

直接依赖在 `agent/pyproject.toml` 用 `>=`，只有 `mcp` 和 `a2a-sdk` 是 `==`。没有 `uv.lock` 或 `requirements.txt`。Docker 与 CI 都浮动安装。

P1 入口：把已解析版本钉住，同一 lock 给本地、`.github/workflows/test.yml` 和 `agent/Dockerfile`。不升级无关包。

## LangGraph 1.2.11 与 checkpoint 3.1.2

现用代码不能并行。`GraphState` 没有 reducer。`Runner._graph` 只用 `add_edge` 和返回字符串的条件边。缺省 checkpointer 是 `langgraph.checkpoint.memory.MemorySaver`（`_graph` 在参数为 `None` 时构造）。配置了 Agent 数据库时，`main.py` 的 `_checkpointer` 调用 `PostgresSaver.from_conn_string(url)` 和 `saver.setup()`。

已安装 API，本机临时进程核对过，没有写入仓库：

- `langgraph.types.Send.__init__(self, /, node: str, arg: Any, *, timeout=None)`。
- `StateGraph` 文档写明 reducer 签名是 `(Value, Value) -> Value`，状态键用 `Annotated[type, reducer]`。
- 包内示例是 `add_conditional_edges(START, continue_to_jokes)`，函数返回 `list[Send]`。节点函数返回 `Send` 列表会得到 `InvalidUpdateError`。本机按文档示例跑了一次内存图：两个 `Send("generate_joke", ...)`，`jokes: Annotated[list[str], operator.add]`，`MemorySaver`，结果是 `['Joke about cats', 'Joke about dogs']`。
- `PostgresSaver.from_conn_string(conn_string: str, *, pipeline: bool = False)` 是 contextmanager。`setup(self) -> None` 执行 `MIGRATIONS[0]` 并读 `checkpoint_migrations`。
- `langgraph.checkpoint.postgres.ShallowPostgresSaver` 存在。源码写明 2.0.20 起废弃，3.0.0 移除，替代是 `PostgresSaver` 加 `durability='exit'`。当前代码没有用它。

`PostgresSaver.setup()` 对真实数据库：`NOT RUN`。没有为它启动数据库。P3 才能把 `Send` 用进主图。P0.1 不改 `graph.py`。

## Fault Lab

目录存在：`/home/sancho/projects/fault-lab`。本阶段没有构建，没有运行。状态是 `NOT RUN`，不是通过，也不是 `BLOCKED`。

构建条件，只来自文件：

- `pyproject.toml`：`requires-python >=3.10`，依赖 `fastapi>=0.110`、`uvicorn[standard]>=0.27`、`pydantic>=2.6`。入口 `fault-lab = fault_lab.main:main`。包在 `backend/`。
- `Dockerfile`：第一阶段 `node:20-alpine` 在 `frontend/` 执行 `npm ci || npm install` 和 `npm run build`。第二阶段 `python:3.12-slim` 执行 `pip install .`，再拷入 `frontend/dist`。环境变量 `SIMULATOR_ADDR=:8090`、`SIMULATOR_CONFIG_PATH=/data/simulator.json`、`SIMULATOR_LOG_LEVEL=info`。
- `main.py` 的 `_not_built_page` 说明不构建前端时 API 仍可提供 `/health`、`/diagnostics/v1/snapshot`、`/control/v1/state`。这是源码说明，不是这次启动的结果。

故障与恢复，只来自源码：

- `schemas.py` 有 `http_503 = "http_503"`。
- `routes_health.py` 的 `HealthEndpoint`：`mode == "http_503"` 时 `GET /health` 返回 503 和正文 `service unavailable\n`。`GET /healthz` 固定 `{"status":"ok"}`，不看故障模式。
- `routes_control.py` 的 `reset` 是 `POST /control/v1/reset`，调用 `store.reset()`，而 `StateStore.reset` 调用 `update(default_state())`。
- 场景在 `scenarios.py`，多个 profile 的 inject 使用 `mode=http_503`。

日志：`main.py` 的 `_configure_logging` 使用 `logging.basicConfig`，格式是 `%(asctime)s %(levelname)s %(name)s: %(message)s`，写到 stderr。这不是 JSON。容器的 stderr 可以被采集，文本格式本身不表示没有日志。源码里没有 `fault_id`、`service_id` 或故障模式这类关联字段，这一点只是源码结论。`state_store.py` 的 `_persist` 写的是 `simulator.json`，`profiles.py` 的条目是 API 快照字段（`http_status`、`message`、`route` 等）。这两处都不是容器日志。构建、运行和 Loki 取证：`NOT RUN`。

## P2–P7 入口

- P2：`targetsOf`、`Connect`、`Sync`，以及 `observer.go` 里丢掉的 `ApplyKubernetesObject` 错误。测试已在 `observer_test.go`。
- P3：`Runner._graph`。新增三个 Specialist。不能把 `delegate_history_review` 改名充数。窗口函数按上一节的输入输出。LangGraph 用法以本节核对到的 `Send` 和 `Annotated` reducer 为准。
- P4：新建 `deploy/k8s/`。现在没有可复用清单。
- P5：同一 Observer 对真实 Kind。fake client 不能当端到端。
- P6：集群内 Blackbox 和 Pod 日志。不能沿用 Compose 的 Docker socket。
- P7：Fault Lab 的 `http_503` 与 `POST /control/v1/reset`。构建和运行都是 `NOT RUN`。

## 第 5 节验收

对照规格第 5 节的执行项。这里的“通过”只表示审计已经对到源码或一次临时 API 核对，不表示产品功能已经符合后面的阶段。

1. 通过。分支 `main`，产品基线 `169ccab`，审计文档 `6b808dd`。没有 `reset` 或 `clean`。
2. 通过。Gin、gRPC、Timeline、Monitor、Observer、MCP、A2A、Compose profile 的路径写在上面各节。
3. 通过（锁定方案只记录，不落 lock）。Go/Python 版本、Dockerfile、两套数据库初始化、pgvector SQL、Kafka reader、Redis `cache.New` 都有函数。`pg_extension` 是否已安装：`NOT RUN`。
4. 通过（源码）。对象类型、`targetsOf`、`Connect` 失败、source 状态和 fingerprint 写在 Observer 一节。真实集群：`NOT RUN`。
5. 通过。`deploy/k8s/` 不存在，没有 Kind/Kustomize/RBAC 清单可复用。
6. 通过。6.1–6.8 各有现状、测试名和 P1 入口。已修好的 `_is_empty_payload` 和 RAGFlow 网络隔离不重复改。
7. 通过。主循环、`delegate_history_review`、`multi-agent` profile 已分开。没有把后两者写成核心 Multi-Agent。
8. 部分通过。`Send`、reducer、`MemorySaver` 的内存示例已跑通。`PostgresSaver.setup()` 对真实库：`NOT RUN`。没有改 Dockerfile，没有创建 lock。
9. 通过。`TestApplyCheckEventTransitions` 给出两种事件的三个时间字段。`build_investigation_window` 尚未实现，输入输出约定已写明。P0 不改测试。
10. 未通过。目录和源码在，`http_503` 与 `reset` 能指到函数。没有构建，没有运行，不能写成模式可用。状态 `NOT RUN`。

规格第 5 节的验收句：通过。当前状态能对应到文件和函数。主图没有固定三 Specialist 并行调查。历史复核和 A2A adapter 不是核心 Multi-Agent。

第 8 条和第 10 条的未跑部分不阻塞 P1。它们分别是 P3 和 P7 的前置记录。
