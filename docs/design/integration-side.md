# 集成侧

AlertFlow 的整体在 `advence` 分支维护。Java 进程只做转发、发现、A2A 调用和 MCP 授权。

- gRPC `:9091`：`AgentExecutionService` 转到 Python，`SystemGatewayService` 转到 Go，`AgentInvocationService` 在本进程完成。
- HTTP `:8081`：`/mcp` 和 actuator 健康检查。
- `gateway` schema 由 Flyway 创建，账号是 `gateway_app`。
- `SubmitTask` 只有在 Python 返回后才回给 Go。Python 失败时 Java 把 gRPC 错误原样返回。
- 归档完成只看 `system.episode_fault_document_refs`。Go 不再查询 `cognitive.fault_documents`。
- Knowledge Agent 镜像来自 `ecakeman/rag` 的 `long` 分支，Compose 使用 `knowledge-agent:dev`。
- Nacos 客户端是 `nacos-client` 3.3.0。Compose 里的服务端镜像是 `nacos/nacos-server:v2.5.1`，因为本环境用它的 naming 接口做 Agent Card 地址发现。没有新快照时调用失败关闭。
