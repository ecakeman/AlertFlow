# 集成侧证据

日期：2026-11-05。AlertFlow 分支 `advence`（基线 `e175b63`）。Knowledge Agent 在 `ecakeman/rag` 的 `long`（基线 `450e454`），Compose 只引用镜像 `knowledge-agent:dev`。没有做文档 4.2 的旧库迁移。

## 锁定的版本

- JDK 镜像 `eclipse-temurin:25-jdk`，Spring Boot 4.1.2，Spring AI 2.0.2，grpc-java 1.73.0，protobuf 4.36.2。
- A2A Java SDK `org.a2aproject.sdk` 1.5.0.Final。Java 客户端实际发送的方法名是 `SendMessage`，传输使用 HTTP/1.1。Knowledge Agent 同时接受 `message/send` 和 `SendMessage`。
- `nacos-client` 3.3.0。Compose 服务端是 `nacos/nacos-server:v2.5.1`（standalone，未开鉴权）。环境变量里的 Agent Card 地址不过期；Nacos 快照超过 `INTEGRATION_REGISTRY_STALE_SECONDS`（默认 300）后，新调用回退到这份地址，没有地址则失败关闭。
- MCP 自动配置类是 `org.springframework.ai.mcp.server.common.autoconfigure.McpServerAutoConfiguration`，已关闭。Gateway 自己实现 Streamable HTTP。Flyway 使用 `spring-boot-starter-flyway`，JDBC URL 带 `currentSchema=gateway`。
- 没有把 `a2a-sdk` 1.3.0 或 MCP Python SDK v2 放进运行依赖。互操作是手写适配对上 A2A Java SDK。

## Java 测试

`mvn -B -Dtest=BridgeAndInvocationTest test`（`eclipse-temurin:25-jdk` 容器内）：6 tests, 0 failures。覆盖 Python 接受后才回 ACK、Python 失败不回 ACK、`InvokeAgent` 幂等、两个 MCP 身份互不可见、A2A data part、依赖类可加载。

## 运行中的栈

`docker compose -f deploy/docker-compose.yml config` 通过。

`ALERTFLOW_SKIP_IMAGE_BUILD=1 bash deploy/e2e.sh` 退出码 0：

- episode `0eaa9d8f-d59b-4698-a3e8-c2620a0f6487` 到达 `ARCHIVED`
- `search_faults` 取回 `fd_id` `42f89b47-9ef5-5112-bc10-e4e7cd35a775`
- `system.episode_fault_document_refs` 该 episode 1 行
- 这次演练窗口内 `gateway.agent_invocations` 状态 `COMPLETED` 10 行
- Knowledge Agent 摄取 `sample.txt` 后，`message/send` 的结果正文含 `pool exhausted`

同一 `invocationId=idem-proof-1` 连续两次 `message/send`，`knowledge.knowledge_invocation` 只有 1 行，两次都返回 `knowledge-retrieval-result-v1`，其中 `results` 长度为 1。

空 MCP 目录：`main-agent-token` 与 `knowledge-agent-token` 的 `tools/list` 都是 `{"tools":[]}`，错误 bearer 被拒绝。两个进程保持健康。

`go test -p 1 ./...` 退出码 0。agent `pytest -q tests` 为 14 passed。

Go 源码里 `cognitive.fault_documents` 只出现在 `apply_test.go` 的清表列表。仓库没有 RAGFlow 配置。

`deploy/e2e.sh` 里的 `UPDATE system.service_monitor_targets SET enabled=false` 会关闭这张表里的全部监控目标，不只是本次演练创建的数据。这是破坏性操作。探测 worker 只有 4 个，历史目标把通道占满后，新服务的恢复探测不会被执行，episode 会停在 `ACTIVE`。
