# AlertFlow V2 契约

日期：2026-10-28  
基线：`a7cc63b7a843e23b5207cf20aa5be105b453b23f`（`main`，与 `origin/main` 一致）

本文是 V2 后续阶段的实施契约。设计来源是 Rev4（`AlertFlow_V2_Cursor_Design_Rev4.md`）和同日的 MCP/A2A 研究摘要。与 Rev4 字面冲突时，以本文为准。本文不改业务行为。

Phase 0 验收通过后才能改核心代码。一次只做一个阶段。

## 1. 阶段顺序

- Phase 0：审计、测试基线、本文。不改业务代码。可选集成不做真实环境 smoke test。
- Phase 1：Fiber v3 整迁移到 Gin。公开 URL、方法、状态码、JSON、cookie、权限、错误体、SPA fallback 和嵌入前端保持现状。此阶段仍会为外发渠道创建投递任务。
- Phase 1.5：退役 Slack、Telegram、email、通用 webhook。`agent_webhook` 的 HTTP 投递保持。
- Phase 2：Runtime Timeline。检查驱动的开/关仍走 Kafka/Consumer。手动创建走 API 事务。
- Phase 3：Go 持续采集应用 diagnostics。Agent 仍用 `DIAGNOSTICS_FILE` 直连。
- Phase 4：新增 `ALERTFLOW_ROLE=monitor`。`server` 不再启动周期调度。`POST /api/services/:id/check` 继续走一次性 `CheckNow`。
- Phase 5：只读 Kubernetes observer。默认关闭。不调用 `event.Decide`。
- Phase 6：`GET /api/services/:id/timeline` 和服务详情页时间轴。
- Phase 7：Go 与 Agent 的三条内部业务路径一次性切到 gRPC。HTTP 留作回滚。
- Phase 8：可选 Prometheus 与 Loki MCP。未配置不连接。结果只留在当次 Agent evidence。
- Phase 9–10：RAGFlow、A2A。某一项验证失败只停该项。
- Phase 11：不实施，除非另下指令。
- `/webhooks/agent`、`/internal/agent/v1/...` 和 Agent 的 `POST /webhooks/alertflow` 已删除。浏览器用的 `/api/...` 还在。

## 2. 当前代码事实

审计对象是上面的基线提交，不是目标架构。

- 角色有 `server`、`monitor`、`publisher`、`consumer`、`delivery`，以及一次性的 `retire-deliveries`（`backend/cmd/alertflow/main.go`）。`monitor` 每秒重读配置并跑周期检查和诊断采集。`api.New` 不 `Sync`。`POST /api/services/:id/check` 仍是一次性 `CheckNow`。删除服务先改 `config.yaml`，再在 `pg_advisory_xact_lock` 里写 `retired_services` 并删运行数据。检查和诊断拿到同一把锁后重读文件，服务已不在就不写。TLS 快照和检查在同一个事务里。Publisher 与删除抢同一把服务级 advisory lock，持锁完成退役检查、租约校验、Kafka 发送和 `published_at` 更新。带 `epoch` 的事件在代际不比退役代际新时不发送；没有 `epoch` 的旧事件仍按 `retired_at` 比较。删除把当前代际写入退役行，并把下一次检查的代际加一，所以同一秒内加回的服务不会被丢掉。Kafka 超时未确认时仍可能重复，Consumer 用同一规则只记 `processed_events`，不再建 Incident 或运行状态。`retire-deliveries` 不在启动时运行。
- HTTP 是 Gin v1.10.1。`HandleMethodNotAllowed` 为 false，错误方法进入 `serveSPA`。模块声明 Go 1.25.7。Agent 业务走双向 unary gRPC：Go 提供 `AlertFlowControlService`，Agent 提供 `AgentTaskService`。`k8s.io/client-go`、`k8s.io/api`、`k8s.io/apimachinery` 固定为 v0.35.9。Redis 只是可降级读缓存。
- Goose 迁移嵌入二进制，在 `db.Open` 时自动执行，并有 advisory lock。已有 `00001`–`00017`。`00013` 让退役审计在投递行删除后仍然保留，并记录取消原因。`00014` 新增 Timeline 三张表。`00015` 新增 `retired_services`。`00016` 增加 `service_epochs`，并给退役行补上代际。`00017` 允许内部 Agent 任务的 `integration_id` 为空。不要改已应用的迁移。
- `integrations.type` 仍允许 `telegram`、`slack`、`email`、`webhook`、`agent_webhook`（`00010`）。创建、更新和测试只接受 `agent_webhook`。这四种外发渠道没有发送器，`Dispatcher.Notify` 已删除。
- `notification_deliveries.status` 允许 `pending`、`running`、`retry_wait`、`succeeded`、`dead`、`cancelled`（`00012`）。
- `insertDeliveriesTx`、`insertManualDeliveriesTx` 和 `CreateChatRequest` 只在 `ALERTFLOW_AGENT_GRPC_TARGET` 非空时插入 `integration_id` 为空的任务。空目标仍写 Incident 和 Timeline。`ClaimDeliveries` 只领取这种内部任务。检查真正打开或解决故障时，同一事务写 `runtime_events`。手动创建走 `CreateManualIncident`，不经过 Kafka。
- 前端集成表单只创建 `agent_webhook`。历史四种渠道仍可列出和删除。
- `POST /api/incidents` 在一个事务里写 Incident、Timeline 和可用 Agent 的调查任务（`backend/internal/db/timeline.go`）。只改标题或开始时间不写 Timeline。首次解决写解决事件，不建新任务。
- `ALERTFLOW_K8S_ENABLED` 为 `true` 或 `1` 时，`monitor` 只读观察已映射的 Deployment。client-go 为 v0.35.9。空的 `kubernetes` 块不采集。连接失败、权限错误或 15 秒内缓存未同步，只把来源标成 `unavailable` 并在后台重试，不卡住配置重载或退出。Pod 和 Deployment 的新增事件在缓存同步之后写入；四类 informer 的 Watch 错误都会标来源并再协调一次。去掉映射只把对象标成不再活动、来源标成 `disabled`（`mapping removed`），Timeline 保留。采集开关关闭时对当前映射做同样收口，`last_error` 为 `collection disabled`，配置没变就不再写。同一 `cluster_id`、命名空间和工作负载名不能配给两个服务。语义指纹变化才写 Timeline，后续事件键带 `resourceVersion`，`source` 为 `kubernetes`。来源状态和对象写入都在服务锁之后确认服务仍在配置里。不调用 `event.Decide`，不建 Incident。`GetKubernetesState` 返回身份、active、resourceVersion、fingerprint 和裁剪后的状态摘要。
- `GET /api/services/:id/timeline` 给已登录用户返回该服务的 Timeline。默认最近 24 小时，窗口不超过 `ALERTFLOW_TIMELINE_RETENTION_DAYS`。按 `(occurred_at, id)` 做 keyset。`title` 和 `summary` 最多 1024 个字符。来源覆盖来自 `runtime_source_state`。不缓存，不写事件，不挂到 `/internal/agent/v1`。
- `monitor` 对配置了 `diagnostics.url` 的服务按间隔拉取应用诊断。种类与 Agent 相同，其中 `saturation` 和 `resource` 一样必须带 `status`：`unhealthy` 记为 `error`，`degraded` 记为 `warning`。失败只更新 `runtime_source_state`。成功时把异常项和恢复写进 `runtime_events`，`source` 为 `application_diagnostics`。替换或去掉采集时会等旧循环退出。不调用 `event.Decide`，不建 Incident。Agent 仍用 `DIAGNOSTICS_FILE` 直连。
- `CheckNow` 在没有常驻 worker 时会造一个临时 worker 做一次性检查（`backend/internal/monitor/scheduler.go`）。
- Agent 任务经 gRPC `StartInvestigation` / `StartChat` 发出。结果经 `SubmitAgentCallback` 回来。查询走 `AlertFlowControlService`。Agent 回调出站表仍是 `agent_callback_outbox`，靠 `message_id` 唯一。回调只在 gRPC target 和 `ALERTFLOW_AGENT_GRPC_TOKEN` 都存在时入队，并只在 `stored` 或 `duplicate` 时标 sent。
- 检查与 Outbox 同一事务。Publisher 在 broker ack 之后才标记已发布。Consumer 在一个事务里写 `processed_events`、Incident、运行状态和投递任务，提交之后才提交 Kafka offset。这些边界在 V2 里保持。

公开路由以 `Server.routes` 为准。Gin 迁移必须保持这些路径和方法：

- 公开：`GET /api/setup/status`，`POST /api/setup`，`POST /api/auth/login`，`POST /api/auth/logout`
- 登录用户：`GET /api/auth/me`，服务列表/指标/检查，设置、仪表盘、Incident 查询与评论，投递列表，Agent 报告和聊天读写
- 管理员：服务和布局的写操作、立即检查、Incident 写操作、图片、设置、原始配置、用户、导出导入、集成 CRUD 和测试发送、投递重试
- `GET /images/:file`。`/api/`、`/webhooks/`、`/internal/` 不落到 SPA

错误体保持 `{ "error": "..." }`。

## 3. 测试基线

工具：Go `go1.26.6`，Node `v22.23.1`，Python `3.12.3`，pytest `9.1.1`，Docker `29.6.1`。

宿主机 `127.0.0.1:5432` 当时由 `enterprise-doc-qa_devcontainer-db-1` 占用。`alertflow-postgres-1` 和 `alertflow-kafka-1` 处于退出状态。文档中的 `alertflow:alertflow@127.0.0.1:5432` 在这次审计里认证失败。Go 与 Agent 的数据库测试改连临时实例 `127.0.0.1:5433`（本地已有的 `pgvector/pgvector:pg16`，用户 `alertflow`，库 `postgres`）。测完已删除该容器。

### Go

`go vet ./...` 退出码 0。`go build ./...` 退出码 0。

```text
cd backend
ALERTFLOW_TEST_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5433/postgres?sslmode=disable' \
  go test ./... -count=1 -timeout 180s
```

退出码 0。`api`、`archive`、`cache`、`config`、`consume`、`db`、`deliver`、`event`、`incident`、`monitor`、`notify`、`query` 通过。`cmd/alertflow`、`auth`、`image`、`logger`、`publish`、`state`、`web` 没有测试文件。

`TestKafkaPublishAndConsume` 被跳过：未设置 `ALERTFLOW_KAFKA_BROKERS`。这是当前代码的既定行为，不是失败。Redis 测试连的是本机 `127.0.0.1:6379/15`（`alertflow-redis-1`），通过。

### 前端

在 `web-ui/` 执行 `npm run type-check`、`npx oxlint .`、`npx eslint . --no-cache`、`npm run build`。四项退出码都是 0。没有使用会改文件的 `npm run lint`（它带 `--fix`）。构建产物在被忽略的 `web-ui/dist`。

### Agent

```text
cd agent && python3 -m pytest -q
```

退出码 0：66 passed，1 skipped。被跳过的是 `test_postgres_event_sequences_and_resolve_are_atomic`。它的默认地址指向 `127.0.0.1:5432`，连到了别的实例。指定临时库之后该测试通过：

```text
AGENT_TEST_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5433/alertflow_agent_seq?sslmode=disable' \
  python3 -m pytest -q tests/test_events.py::test_postgres_event_sequences_and_resolve_are_atomic
```

退出码 0。pytest 无法写入 `agent/.pytest_cache`（权限被拒绝），不影响结果。

```text
python3 evals/run_evals.py
```

打印 `NOT RUN`，退出码 1。未设置 `LLM_API_KEY`。这与 `agent/README.md` 的说明一致，没有编造分数。

## 4. 外发通知退役

Phase 1.5 停用 `telegram`、`slack`、`email`、`webhook`。`agent_webhook` 继续按现有 HTTP 投递，直到 Phase 7。

顺序：

1. 代码先禁止为这四种类型新建投递。`ClaimDeliveries` 不再领取、不再重试它们，也不再把它们扫成 `dead`。只把 `enabled = false` 当作停用是不够的，重新启用会再次被领取。
2. 排空并隔离仍在跑旧二进制的 Delivery Worker。
3. 确认没有旧 Worker 之后，把这些渠道上的 `pending` 和 `retry_wait` 标成 `cancelled`。
4. `running` 只有在能证明没有旧 Worker 还会发出请求时才标成 `cancelled`。证明不了就写入审计表，行保持 `running`。

`cancelled` 不能伪装成 `dead` 或 `succeeded`。`00012` 只增加 `cancelled` 和 `retired_delivery_audits`。Goose 会在每次 `db.Open` 时自动迁移，所以 `Up` 里不批量修改已有行。排空旧 Worker 之后，用 `ALERTFLOW_ROLE=retire-deliveries` 跑一次：它把这四种渠道上的 `pending` 和 `retry_wait` 标成 `cancelled`，`last_error` 和 `retired_delivery_audits` 都记下原因；`running` 保持原状并写一行审计。审计表不引用投递行的外键，删除集成或导入归档不会带走这些记录。再跑一次不会重复写审计。历史 `integrations` 行保留，不 DROP。

Timeline 迁移是 `00014`。`00013` 补了取消原因，并让审计记录不随投递行级联删除。

归档导出仍包含历史渠道。导入这四种类型时只保留记录，不重新入队。创建、更新和测试发送 API，以及集成表单里的这四种类型，在 Phase 1.5 去掉。`agent_webhook` 的创建、更新和投递保持。

## 5. Timeline 与手动 Incident

Phase 2 新增 `runtime_events`、`runtime_source_state`、`runtime_object_states`。成功的 HTTP 检查仍只留在 `checks`。warning 检查不逐条进 Timeline。只有故障打开和解决写入。

检查驱动的打开和解决仍只在 `ApplyCheckEvent` 的事务里写 Timeline，并且只有这次事务真正插入或解决了一行 Incident 时才写。已有进行中故障时，现有逻辑会跳过再次打开，因此不再写打开事件，也不再入队。

`POST /api/incidents` 不补发 Kafka。它在同一个数据库事务里写入 Incident、Timeline 打开事件，以及在 Agent 目标有效时写入调查任务。三者一起提交或一起回滚。请求带 `resolvedAt` 时，同一事务再写一条 Timeline 解决事件，调查任务仍只有一条。任务形态与现有 `incident_start` 投递相同，并标上 `manual` 来源。

Agent 目标有效指：存在启用的 `agent_webhook`，且 URL 可用。没有这样的目标时仍提交 Incident 和 Timeline，不插入会一直重试的任务。每个启用的 `agent_webhook` 各一条任务。

幂等键按 Incident 固定：`incident:{id}:opened` 与 `incident:{id}:resolved`。`occurred_at` 是故障发生时间。`observed_at` 是系统观察到这次事件的时间：检查路径用检查时间，手动补录用写入时的当前时间。`recorded_at` 是落库时间。投递继续用 `(event_id, integration_id)` 唯一约束，`event_id` 与打开事件键相同。另一次 HTTP 创建是另一条 Incident。服务上已有进行中故障时，现有唯一索引拒绝第二次创建。

`PUT` 只改标题或开始时间：不写 Timeline，不建任务。`PUT` 把尚未解决的 Incident 标成已解决：在该次更新的事务里写解决事件，不建任务。评论不写 Timeline，不建任务。删除不建任务。Timeline 行保留，`incident_id` 按 `ON DELETE SET NULL` 置空。

Kubernetes、应用诊断、Prometheus、Loki 在本轮不调用 `event.Decide`，也不直接开关 Incident。

删除服务时先停掉它的检查协程，再在一个事务里删除检查、TLS、Timeline、运行状态、未发布 outbox 和 Incident。事务成功后才从 `config.yaml` 去掉该服务。清理失败会记日志并返回错误，配置保持不变。

## 6. gRPC 契约

Phase 7 才生成代码和切换传输。Phase 0 把地址、认证和切换规则定死。字段级 `.proto` 到 Phase 7 再按现有 DTO 收口，生成的 Go/Python stub 提交进仓库。只用 unary RPC。

两组服务嵌入现有进程，不另拆微服务：

- Go `AlertFlowControlService`：`GetRuntimeConfig`、`ListIncidents`、`GetIncident`、`GetMetrics`、`ListChecks`、`QueryTimeline`、`GetSourceCoverage`、`GetKubernetesState`、`SubmitReport`、`SubmitRunEvent`。复用现有 query/DB，不复制业务规则。
- Python `AgentTaskService`：`StartInvestigation`、`StartChat`。任务先落库或被识别为重复，再返回成功。成功不表示调查已经完成。

变量：

| 变量 | 值 |
| --- | --- |
| `ALERTFLOW_GRPC_ADDR` | `:9090` |
| `AGENT_GRPC_ADDR` | `:9000` |
| `ALERTFLOW_AGENT_GRPC_TARGET` | `agent:9000` |
| `ALERTFLOW_GRPC_TARGET` | `alertflow:9090` |
| `AGENT_TASK_GRPC_TOKEN` | Delivery 调用 Agent |
| `ALERTFLOW_AGENT_GRPC_TOKEN` | Agent 调用 Go |

两枚 token 互相独立，也不复用 MCP 或模型密钥。发送时读取当前值，不写进任务行。任务、查询和回调都走这一份 proto。默认 Compose 不把这两个端口映射到宿主机。暂不启用 mTLS。

三条路径一起切换：Delivery 启动任务、Agent 查询 Go、Agent 提交报告和运行状态。不允许只切其中一条。HTTP 实现留在代码里，供回滚。可靠投递仍由数据库任务行、租约、重试和 callback Outbox 负责。gRPC 不是队列。

## 7. 可选集成的候选、风险和降级

以下版本来自 2026-10-28 的研究摘要，是候选，不是锁定版本。本阶段没有启动这些服务，也没有做真实查询。某一项在自己的阶段验证失败时，只停该项。

Prometheus MCP 候选是 `prometheus/prometheus-mcp` `v0.18.0`。候选 transport 是 Streamable HTTP 或 stdio。首批工具候选是 `query`、`range_query`、`metric_metadata`、`label_names`、`label_values`。不开放 TSDB admin。`series` 要等真实案例证明需要再开。若该版本与 LangChain MCP adapter 不兼容，候选退路是已配置数据源的 `grafana/mcp-grafana`。不自研 MCP Server。

Loki MCP 候选是 `grafana/loki-mcp` `v0.6.0`。首批只开放 `loki_query`。它的 transport 比 Prometheus MCP 更需要在 Phase 8 做兼容验证。同样可以退到 `grafana/mcp-grafana`。

Phase 8 锁定（没有连接真实 Prometheus 或 Loki，也没有退到 `grafana/mcp-grafana`）：

- Prometheus：镜像 `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0`。不要用 `latest`；v0.19.0 起二进制改名为 `prometheus-mcp`，环境变量前缀改为 `PROMETHEUS_MCP_`。Transport 是 Streamable HTTP，路径 `/mcp`（`--mcp.transport=http`）。Agent 只调用 `query`、`range_query`、`metric_metadata`、`label_names`、`label_values`。不调用 `series`，也不调用 TSDB admin。
- Loki：发布物是 `grafana/loki-mcp` tag `v0.6.0` 的 `loki-mcp-server`。该 tag 的 `cmd/server/main.go` 同时提供 stdio、SSE（`/sse`，消息端点 `/mcp`）和 Streamable HTTP（`/stream`）。锁定 Streamable HTTP，配置的 URL 必须是 `/stream`。Agent 只调用 `loki_query`。该发布物的 README 没有单独的镜像名。
- LangChain MCP adapter 记录的 `http` transport 就是 Streamable HTTP，两边都对得上。运行时用 MCP Python SDK `1.30.0` 的同一 transport，只在 URL 非空时创建客户端。每次调用先 `initialize`，再 `list_tools`，只调用服务器实际提供且本项目批准的只读工具。服务器没有该工具时记 `MISSING_TOOL`，不调用。`langchain-mcp-adapters` 已不再维护，不作为依赖。
- 查询必须能映射到当前 `service_id`。`PROMETHEUS_SERVICE_SELECTORS` / `LOKI_SERVICE_SELECTORS` 没有该服务的条目时记 `UNMAPPED`，不把全局结果存成该服务的证据。`label_names` 和 `metric_metadata` 不写入 evidence。单次 run 最多 `MCP_MAX_CALLS`（4）次；时间窗最多 `MCP_MAX_WINDOW_SECONDS`（3600）；样本、日志行和响应字节分别不超过 100、100 和 65536。超时、断连、认证失败、429、空结果、畸形结果和超限只记该来源的 gap，调查继续。

RAGFlow 候选是稳定版 `v0.27.3` 和预览版 `v1.0.0-rc1`。Phase 9 先做 API smoke test 再固定 tag。不把 RC 称作稳定版，也不使用 `latest`。当前研究没有确认 RAGFlow 原生 A2A。

A2A 使用 1.0 的 HTTP+JSON/REST，非流式，轮询任务。官方 Python SDK 的具体版本留到 Phase 10，用 Agent Card 和 SendMessage/GetTask 做一致性验证。适配器只做协议翻译，不复制 RAGFlow 的检索或推理。

降级：

- 未配置 Prometheus MCP、Loki MCP 或 RAGFlow 时，核心 Compose 和 Agent 调查仍可运行。客户端不反复连接未配置的地址。
- 某个可选来源超时、鉴权失败或返回无法解析时，报告写明该来源没有证据，调查继续。
- 没有可验证文档来源的 A2A 结果只能作为专家建议，不能当作已证实引用。
- 指标和日志只进入当次 Agent evidence，不复制进 Runtime Timeline。
- 默认不把这些服务放进核心 Compose。

## 8. 留到对应阶段再定的事项

- Phase 7：`.proto` 字段。
- Phase 8 已锁定：Prometheus `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0`，Streamable HTTP `/mcp`，工具 `query`、`range_query`、`metric_metadata`、`label_names`、`label_values`。Loki `grafana/loki-mcp` `v0.6.0`，Streamable HTTP `/stream`，工具 `loki_query`。
- Phase 9：RAGFlow 的最终 tag。
- Phase 10：A2A Python SDK 的最终版本。
- HTTP 业务路由的删除日期。
