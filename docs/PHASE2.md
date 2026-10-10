# 阶段 2：检查记录与 Transactional Outbox

日期：2026-10-10

## 完成的内容

一次检查周期结束后，`checks` 行和对应的 `outbox_events` 行在同一个数据库事务里写入。事件类型是 `monitor.check.completed`，`schema_version` 为 1。`event_id` 在创建时生成一次，同时放在列和 payload 里。payload 的 `check_id` 是该事务里 `INSERT ... RETURNING id` 得到的检查 id，并带上当时的服务名和 URL。

事务失败时两行都不留下。Scheduler 不更新 `lastStatus`，不写 TLS，也不调用 `OnTransition`，因此不会为这次未保存的检查发通知。HTTP 检查失败仍记成 `offline`（或重试后的 `warning`）；那是监控结果。数据库错误是另一条路径。

提交成功之后，才沿用原来的 `OnTransition`。Incident 创建、恢复和 warning 记录仍在这个事务外面，通知仍是提交后的 goroutine。`published_at` 在正常路径保持 NULL。`MarkOutboxPublished` 只给测试和下一阶段调用，Scheduler 不调用它。

领取用短事务，`FOR UPDATE SKIP LOCKED` 只锁 Outbox 行，并写下 `lease_owner` 和 `lease_until`。失败记录增加 `attempt_count`、`last_error` 和 `next_attempt_at`，清掉租约，不删除行，也不标记已发布。没有发布循环，没有 Kafka。

## 没有做的事

- 没有把 Incident SQL 放进检查事务。
- 没有 `processed_events`、`service_runtime_state` 或通知投递表。
- 没有跨进程锁，也没有服务表上的 `FOR UPDATE`。`runMu` 只串行同一进程里同一个 worker 的定时检查和手动检查。
- 提交之后、通知发出之前进程崩溃，这次通知仍可能丢失。Outbox 行还在，但没有发布器去补发。

## 测试

```text
cd backend && ALERTFLOW_TEST_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5432/postgres?sslmode=disable' go test ./...
```

退出码 0。`db` 覆盖同事务提交、重复 `event_id` 和非法状态的回滚、`event_id`/`check_id` 读出后再序列化不变、租约挡住第二次领取、过期后可再领、失败记录不删除也不标记发布。`monitor` 覆盖持久化失败时 `lastStatus` 不变、没有 Incident、没有通知请求，以及同一 worker 上两次 `check` 的 HTTP 不重叠。`incident` 原有状态测试通过。测试库名是 `alertflow_test_*`。

```text
cd backend && go vet ./... && go build ./...
```

退出码 0。

没有改前端和 Compose。没有再次执行 `docker build`；阶段 1 已记录它停在解析 `docker.io/docker/dockerfile:1` 的网络超时，镜像仍未构建。
