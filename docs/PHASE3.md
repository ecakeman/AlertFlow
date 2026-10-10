# 阶段 3：Kafka、消费幂等与 Incident

日期：2026-10-11

## 完成的内容

同一份二进制按 `ALERTFLOW_ROLE` 分成三个进程。`server` 仍是 API 和 Scheduler，不发布、不消费。`publisher` 和 `consumer` 各跑一个副本，不带 Scheduler。

Publisher 按 Outbox `id` 逐条领取，消息 key 是 `service_id`，主题是 `alert-events`（3 个分区）。Kafka 调用在数据库事务外。broker ack 之后，并且租约仍属于这次领取，才写 `published_at`。发布失败只增加 `attempt_count`、`last_error`、`next_attempt_at`，不删除行，也不把事件标成已发布。过期或被别人领走的租约更新不到行。数据库提交和 Kafka ack 不是同一次原子提交。

Consumer 先校验类型、`schema_version = 1`，以及 header 里的 `event_id` 和 payload。无法解析或不支持的版本写入 `dead_letter_events` 后再提交 offset。其余消息在一个 PostgreSQL 事务里插入 `processed_events`、按 `event.Decide` 更新 Incident、为启用的集成插入 `pending` 投递任务，并更新 `service_runtime_state`。重复的 `event_id` 不再创建 Incident 或投递任务。前序状态来自锁住的 `service_runtime_state`，不是服务当前最新检查。更早的 `check_id` 不会把已经前进的状态倒回去。数据库失败则不提交 offset。本阶段不调用 `Dispatcher.Notify`。

人工创建、编辑、恢复 Incident 的 API 没有改。`idx_incidents_one_ongoing` 仍限制每个服务最多一条进行中故障。

## 没有做的事

- 没有发送通知，也没有投递重试。
- 没有多个 Publisher 副本。分区数多于 Consumer 副本时，多出来的 Consumer 不会提高并行度。
- 没有把 `published_at` 改成 `processed_at`，也没有改已经应用的 `00001` / `00002`。

随后的修补用 `00004_delivery_integration_cascade.sql` 把 `notification_deliveries.integration_id` 改成 `ON DELETE CASCADE`。删除集成或用归档替换集成时，关联的投递任务被删掉，这不是送达记录。`00003` 文件本身没有改。

## 测试

```text
cd backend && ALERTFLOW_TEST_DATABASE_URL='postgres://alertflow:alertflow@127.0.0.1:5432/postgres?sslmode=disable' ALERTFLOW_KAFKA_BROKERS='127.0.0.1:9092' go test ./... -count=1 -timeout 180s
```

退出码 0（2026-10-11）。`db` 覆盖状态转换、重复消费、非法状态的事务回滚、过期租约不能改写、更早的检查不倒退状态。`consume` 覆盖无法解析的消息进入 dead letter、数据库失败时不提交，以及一条真实 Kafka 路径：连不上 broker 时检查和 Outbox 仍在，恢复后 Publisher 写出，Consumer 落下进行中 Incident 和 `pending` 任务。缺少 `ALERTFLOW_KAFKA_BROKERS` 时该测试失败，不跳过。

```text
cd backend && go vet ./... && go build ./...
```

退出码 0。仓库根目录 `git diff --check` 退出码 0。

没有改前端。`docker pull apache/kafka:3.9.1` 停在 `registry-1.docker.io` 的 `dial tcp ... i/o timeout`，官方镜像没有拉下来。测试用的是本机已经有的 `docker.m.daocloud.io/apache/kafka:3.9.1`。没有再次执行应用镜像的 `docker build`。
