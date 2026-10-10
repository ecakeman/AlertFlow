# 阶段 0 基线审计

阶段 1 的 PostgreSQL 迁移记录在 [PHASE1.md](PHASE1.md)。下文保留审计当时的事实，不随后续阶段改写。

审计日期：2026-10-10  
审计对象：本地 `main` 上的上游提交，不包含任何 AlertFlow 业务改动。

## 仓库状态

| 项 | 值 |
|---|---|
| 工作目录 | `/home/sancho/projects/AlertFlow` |
| HEAD | `46a00d5f9352d9fc267dfca0bcecd1d78af08279` |
| 提交说明 | `ci: build the web stage natively to avoid QEMU-emulated npm` |
| 标签 | `v1.0.9` |
| 本地分支 | `main`，未设置 upstream，避免默认推送到原作者仓库 |
| `origin` | `https://github.com/ecakeman/AlertFlow.git`（fetch/push） |
| `upstream` | `https://github.com/antlko/upmonitor.git`（fetch/push URL 存在，本阶段未推送） |
| 许可证 | MIT，Copyright (c) 2026 Anatol Kozhukhar，见 `LICENSE` |

审计开始前，该目录不是 Git 仓库，只有未跟踪的规划文件 `ALERTFLOW_CURSOR_PLAN (1).md`。`ecakeman/AlertFlow` 当时是空仓库（`size=0`，`git ls-remote` 无引用，默认分支名 `main`）。上游 `antlko/upmonitor` 的 `master` 就是上述 SHA。

纳入方式：`git init` 后 fetch `upstream`，用同一提交创建本地 `main`。没有改写历史，没有 force push，没有 `git push`。规划文件仍保持未跟踪。

本阶段结束后未再次成功访问 GitHub（一次 `git ls-remote origin` 挂起后被中止）。未执行推送，因此不能把远程说成已同步；审计开始时该远程没有提交。

## 版本

| 工具 | 本机 | 仓库声明 |
|---|---|---|
| Go | `go1.26.6 linux/amd64` | `backend/go.mod`：`go 1.25.7` |
| Node | `v22.23.1` | `web-ui/package.json` engines：`^22.18.0 \|\| >=24.12.0` |
| npm | `10.9.8` | 锁定文件 `web-ui/package-lock.json` |
| Docker | `29.6.1` | — |
| Compose | `v5.2.0` | `docker-compose.yml` 使用 `ghcr.io/antlko/upmonitor:latest` |
| 前端包 | — | `web-ui` `0.0.0`，Vue `^3.5.38`，Vite `^8.0.16` |
| 后端模块 | — | `module upmonitor`；Fiber v3、Goose v3、`modernc.org/sqlite v1.53.0` |

`gh` 未安装。CI 只有 [`.github/workflows/docker-publish.yml`](../.github/workflows/docker-publish.yml)：在 tag `v*` 或手动触发时构建并推送 `ghcr.io/antlko/upmonitor`。没有 Go 测试或前端构建的 CI。

## 命令与结果

### Go 测试：通过

```text
cd backend && go test ./...
```

退出码 0。首次运行下载了模块。结果：

```text
?    upmonitor/cmd/upmonitor          [no test files]
ok   upmonitor/internal/api           0.006s
ok   upmonitor/internal/archive       0.515s
?    upmonitor/internal/auth          [no test files]
ok   upmonitor/internal/config        0.006s
ok   upmonitor/internal/db            0.971s
?    upmonitor/internal/image         [no test files]
ok   upmonitor/internal/incident      0.902s
?    upmonitor/internal/logger        [no test files]
ok   upmonitor/internal/monitor       0.162s
ok   upmonitor/internal/notify        0.308s
?    upmonitor/internal/state         [no test files]
?    upmonitor/internal/web           [no test files]
```

没有失败包。`auth`、`image`、`logger`、`state`、`web`、`cmd/upmonitor` 没有测试文件。

### 前端构建：通过

```text
cd web-ui && npm ci && npm run build
```

退出码 0。`npm ci` 安装 355 个包。`npm run build` 同时执行 `vue-tsc --build` 与 `vite build`，2589 个模块，`✓ built in 2.06s`。

### 本地启动：通过（开发模式，不是容器）

按 [`docs/DEVELOPMENT.md`](DEVELOPMENT.md)，没有拉取或构建 `ghcr.io/antlko/upmonitor:latest`。Docker 可用，但本阶段没有验证镜像启动。

```text
cd backend && go run ./cmd/upmonitor --config-dir /tmp/alertflow-baseline-config --addr :8080
cd web-ui && npm run dev -- --host 127.0.0.1 --port 5173
```

Vite 把 `/api` 和 `/images` 代理到 `http://localhost:8080`。浏览器打开 `http://127.0.0.1:5173`，走完：

1. `/setup` 创建管理员 `admin`，进入 Dashboard。
2. 添加服务 `Example`，URL `http://127.0.0.1:8080/api/setup/status`。Dashboard 出现该服务。
3. `/services/example` 显示 1 次成功检查、平均 1ms、uptime 100%、Recent incidents 为空。
4. `/incidents` 打开，列表为空，有 New incident。
5. `/integrations` 打开空状态，Add integration 对话框包含 Telegram / Bot token / Chat ID / Notify on warnings。

临时库 `/tmp/alertflow-baseline-config/upmonitor.db` 中：`users` 1 行 admin；`checks` 1 行 `example/online/attempts=1`；`incidents` 与 `integrations` 为空。这与「第一次 online 不制造恢复事件」一致。

审计进程和 `~/.config/upmonitor/state.json`（内容仅指向临时配置目录）已删除。业务代码无改动。

## 当前调用链

```text
main.main
  → api.New(configDir)
       → db.Open（SQLite + goose sqlite3）
       → config.Load（config.yaml）
       → notify.NewDispatcher
       → monitor.New
       → Scheduler.Sync → 每个 service 一个 goroutine
       → retentionLoop
  → Fiber Listen
  → SIGINT/SIGTERM → App.ShutdownWithContext + Server.Close
```

一次检查：

```text
worker.run / Scheduler.CheckNow
  → Scheduler.check
       → runCycle（HTTP Check；已 offline 时不再重试）
       → db.InsertCheck          （独立语句，失败只打日志，不 return）
       → db.UpsertServiceTLS
       → 写入 worker.lastStatus  （进程内存）
       → incident.OnTransition
            previous == current → return
            → offline：db.CreateIncident
                 成功 → go dispatcher.Notify(incident_start)
                 UNIQUE → 打 Debug 后 return
            → online/warning：db.ResolveOngoingIncident
                 有活动故障 → go dispatcher.Notify(incident_resolve)
                 无活动故障且 warning → go dispatcher.Notify(warning)
                      然后 db.CreateWarningEvent
  → Dispatcher.Notify
       → ListEnabledIntegrations
       → 每个渠道一个 goroutine → Sender.Send
       → db.LogNotification（sent/failed）
```

服务定义在 `config.yaml`。`Server.updateConfig` 在锁内克隆、校验、原子保存，再 `Scheduler.Sync`。用户、会话、检查、TLS、Incident、评论、集成和通知日志在 SQLite。

`worker.lastStatus` 只在启动时由 `incident.InitialStatus` 播种：有 ongoing Incident 则为 `offline`，否则为 `online`。它不是数据库里的权威状态。

通知在 `incident.fire` / `fireWarning` 里用 `context.Background()` 另起 goroutine。Dispatcher 没有待发送队列；失败只写入 `notification_log`。测试发送走 `Dispatcher.Test`，不写日志。

## SQLite 特有行为

驱动与连接，[`backend/internal/db/db.go`](../backend/internal/db/db.go)：

- `modernc.org/sqlite`，`sql.Open("sqlite", ...)`
- DSN pragma：`busy_timeout(5000)`、`journal_mode(WAL)`、`foreign_keys(1)`、`synchronous(NORMAL)`
- `SetMaxOpenConns(1)`
- `goose.SetDialect("sqlite3")`

迁移：

- `INTEGER PRIMARY KEY AUTOINCREMENT`：`00001_init.sql`、`00003_incidents.sql`、`00004_integrations.sql`、`00005_check_retries.sql`
- 部分唯一索引：`idx_incidents_one_ongoing ON incidents (service_id) WHERE status = 'ongoing'`
- `00005` 整表重建 `checks` 和 `notification_log`，注释写明 SQLite 不能 `ALTER` CHECK
- 布尔用 `INTEGER`：`integrations.enabled`、`integrations.notify_warnings`
- 集成配置是 `TEXT` 里的 JSON
- 时间是 Unix 秒 `INTEGER`
- `00006` 用 `ALTER TABLE ... ADD COLUMN` 加带 CHECK 的 `severity`

查询与错误，覆盖 `backend/internal/db/*.go` 与 `*_test.go`，不只 `db.go`：

- SQL 占位符全部是 `?`：`users.go`、`sessions.go`、`tls.go`、`checks.go`、`incidents.go`、`integrations.go`，以及 `db_test.go`、`tls_test.go`
- `LastInsertId()`：`users.go`、`integrations.go`、`incidents.go`（创建 Incident、warning、评论）
- `ON CONFLICT (service_id) DO UPDATE` 与 `excluded.*`：`tls.go`
- 唯一冲突靠错误字符串包含 `UNIQUE constraint failed`：`incidents.go` `isUniqueViolation`
- `sqlite_master`：`db_test.go`
- `(ts / ?) * ?` 分桶、`ROW_NUMBER() OVER`：`checks.go`
- `RowsAffected()`：`checks.go` `DeleteOlderThan`
- `db.Begin()` 只出现在归档导入（`incidents.go`、`integrations.go`），检查写入与 Incident 转换不在同一事务
- 未发现 `INSERT OR IGNORE` / `INSERT OR REPLACE`、`julianday`、`strftime`、`datetime()`

Go 把 `enabled` / `notify_warnings` 直接 Scan 进 `bool`。

## 与规划的差异和建议

没有发现需要改规划才能继续的冲突。下面是后续阶段必须处理的事实，本阶段不实施。

1. 检查落库和 Incident/通知不是一个事务。`InsertCheck` 失败后仍会调用 `OnTransition`。
2. 当前状态在 Scheduler 内存。进程重启后，没有 ongoing Incident 的服务会被当成 `online`。
3. 通知是进程内 goroutine，没有租约、重试队列或投递状态机。`notification_log` 只追加 `sent`/`failed`，且对 integration、incident 使用 `ON DELETE CASCADE`。
4. PostgreSQL 迁移不能复用现有 SQLite 迁移文件。`00005` 的表重建、`AUTOINCREMENT`、`?`、`LastInsertId`、pragma、单连接和 `UNIQUE constraint failed` 字符串都要换掉。
5. 模块路径、二进制名和配置目录环境变量仍是 `upmonitor`。改名不是阶段 0 的工作。
6. Compose 和运行时基础镜像使用 `latest`（`ghcr.io/antlko/upmonitor:latest`、`gcr.io/distroless/static:latest`）。阶段 6 再固定版本。
7. 仓库里没有 Agent 包、模型客户端、Agent 表或 Agent 页面。本阶段没有为它加空接口。

## 本阶段明确没有做的事

- 未改业务代码，未加 PostgreSQL、Kafka、Outbox、通知队列或 Agent。
- 未提交，未推送到 `origin` 或 `upstream`。
