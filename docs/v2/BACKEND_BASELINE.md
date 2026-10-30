# AlertFlow 后端基线审计（P0-15/16/17 · B0）

- 审计对象：`backend/`（模块 `github.com/ecakeman/alertflow`），HEAD `a5ca5d2`，分支 `main`，2026-10-30。
- 依据：`AlertFlow_V2_Cursor_Design_Rev6.4_后端规整与系统认知部署整合版.md` 第 6 节 P0 第 15–17 条、第 7.8 节 B0–B4。
- 方法：只读代码追踪 + 全仓引用搜索 + 编译/静态检查/测试三证据。所有结论均可由文中命令复现。
- 判定标签：**KEEP**（当前正确、保留）/ **REFACTOR**（目标态需调整）/ **LEGACY CANDIDATE**（无生产入口，可删）/ **UNVERIFIED**（证据不足）。

---

## 0. 审计证据命令与退出码

在 `backend/` 下执行（Go 1.26.6，PG 用临时容器 `alertflow-test-pg`）：

| 命令 | 退出码 | 结论 |
| --- | --- | --- |
| `go build ./...` | 0 | 全仓可编译 |
| `go vet ./...` | 0 | 无静态检查告警 |
| `go test -count=1 -p 1 -timeout 600s ./...` | 1 | 仅 `internal/api` 的 `TestCrossLanguageGRPC` 失败，属环境问题（见 §11.4） |
| `go test -count=1 ./internal/incident` | 0 | 死代码包自身测试通过（14.86s） |
| `go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./... \| grep '\|.*internal/incident' \| wc -l` | 0 | 输出 `0`：零 importer |
| `grep -rn "internal/incident" --include=*.go .` | 1 | 无输出：生产与测试 Go 代码均零引用 |
| `grep -rn "incident\." --include=*_test.go internal \| grep -v internal/incident/` | 0 | 仅命中 SQL 字符串（`incident.opened` 等），非包调用 |
| `grep -rn "INSERT INTO notification_deliveries" internal/` | 0 | 生产仅 `internal/db/delivery.go:189`（`integration_id` 为 NULL）；其余为测试 fixture |

---

## 1. 角色启动图（`ALERTFLOW_ROLE`）

入口统一为 `backend/cmd/alertflow/main.go`。`-addr` 默认 `ALERTFLOW_ADDR` 或 `:8080`（main.go:38），`-config-dir` 默认空（main.go:39）；角色由 `envOr("ALERTFLOW_ROLE","server")` 决定（main.go:42），switch 分发 main.go:44–57，未知角色 `slog.Error("ALERTFLOW_ROLE must be server, monitor, publisher, consumer, delivery, or retire-deliveries")` 并 `os.Exit(1)`（main.go:55-57）。

| ALERTFLOW_ROLE | 入口函数 | 主要依赖构造 | 监听/端口 | 实际调用 package | 退出方式 |
| --- | --- | --- | --- | --- | --- |
| `server`（默认） | `runServer` main.go:61-97 | `resolveConfigDir`→`api.New(dir)` main.go:62-65 | HTTP `:8080`（`-addr`）；gRPC `ALERTFLOW_GRPC_ADDR` 默认 `:9090`（main.go:83） | `api`→`db/config/monitor/query/notify/cache/web` | SIGTERM/SIGINT 后 10s 优雅关闭（main.go:92-96） |
| `monitor` | `runMonitor` main.go:99-122 | `openRoleDB` main.go:101；`cache.New(REDIS_URL)` main.go:103（失败降级 nil）；Redis Ping main.go:108-112；`monitor.Loop{DB,Dir,Cache}` main.go:114 | 无监听 | `monitor`→`db/config/cache/query/diagnostics/observe/kubernetes/event` | 阻塞至 ctx 取消 |
| `publisher` | `runPublisher` main.go:124-139 | `openRoleDB`；`publish.New(database, ALERTFLOW_KAFKA_BROKERS, publish.Topic, "")` main.go:127 | 无监听（Kafka producer） | `publish`→`db`→Kafka topic `alert-events` | 阻塞至 ctx 取消 |
| `consumer` | `runConsumer` main.go:141-156 | `openRoleDB`；`consume.New(database, ALERTFLOW_KAFKA_BROKERS, publish.Topic, "")` main.go:144 | 无监听（Kafka consumer group `alertflow-incident`） | `consume`→`event`→`db` | 阻塞至 ctx 取消 |
| `delivery` | `runDelivery` main.go:172-198 | `openRoleDB` main.go:173；`registerAgentSender` main.go:175；`deliver.New(database,"")` main.go:176；`ALERTFLOW_AGENT_GRPC_TARGET` 非空才 `deliver.DialTasks` main.go:185 | 无监听（gRPC client→Python Agent） | `deliver`→`notify`/`db`/`query`/gRPC | target 空：warn+阻塞（main.go:179-183）；非空：`worker.Run` 阻塞 |
| `retire-deliveries` | `runRetireDeliveries` main.go:161-170 | `openRoleDB`；`database.CancelRetiredDeliveries(time.Now().Unix())` main.go:164 | 无 | `db` | 打印 `cancelled`/`running_left` 后退出 0 |

`registerAgentSender`（main.go:207-222）：`config.EnsureDir` 失败→空配置 `query.New`（main.go:210）；否则 `config.Load` 构建 `liveConfig{dir,current}`（main.go:217，热加载，失败回退最后良好快照，main.go:225-242）→`query.New(database, live.get)`（main.go:218）→`notify.Register(notify.AgentWebhookType, notify.AgentSender{Q: reads})`（main.go:219）。

`openRoleDB`（main.go:244-251）：`db.Open(os.Getenv("ALERTFLOW_DATABASE_URL"))`，失败 `slog.Error("startup failed")`+`os.Exit(1)`。

全量环境变量：`ALERTFLOW_ADDR`、`ALERTFLOW_ROLE`、`ALERTFLOW_CONFIG_DIR`、`ALERTFLOW_GRPC_ADDR`、`ALERTFLOW_AGENT_GRPC_TOKEN`、`ALERTFLOW_DATABASE_URL`、`REDIS_URL`、`ALERTFLOW_KAFKA_BROKERS`、`ALERTFLOW_AGENT_GRPC_TARGET`、`AGENT_TASK_GRPC_TOKEN`、`AGENT_BASE_URL`、`AGENT_API_TOKEN`、`KUBECONFIG`、`LOG_LEVEL`、`TIMELINE_RETENTION_DAYS`、`DB_MAX_OPEN`、`DB_MAX_IDLE`、`DB_CONN_MAX_LIFETIME`、`DB_CONN_MAX_IDLE_TIME`。

结论：**Composition Root 只做装配**（每个角色只构造依赖并 Run），无业务规则；`server` 同时承载 HTTP 与 gRPC（`ServeControl` → `backend/internal/api/grpc_control.go:31`）。判定 **KEEP**。

---

## 2. package 职责表

| package | 非测试文件/行 | 职责 | 判定 |
| --- | --- | --- | --- |
| `cmd/alertflow` | 1 / 275 | 唯一进程入口，角色分发与环境组装 | KEEP（B4 补 smoke） |
| `internal/api` | 18 / 3441 | HTTP API + gRPC 控制面（读接口、agent callback、chat、manual incident） | KEEP |
| `internal/archive` | 1 / 272 | 归档/保留期清理 | KEEP |
| `internal/auth` | 1 / 42 | 登录鉴权 | KEEP |
| `internal/cache` | 1 / 347 | Redis 读缓存 | KEEP |
| `internal/config` | 3 / 637 | 配置加载/校验/落盘 | KEEP |
| `internal/consume` | 1 / 202 | Kafka 消费循环，单事务入口 `Handle` | KEEP |
| `internal/db` | 17 / 4109 | 持久化与事务边界（checks/outbox/incidents/timeline/deliveries/agent 报告） | KEEP（B3 注释） |
| `internal/deliver` | 2 / 436 | Delivery Worker（lease/retry）+ gRPC task client | KEEP（B1 薄改造） |
| `internal/diagnostics` | 2 / 517 | 诊断采集（HTTP/TLS 等） | KEEP |
| `internal/event` | 2 / 110 | `CheckCompleted` 事件契约 + `Decide` 状态机 | KEEP |
| `internal/image` | 1 / 101 | 图标/图片处理 | KEEP |
| `internal/incident` | 1 / 84 | 旧 incident 转换规则（直写 DB） | **LEGACY CANDIDATE**（B2 删除） |
| `internal/logger` | 1 / 37 | slog 初始化 | KEEP |
| `internal/monitor` | 3 / 641 | 检查调度、worker、结果落库 | KEEP |
| `internal/notify` | 4 / 680 | 通知适配器：Agent webhook HTTP sender + Dispatcher | REFACTOR（B1：envelope 构建外移） |
| `internal/observe` | 2 / 1095 | 观测采集（含 kubernetes） | KEEP |
| `internal/publish` | 2 / 261 | Outbox→Kafka 发布 | KEEP |
| `internal/query` | 5 / 1125 | 只读查询 + `DiagnosticContext` 构建 | KEEP |
| `internal/state` | 1 / 50 | CLI 状态（config dir 记忆） | KEEP |
| `internal/web` | 1 / 19 | 内嵌前端资源 | KEEP |

目标态新增 `internal/agentmessage`（B1，REFACTOR）：transport-independent 的 Agent v2 envelope builder，供 HTTP（notify）与 gRPC（deliver）共用。

---

## 3. 业务事实表写入者与唯一事务入口

| 业务事实 | 表 | 生产写入者 | 唯一事务入口 | 备注 |
| --- | --- | --- | --- | --- |
| 检查结果 + 待发布事件 | `checks`、`outbox_events` | `db.RecordCheckAndOutboxPresent`（outbox.go:46，`RecordCheckAndOutbox` outbox.go:38 转发） | 单事务（outbox.go:46），调用点 `monitor/scheduler.go:240`（`finishCycle` scheduler.go:221） | 同一事务内 `INSERT checks`+`INSERT outbox_events` |
| 自动事件处理幂等 | `processed_events` | `rememberProcessed`（apply.go:219） | `applyCheckEventTx`（apply.go:73）内 | 重复事件返回 `ErrAlreadyProcessed` |
| 服务运行态 | `service_runtime_state` | `applyCheckEventTx`（apply.go:73） | `db.ApplyCheckEvent`（apply.go:50）单事务 | 与 incident 决策同事务 |
| 自动 Incident | `incidents` | `insertOngoingTx`（apply.go:262）/`resolveOngoingTx`（apply.go:279）/`insertWarningTx`（apply.go:293） | `db.ApplyCheckEvent`（apply.go:50）单事务 | 每个服务最多一个 ongoing（DB 部分唯一索引） |
| 人工 Incident | `incidents` | `db.CreateManualIncident`（timeline.go:94） | 该函数自身单事务 | API `POST /api/incidents`（handlers_incidents.go:92） |
| 时间线 | `runtime_events` | `insertTimelineTx`（timeline.go:47） | 由调用方事务提供：apply.go（自动）、timeline.go:94（人工） | 自动路径 key `incident:{id}:opened/resolved`，`origin="check"` |
| Agent task（内部投递） | `notification_deliveries`（`integration_id IS NULL`） | `db.InsertInternalAgentTask`（delivery.go:186） | 依赖调用方事务 | 调用点：apply.go:343、timeline.go:158、agent_chat.go:89 |
| 投递结果日志（历史） | `notification_log` | `finishDelivery`（delivery.go:203） | `SucceedDelivery`/`FailDelivery`（delivery.go:164/169） | 历史命名，保留 |
| 历史渠道投递 | `notification_deliveries`（`integration_id IS NOT NULL`） | **无生产写入者**（仅测试 fixture） | — | 见 §5 与 §8 判定 |
| Agent 调查/chat 结果 | `agent_runs` | `db.SaveAgentRun`（agent_reports.go:45，SQL agent_reports.go:64） | 该函数自身事务 | gRPC `SubmitAgentCallback`（grpc_control.go:300）调用（:317） |
| Agent chat 请求 | `agent_chat_requests` | `db.CreateChatRequest`（agent_chat.go:55） | 该函数单事务（含投递行） | 回调 `CompleteChatRequest`（:204）/`FailChatRequest`（:222） |
| 通知渠道配置 | `integrations` | `db`（integrations.go:45/61/72） | `integrations.go` 相应方法 | 由 API 层调用 |
| Outbox 发布状态 | `outbox_events` | `MarkOutboxPublished`（outbox.go:404）/`RecordOutboxFailure`（outbox.go:378） | `PublishUnderServiceLock`（outbox.go:197） | 发布成功/失败均只在此 |

---

## 4. CheckCompleted 端到端调用图

```
monitor.Scheduler.run (scheduler.go:175)
  └─ check (scheduler.go:196) → runCycle (scheduler.go:285)
      └─ finishCycle (scheduler.go:221)
          └─ event.NewCheckCompleted (event/check.go:41)
          └─ db.RecordCheckAndOutboxPresent (db/outbox.go:46)   [单事务: checks + outbox_events]
          └─ OnCheckStored(serviceID) (scheduler.go:249)
publish.Publisher.Run (publish/publish.go:73)
  └─ ClaimOutbox (db/outbox.go:327) / PublishUnderServiceLock (outbox.go:197)
      └─ MarkOutboxPublished (outbox.go:404) → Kafka topic "alert-events"
consume.Consumer.Run (consume/consume.go:59) → ConsumeOne (consume.go:89)
  └─ Handle (consume/consume.go:121) → db.ApplyCheckEvent (db/apply.go:50) [单事务]
      └─ applyCheckEventTx (db/apply.go:73)
          ├─ rememberProcessed (apply.go:219)          → processed_events
          ├─ event.Decide (event/decide.go:18)          → TransitionOpen/Resolve/Warning
          ├─ insertOngoingTx/resolveOngoingTx/insertWarningTx  → incidents
          ├─ insertTimelineTx (db/timeline.go:47)       → runtime_events (origin="check")
          └─ insertDeliveriesTx (apply.go:315)          → InsertInternalAgentTask (delivery.go:186)
                                                            → notification_deliveries (integration_id NULL)
deliver.Worker.Run (deliver/deliver.go:71) → ProcessDue (deliver.go:95)
  └─ ClaimDeliveries (db/delivery.go:59)   [仅 integration_id IS NULL]
      └─ GRPCTasks.Accept (deliver/grpc_tasks.go:46) → Python AgentTaskService
                                                       StartInvestigation / StartChat
```

要点：
- `event.Decide`（event/decide.go:18）是唯一状态机；`CheckCompleted` 契约 `event/check.go:20`，schema_version=1（`event/check.go:10`）。
- Consumer group 固定 `alertflow-incident`（consume.go:22）；重复事件按已处理提交，失败不提交（consume.go:121-148）。
- 发布 topic 与分区：`alert-events`、`Partitions=3`、lease 30s（publish/publish.go:22 起）。
- `RecordCheckAndOutboxPresent` 是 checks/outbox 的唯一事务入口（outbox.go:46）；monitor 无第二条写入路径。

---

## 5. `notification_deliveries` 两类 row

定义见 migration `internal/db/migrations/00017_internal_agent_tasks.sql`（`integration_id` DROP NOT NULL + 部分唯一索引 `(event_id,event_type) WHERE integration_id IS NULL`）。

### 5.1 `integration_id IS NULL`：内部 Agent task（当前唯一活跃路径）

| 环节 | 位置 |
| --- | --- |
| 插入 | `db.InsertInternalAgentTask`（delivery.go:186）：`INSERT ... integration_id NULL ... ON CONFLICT (event_id,event_type) WHERE integration_id IS NULL DO NOTHING` |
| 插入调用者 | `apply.go:343`（自动 incident_start/incident_resolve/warning）、`timeline.go:158`（人工 incident_start）、`agent_chat.go:89`（chat_requested） |
| 领取 | `db.ClaimDeliveries`（delivery.go:59），两处 SQL 条件 `integration_id IS NULL`（delivery.go:78、delivery.go:94） |
| 执行 | `deliver.Worker.sendOne`（deliver.go:116）→ `GRPCTasks.Accept`（grpc_tasks.go:46）→ Python `AgentTaskService` |
| 结果 | `SucceedDelivery`/`FailDelivery`（delivery.go:164/169）→ `finishDelivery`（delivery.go:203）写 `notification_log`，行状态 `succeeded/retry_wait/dead` |
| 启停 | 仅当 `ALERTFLOW_AGENT_GRPC_TARGET` 非空才插入（`AgentTasksEnabled` delivery.go:180）与执行 |

### 5.2 `integration_id IS NOT NULL`：历史渠道行（无执行者）

- 生产代码零插入点：全仓 `INSERT INTO notification_deliveries` 仅 `delivery.go:189`（NULL 插入）；非 NULL 行只出现在测试 fixture（`deliver/deliver_test.go:356`、`db/delivery_retire_test.go:16/54`、`db/delivery_fk_test.go:122`）。
- `ClaimDeliveries` 已从 SQL 层排除（delivery.go:78/94），因此 `deliver.go:123` 的 `IntegrationID != 0` 分支（查 `integrations` 取 type/config）当前不可达；`sendOne` 的历史 `SenderFor(IntegrationType).Send` 分支（`deliver.go:134-146`）同样不可达。
- 历史行只由 `CancelRetiredDeliveries`（delivery.go:360，`retire-deliveries` 角色调用，main.go:164）批量标记：pending/retry_wait→cancelled，running→写 `retired_delivery_audits`；注释明确不在 server 启动时执行。
- 现存 sender 只有 `agent_webhook` 一个（api/server.go:114、cmd/alertflow/main.go:219）；`notify.Dispatcher`（notify/dispatcher.go）只服务“发送测试通知”接口（`s.dispatch()` api/server.go:307 的 Test），不是第二自动通道。
- 判定：**LEGACY CANDIDATE（表名与列语义）**，B3 仅加注释，不迁移、不改名、不恢复渠道执行。

---

## 6. Agent v2 消息路径（B1 依据）

### 6.1 生产调用图

```
notify.AgentSender.Send (notify/agentwebhook.go:123)          HTTP 适配器
  ├─ Chat != nil → sendChat (agentwebhook.go:160) → postAgentJSON (:190)
  └─ 否则 build (:217) → encodeAgentMessage (:368，超限跑 agentTrimSteps :404)
        → postAgentJSON (:190)

deliver.GRPCTasks.Accept (deliver/grpc_tasks.go:46)           gRPC 适配器
  └─ notify.AgentSender{Q}.Encode (notify/agentwebhook.go:327)
      ├─ Chat != nil → 内联重复组装 chat envelope
      └─ 否则 build + encodeAgentMessage
  → json.Unmarshal envelope → protojson → StartInvestigation / StartChat
```

- `AgentSender.Send/Encode` 的注册点：`api/server.go:114`（server 进程）、`cmd/alertflow/main.go:219`（delivery 进程）。
- `Encode` 唯一调用点：`deliver/grpc_tasks.go:52`。
- `Dispatcher.Test` 只走 `Sender.Send`，不写 `notification_log`（notify/dispatcher.go）。

### 6.2 必须保真的输出语义（B1 验收）

| 项 | 当前实现 |
| --- | --- |
| `schema_version` | `"v2"`（agentwebhook.go:31） |
| `message_id` | IncidentID≠0→`{event}:{incidentID}`；否则 DeliveryID≠0→`delivery:{id}`；否则 `{event}`（:291） |
| `correlation_id` | `incident:{id}` / `delivery:{id}`（:301） |
| `event_id` | 规则同 `message_id`（:308） |
| `message_type` | `diagnostic.requested` / `diagnostic.resolved` / `diagnostic.warning` / `diagnostic.test` / `chat.requested`（:39 起；`IncidentID==0`→test 但 warning 事件仍为 warning） |
| `sent_at` | `time.Now().UTC().Format(time.RFC3339)`（:217/:160） |
| `occurred_at` / `started_at` / `resolved_at` | warning→occurred；resolve→resolved（缺省 occurred）；started_at 非零才填；resolved_at 非 nil 才填 |
| `status` | `down` / `warning` / `recovered`（:315） |
| `severity` | `warning` 事件→`warning`，否则 `outage`（notify.go:111） |
| `diagnostic_context` | 仅 IncidentID≠0 且非 warning 时调 `query.DiagnosticContext`（agentwebhook.go:275）；失败把 `" diagnostic context unavailable: "+err` 追加到 `event.error` |
| body 上限 | `AgentMaxPayloadBytes = 512 << 10`（:36） |
| 裁剪顺序 | 8 步 `agentTrimSteps`（:404）：Comments>10 留后 10 → Related>5 留前 5 → Metrics.Series>96 留后 96 → RecentChecks>50 留后 50 → Comments 清空 → RecentChecks>10 留后 10 → Metrics.Series 清空 → Related 清空；每步 `markTruncated`（:455）去重追加 `coverage.truncated` 并置 `coverage.complete=false` |
| chat 上限 | 正文 `clipRunes` 4000 字符（:153/:206）；整包 ≤512KiB |
| 错误文案 | `agent_webhook: url is required`（:129）、`agent_webhook: query service is not configured`（:132/:329）、`agent_webhook: event payload is %d bytes, over the %d limit`（:368 起）、`agent_webhook: diagnostic context is %d bytes, over the %d limit`、`agent_webhook: chat request is %d bytes, over the %d limit` |

### 6.3 重复入口（B1 需消除）

| 重复项 | 位置 | 处理 |
| --- | --- | --- |
| chat envelope 组装两份 | `sendChat`（:160）与 `Encode` 内联分支（:327 起） | 收敛到 `agentmessage.BuildChat/EncodeChat` |
| 构建逻辑被 HTTP/gRPC 各调一次同名方法 | `Send` 与 `Encode` | 收敛到 `agentmessage`，两适配器仅做 transport 转换 |
| `grpc_tasks.go:52-58` 冗余 `if err != nil { if permanent(err){return err}; return err }` | 两支相同 | B1 顺手简化为 `return err`（不改变错误语义） |

判定：**REFACTOR**（B1，最小变体：纯构建函数 + 两处薄转换；不整体搬 notify 类型、不改 JSON 字节）。

---

## 7. `query.DiagnosticContext` 唯一构建入口

- 结构体 `DiagnosticContext`：`internal/query/context.go:54`；构建函数 `func (s *Service) DiagnosticContext(opts ContextOptions) (*DiagnosticContext, error)`：**`internal/query/context.go:192`（唯一）**。
- 入参 `ContextOptions{IncidentID, EventTime, CheckLookbackMinutes}`（context.go:179）。
- 生产调用点：**仅** `internal/notify/agentwebhook.go:275`；测试 `internal/query/context_test.go`。
- 时间窗锚定 `EventTime` 而非 `now`；上限常量 context.go:14/30/38（`MaxContextChecks=200`、`MaxContextMetricPoints=200`、`MaxContextComments=50`、`MaxContextRelatedIncidents=10`、`MaxContextTextRunes=2000`、`MaxContextLookbackHours=24`、`DefaultCheckLookbackMinutes=60`、`contextScanPages=20`）。
- `query.New` 生产调用点：`api/server.go:106`、`monitor/run.go:41`、`cmd/alertflow/main.go:210` 与 `:218`（同属 delivery 角色）。
- 判定：**KEEP**（B1 后 `agentmessage` 成为其唯一调用者，语义不变）。

---

## 8. `internal/incident` 引用审计（B2 依据）

文件：`backend/internal/incident/incident.go`（84 行）、`backend/internal/incident/incident_test.go`（160 行）。

三证据：

1. **静态引用**：`grep -rn "internal/incident" --include=*.go backend` 无输出；`go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./... | grep '|.*internal/incident'` 计数 **0**。生产与测试 Go 代码零 import。
2. **编译**：`go build ./...`、`go vet ./...` 退出 0；删除前后 `go build` 结果不受影响（包未被依赖）。
3. **测试**：`go test -count=1 ./internal/incident` 通过（14.86s），仅证明包自洽，无外部调用者。

语义：`incident.go` 文件头注释自认“检查调度器不调用它，Kafka consumer 通过 `event.Decide` 在同一数据库事务中应用同一规则，不调用 `OnTransition`”。`OnTransition`（incident.go 内）直接 `db.CreateIncident`/`db.ResolveOngoingIncident`/`db.CreateWarningEvent`，与生产路径 `db.ApplyCheckEvent`→`event.Decide`（apply.go:73）重复。

全仓非 Go 引用（不在写域，供 lead 处理）：
- `docs/ARCHITECTURE.md:164`（“It does not call `OnTransition`”）、`:168-171`（转换矩阵表格 `CreateIncident`/`CreateWarningEvent`/`ResolveOngoingIncident`/`InitialStatus` 文案）、`:265`、`:278`（“Why `incident` is its own package”）、`:281`、`:283-284`、`:288`。
- `docs/BASELINE.md:59`（旧名 `upmonitor/internal/incident` 测试输出）；`ALERTFLOW_CURSOR_PLAN (1).md:138,146`。

结论：无动态调用、无反射调用（`grep -rn "reflect" internal/incident` 无输出）、无间接依赖、无生产入口 → **LEGACY CANDIDATE，可整体删除**。

执行结果（B2）：`backend/internal/incident/` 已整体删除。删除后复验：`go build ./...` 与 `go vet ./...` 均退出 0；`go list -f '{{.ImportPath}} {{join .Imports " "}}' ./...` 与 `go list -f ... .TestImports ...` 中 importer 计数均为 0；`grep -rn "OnTransition|InitialStatus" --include=*.go` 无生产命中。非 Go 文档引用（`docs/ARCHITECTURE.md` 等，属 lead 写域）已单独发消息告知需修改的行号。

---

## 9. 重复入口核对表

| 业务动作 | 入口 A（生产） | 入口 B | 核对结论 |
| --- | --- | --- | --- |
| 自动 incident 状态转换 | `db.ApplyCheckEvent`（apply.go:50）+`event.Decide`（decide.go:18） | `incident.OnTransition`（incident.go，死代码） | B：无调用者，删（B2） |
| Agent v2 envelope 构建 | `notify.AgentSender.Send`（HTTP） | `notify.AgentSender.Encode`（gRPC，grpc_tasks.go:52） | 两份实现，收敛到 `agentmessage`（B1） |
| chat envelope 构建 | `sendChat`（agentwebhook.go:160） | `Encode` 内联 chat 分支 | 合并（B1） |
| Delivery claim | `db.ClaimDeliveries`（delivery.go:59，仅 NULL） | 历史 `integrations` 分支（delivery.go:139 起） | B：SQL 已排除，不可达；注释（B3） |
| Agent sender 注册 | `api/server.go:114`（server） | `cmd/alertflow/main.go:219`（delivery） | 两进程各注册一次，同类型 `AgentSender`；KEEP（各进程独立） |
| 通知派发 | `notify.Dispatcher.Test`（测试通知） | `deliver.Worker`（自动投递） | 仅测试接口；非第二自动通道，KEEP |
| Incident 写入（人工） | `db.CreateManualIncident`（timeline.go:94） | — | 唯一，KEEP |

---

## 10. 未实现 / 部分实现清单

| 项 | 证据 | 判定 |
| --- | --- | --- |
| `a2a_adapter_tasks`、`a2a_task_aliases` 两张表 | migration 00018/00019 存在；Go 代码 `grep -rn "a2a\|A2A" backend --include=*.go` 零命中 | UNVERIFIED（未实现/历史遗留，本批不动） |
| `notification_deliveries.integration_id IS NOT NULL` 执行路径 | 无生产插入点、Claim 排除、sender 仅 agent_webhook | LEGACY CANDIDATE（B3 注释） |
| 旧同步 Agent chat 代理 `AGENT_BASE_URL`/`AGENT_API_TOKEN` | `api/server.go:59-63` 字段 + `handlers_agent_chat.go:233/255/261` | 部分实现（chat 页面用），KEEP 现状 |
| `internal/incident` | 零 import | LEGACY CANDIDATE（B2 删） |
| 角色启动 smoke | `cmd/alertflow/main.go` `-smoke` + `cmd/alertflow/main_test.go` `TestRoleSmoke` | 已实现（B4） |
| Makefile 角色/smoke 入口 | 仓库根 Makefile 无对应 target | 待 lead 补（B4 已发消息给出建议 target） |
| docs 中 `internal/incident`、a2a 表述 | ARCHITECTURE.md 多处；BASELINE.md:59 | 过时（lead 处理） |
| `TestCrossLanguageGRPC` 默认环境 | 默认 `python3` 无 grpc 模块；venv 下 :330 期望 `DEADLINE_EXCEEDED` 得 `UNAVAILABLE` | UNVERIFIED（环境问题，非本批回归） |

---

## 11. 判定汇总

### 11.1 KEEP
角色装配（main.go 各 `runX`）、`db.ApplyCheckEvent` 单事务入口、`event.Decide` 唯一状态机、Outbox 发布链路、`ClaimDeliveries` 只领 NULL、`query.DiagnosticContext` 唯一入口、Delivery lease/retry 语义、gRPC control 面。

### 11.2 REFACTOR（B1）
`notify.AgentSender` 的 envelope 构建外移到 `internal/agentmessage`；HTTP/GrPC 两处薄转换；JSON 字节不变（golden 测试证明）。

### 11.3 LEGACY CANDIDATE
`internal/incident`（删除，B2）；`notification_deliveries` 历史命名与 `integration_id IS NOT NULL` 分支（仅注释，B3）。

### 11.4 UNVERIFIED / 环境限制
- `TestCrossLanguageGRPC`（`backend/internal/api/grpc_cross_test.go:41`）：默认 `python3` 无 `grpc` 模块（`ModuleNotFoundError: No module named 'grpc'`，harness 报 `read harness: EOF`）；改用 `ALERTFLOW_PYTHON=agent/.venv/bin/python` 后可跑通至 `grpc_cross_test.go:330`，失败：`deadline = map[code:UNAVAILABLE]`（期望 `DEADLINE_EXCEEDED`；python 侧 `agent/tests/cross_grpc.py:243-249` 以 `timeout=0.2` 调 `timeline`，Go 侧 slow 服务 `serveSlowTimeline` 在 `grpc_cross_test.go:383-396`）。该失败在改动前即存在，属环境问题。
- CI（`.github/workflows/test.yml:70`）只执行 `go test ./...`，未设置 `ALERTFLOW_PYTHON`。

---

## 12. 后续批次对照（B1–B4）

| 批次 | 目标 | 本审计依据 |
| --- | --- | --- |
| B1 | `internal/agentmessage`：`BuildDiagnostic`/`EncodeDiagnostic`/`BuildChat`/`EncodeChat`；notify/deliver 薄转换；golden 字节等价测试 | §6 |
| B2 | 删除 `internal/incident`；通知 lead 改 `docs/ARCHITECTURE.md` 精确行号（:164、:168-171、:265、:278-288） | §8 |
| B3 | `notification_deliveries` 历史命名 + `integration_id != 0` 不可达分支注释 | §5、§9 |
| B4 | 每个 `ALERTFLOW_ROLE` 的 `-smoke` 构造级启动测试；Makefile 入口（lead） | §1 |


---

## 13. 执行结果（B1–B4）

- **B1 完成**：新增 `backend/internal/agentmessage/{agentmessage.go,encode.go}`（transport-independent builder，只依赖 `query`）；`notify.AgentSender` 变薄 adapter（`Send` 调 `agentmessage.Encode`，`AgentMessageInputs` 做 `notify.Message → DiagnosticInput/ChatInput` 映射），`notify.AgentSender.Encode` 已删除；`deliver/grpc_tasks.go` 的 `Accept` 改调 `agentmessage.Encode`，并删除原 `if permanent(err){...}` 冗余分支。字节等价由 `backend/internal/notify/agentwebhook_golden_test.go` + `testdata/agentv2/`（7 个场景，重构前 a5ca5d2 实现捕获，固定时钟）证明，7/7 PASS。
- **B2 完成**：`backend/internal/incident/` 已整体删除；删除前后 `go build ./...`、`go vet ./...` 均 0；包清单与 `TestImports` 两路 importer 计数 0。`docs/ARCHITECTURE.md` 的过时引用属 lead 写域，已发消息列出精确行号。
- **B3 完成**：`db/delivery.go`（`ClaimDeliveries` 历史分支、`InsertInternalAgentTask` 表名语义）与 `deliver/deliver.go`（`sendOne` 旧渠道 `SenderFor` 分支）只加中文注释，行为、表名、迁移均未改。
- **B4 完成**：`cmd/alertflow/main.go` 增加 `-smoke`（构造依赖后退出 0，不监听、不循环），`runServer/runMonitor/runPublisher/runConsumer/runDelivery/runRetireDeliveries` 增加 `smoke bool` 参数；`cmd/alertflow/main_test.go` 的 `TestRoleSmoke` 用临时库逐角色验证。实跑 6 个角色 `-smoke` 退出码均为 0。附带修复：`api.Server.Close` 现在等待 retention goroutine 退出后再关库，消除关闭瞬间的 `sql: database is closed` 噪声日志（仅关闭顺序，不影响服务行为）。Makefile 入口待 lead 添加。
