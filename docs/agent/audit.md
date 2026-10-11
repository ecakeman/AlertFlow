# Agent P0 审计

日期：2026-10-11。只读代码，未改 `backend/`。Q1、Q4、Q6 没有在本环境端到端跑通，原因写在对应条。没有编造接收到的 payload。

## Q1 投递链路

代码路径是通的：scheduler 调用 `RecordCheckAndOutbox`（`backend/internal/monitor/scheduler.go`），publisher 把 `outbox_events` 发到 `alert-events`，consumer 的 `Handle` 按 `event_type` 调用 `ApplyCheckEvent` 或 `ApplyExternalAlert`，delivery 用 `SenderFor` 发送。`docker compose up` 没有在这次审计里重新拉起全栈。以往构建停在 `registry-1.docker.io` 解析 `docker.io/docker/dockerfile:1` 超时。因此没有贴出真实的 `incident_start` / `incident_resolve` / `warning` HTTP 正文。

## Q2 默认 Webhook 字段

审计当时，`bodyTemplate` 为空的默认 JSON 只有下面这些字段。当前代码已经加上契约字段，见本文件末尾“当前代码”。

`backend/internal/notify/webhook.go` 的 `renderWebhookBody` 在 `bodyTemplate` 为空时输出：

- `event`：`incident_start`、`incident_resolve` 或 `warning`（`notify.Event`）
- `service`：服务显示名
- `url`：服务 URL
- `status`：`down`、`recovered` 或 `warning`。这不是检查状态 `online/offline`
- `message`：`Message.Body()` 的英文句子
- `attempts`：只有 warning 才有

没有 `schema_version`、`event_id`、`incident_id`、`check_id`、`service_id`、`occurred_at`、`started_at`、`resolved_at`、`error`。

`bodyTemplate` 非空时，模板的数据是整个 `notify.Message`：`Event`、`IncidentID`、`ServiceID`、`ServiceName`、`ServiceURL`、`StartedAt`、`ResolvedAt`、`Attempts`、`Error`，以及方法 `Subject`、`Body`、`Down`、`Warning`。自定义模板可以变成任意正文，不能当成 Agent 契约。

## Q3 快照里有什么

`insertDeliveriesTx`（`backend/internal/db/apply.go`）写入 `DeliverySnapshot`：`event`、`eventId`（Kafka 事件 id，不是 `<event>:<incident_id>`）、`incidentId`、`serviceId`、`serviceName`、`serviceUrl`、`startedAt`、`resolvedAt`、`attempts`、`error`。

`check_id` 不在快照里。检查事件的 payload 里有 `check_id`，但抄进通知时被丢掉。外部告警没有 check。

`startedAt` 来自事件的 `OccurredAt`，是故障或检查周期的时间，不是 `sent_at`。warning 的投递行有 `incident_id`（那条已解决的 warning 记录）；`notification_log.incident_id` 对 warning 仍是 NULL。

## Q4 重试是否同一正文

没有真实 HTTP 抓包。代码上，delivery 重试同一 `notification_deliveries` 行，发送时再读当前 `integrations.config`，payload 列不改。因此同一行的默认 JSON 在配置不变时逐字节相同。现在的默认 JSON 不含稳定的契约 `event_id`；快照里的 `eventId` 是检查或外部事件的 id，同一次通知的重试不会换这个值。

## Q5 四个 GET 与 token

| 路由 | 处理函数 | 是否读登录用户 |
| --- | --- | --- |
| `GET /api/incidents` | `handleListIncidents` | 否 |
| `GET /api/incidents/:id` | `handleGetIncident` | 否 |
| `GET /api/services/:id/metrics` | `handleServiceMetrics` | 否 |
| `GET /api/services/:id/checks` | `handleServiceChecks` | 否 |

`userLocal` 只在创建、更新事故时可选地写入 `createdBy`。`ALERTFLOW_WEBHOOK_TOKEN` 在 `handleAlertWebhook`：未配置返回 503，错误 Bearer 返回 401，比较用 `crypto/subtle`。内部 API 应复制这个比较，使用另一个变量 `ALERTFLOW_AGENT_TOKEN`。

未知 `/api/` 由 `serveSPA` 返回 JSON 404。审计当时 `/internal/` 会落到 SPA 并返回 HTML。当前 `/internal/agent/v1` 未命中的路径返回 JSON 404。

## Q6 error 文案

没有在这次审计里对五个故障各插一条检查。分类正则要等真实样本再改。代码能确定的是：传输失败时 `error` 是 `http.Client` 的 `err.Error()`，并带上耗时；状态码不在 `ExpectedStatus` 时，`error` 是 `http.StatusText(code)`，例如 `Service Unavailable`。warning 行的 `error` 是第一次失败的文本，`latency_ms` 和 `status_code` 来自成功的那次。

## Q7 latencyMs

`monitor.Check` 在 `client.Do` 前后用 `time.Since`。这是单次请求从发出到响应头返回的毫秒数，不是多次重试的合计。请求构造失败时没有延迟。offline 行也可以有延迟。周期开始时间存在 `checks.ts`，不是这条延迟。

## Q8 外部告警

`ApplyExternalAlert` 可以在没有进行中事故时打开一条，并写 `incident_start`。已有进行中事故时，新的 firing 只更新告警行，不再开一条，也不再发开始通知。`resolved` 只有在 HTTP 状态不是 `offline` 且没有其他 firing 告警时才关闭事故。检查从 offline 恢复时，若还有 firing 告警，不关闭事故，但会更新 `service_runtime_state`。

详情接口的重叠规则在 `ListExternalAlertsOverlapping`：`opened_at <= 结束` 且（未恢复或 `resolved_at >= 事故开始`）。进行中的结束时间是查询时的现在。

## Q9 文档与 Compose

`docs/ARCHITECTURE.md` §3 写的是 scheduler 不发送，delivery 角色发送。这和代码一致，不是“此阶段不调用 sender”的过时句。§4 已写明 HTTP offline 或外部告警 firing 都会让事故保持打开。

`docker compose config --quiet` 在 Phase 5 时通过。这次没有重新 `docker compose up`。应用镜像构建仍可能被 Docker Hub 超时挡住。

## 对规格 §4.1 的修改建议

保留现有字段，不要把 `status` 改成 `offline`。建议默认 JSON 变成：

```diff
 {
   "event": "incident_start",
   "service": "Order API",
   "url": "https://example.com/health",
   "status": "down",
   "message": "Order API (https://example.com/health) went down at ...",
+  "schema_version": "v1",
+  "event_id": "incident_start:123",
+  "incident_id": 123,
+  "check_id": 4821,
+  "service_id": "order-api",
+  "service_name": "Order API",
+  "occurred_at": "2026-10-11T10:00:00Z",
+  "started_at": "2026-10-11T10:00:00Z",
+  "resolved_at": null,
+  "severity": "outage",
+  "attempts": 3,
+  "error": "connection refused"
 }
```

`event_id` 用 `<event>:<incident_id>`。快照没有 incident 时用 `delivery:<notification_deliveries.id>`。`check_id` 只在检查触发时出现。`occurred_at` 是这次事件的时间，`started_at` 是故障开始时间；warning 不带 `started_at` 或 `resolved_at`。`resolved_at` 只在恢复事件上有值，并与该事件的 `occurred_at` 相同。

§4.5 的错误分类先保持规格中的正则。`http.StatusText` 的 `Service Unavailable` 不会命中 `http_status` 正则，除非分类同时看 `status_code`。P1 不改分类；Agent 分析应同时看 `status_code`。

## 当前代码

上面 Q2 和 Q5 记录的是 2026-10-11 审计时的代码。当前 `renderWebhookBody` 在 `bodyTemplate` 为空时已经输出 `schema_version`、`event_id`、`service_id`、`service_name`、`occurred_at` 和 `severity`。`started_at` 在 warning 上省略，`resolved_at` 只在恢复事件上出现并等于该事件的 `occurred_at`。`/internal/agent/v1` 的未知路径返回 JSON 404。同一秒的外部告警仍不能判断先后。
