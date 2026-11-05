# 集成侧

AlertFlow 的整体在 `advence` 分支维护。Java 进程只做转发、发现、A2A 调用和 MCP 授权。

- gRPC `:9091`：`AgentExecutionService` 转到 Python，`SystemGatewayService` 转到 Go，`AgentInvocationService` 在本进程完成。
- HTTP `:8081`：`/mcp` 和 actuator 健康检查。
- `gateway` schema 由 Flyway 创建，账号是 `gateway_app`。
- `SubmitTask` 只有在 Python 返回后才回给 Go。Python 失败时 Java 把 gRPC 错误原样返回。
- 归档完成只看 `system.episode_fault_document_refs`。Go 不再查询 `cognitive.fault_documents`。
- Knowledge Agent 镜像固定为 `knowledge-agent:72a715f17fe723b2457b24d8981ce2b083671be7`。构建 context 是 `https://github.com/ecakeman/rag.git#72a715f17fe723b2457b24d8981ce2b083671be7:ai`。容器内存上限是 768m。上传上限和摄取重试次数由 `KNOWLEDGE_UPLOAD_MAX_BYTES`、`KNOWLEDGE_INGEST_MAX_ATTEMPTS` 传入，默认 33554432 和 5。
- Nacos 客户端是 `nacos-client` 3.3.0。Compose 里的服务端镜像是 `nacos/nacos-server:v2.5.1`，因为本环境用它的 naming 接口做 Agent Card 地址发现。没有新快照时调用失败关闭。
- 当前 gRPC 没有 TLS。这只是本地开发配置，不是生产环境的服务认证。
- Java 调用 Knowledge Agent 时必须带 `KNOWLEDGE_A2A_TOKEN`。token 来自环境变量，不写进代码或镜像。
- `gateway.agent_invocations` 用 Flyway `V2__invocation_lease.sql` 增加租约和 `fencing_token`。同一个调用只有一个执行者。
- Kafka、Nacos、Integration 的 JVM 堆和容器内存上限写在 Compose 里。Knowledge Agent 属于 profile `knowledge`，Integration 不依赖它启动。这些限制没有在本轮实测。
