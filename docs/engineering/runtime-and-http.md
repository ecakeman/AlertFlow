# 运行时与 HTTP

这份说明对应已经落地的代码，不描述未实现的 Kubernetes、MCP、A2A 或 AgenticRAG。

## Gin

Go 对外 HTTP 在 `backend/internal/api/server.go`。`Handler()` 返回 `gin.New()` 的 Engine，由 `backend/cmd/alertflow/main.go` 里的标准库 `http.Server` 监听。默认地址是 `:8080`，Compose 映射为宿主机 `18080`。

路由保持原来的 11 条：`/healthz`，Service 的创建、查询、更新、时间轴和应用事件，Episode 的查询、时间轴、产物和两次重试。JSON 对象仍用 `DisallowUnknownFields`。应用事件正文仍只读前 1MiB。方法不匹配返回 405。

中间件：

- `X-Request-ID`：请求已带则沿用，否则生成。
- 访问日志：`slog` 记录方法、路径、状态码、耗时和 Request ID，不记录正文。
- Recovery：panic 变成 500 JSON，进程不退出。
- `SetTrustedProxies` 使用空列表，不信任转发头。

产物全文仍通过 `ArtifactSource.GetArtifact(*http.Request, ...)` 调 Python gRPC。未配置 agent 地址时返回 503 `AGENT_UNAVAILABLE`。

## FastAPI

Python HTTP 只在 `agent/src/alertflow_agent/http_app.py`。

| 方法 | 路径 | 含义 |
|---|---|---|
| GET | `/health/live` | 进程还在就返回 200，不查数据库或模型 |
| GET | `/health/ready` | runtime 已初始化、HTTP lifespan 已开始、gRPC 已启动、worker 已启动、未关闭，并且连接池能执行 `SELECT 1`。否则 503 |

它不实现 `SubmitTask`，也不提供 Service 或 Episode 接口。

## gRPC 边界

Go 的 `SubmitTask` 和 `GetArtifact` 发到 Java `:9091`，再转到 Python。Python 的系统 RPC 发到同一个 Java 端口，再转到 Go `:9090`。字段没有改。Python 侧仍是 `grpc.aio`，内部 deadline 是 30 秒。模型 HTTP 使用 runtime 持有的 `httpx.AsyncClient`，超时 60 秒。这些 gRPC 通道没有 TLS，只作为本地开发配置。

`InvokeAgent` 也在 Java `:9091`。Java 再用 A2A `message/send` 调用 Knowledge Agent。MCP 入口是 Java `http://integration:8081/mcp`。

## 启动和关闭

`python -m alertflow_agent` 进入 `runtime.serve()`，在同一个事件循环里启动：

1. 业务 `AsyncConnectionPool`，新连接注册 pgvector。
2. Checkpoint 用的另一个池：`autocommit`、`dict_row`、`search_path=cognitive, public`，然后 `AsyncPostgresSaver.setup()`。
3. 固定数量的 worker 协程和两秒一次的恢复扫描。
4. `grpc.aio` server。
5. Uvicorn。Uvicorn 自己的信号处理被关掉，只由 runtime 接收 SIGINT 和 SIGTERM。

关闭时先拒绝新的 `SubmitTask`，取消恢复扫描，不再给 worker 分配新任务，等待正在执行的任务。超时后取消剩余协程。取消不会把任务报成成功，也不会推进 `processed_input_version`。advisory lock 和它专用的连接在 `finally` 里释放。未完成的 receipt 留在 `ACCEPTED` 或 `RUNNING`，下次扫描可以再入队。

## Worker Pool

默认最多 4 个任务同时执行，队列容量 128。环境变量是 `AGENT_MAX_CONCURRENT_TASKS` 和 `AGENT_TASK_QUEUE_CAPACITY`。

`SubmitTask` 先把 receipt 写入 PostgreSQL。新任务再 `put_nowait`。队列满时不丢 receipt，保持 `ACCEPTED`，交给恢复扫描。同一个 `task_id` 已经在本进程队列或执行中时不再入队。

每个任务使用单独的 `AsyncConnection` 获取 `pg_advisory_lock`，锁键来自 `diagnosis:{episode_id}` 或 `archive:{episode_id}`。这条连接在持锁期间不归还连接池。同一 Episode 因此串行，不同 Episode 可以并行。`asyncio.Lock` 只防止本进程重复入队。

LangGraph 诊断图通过 `ainvoke` 执行。节点、路由、`query_timeline`、`search_faults`、四轮上限和 ScriptedModel 没有改。没有 `ALERTFLOW_LLM_API_KEY` 时仍走 ScriptedModel。

## 连接

| 用途 | 连接 |
|---|---|
| receipt、GetArtifact、恢复扫描 | 业务连接池 |
| 任务执行、advisory lock、产物和 FD | 该任务专用的 `AsyncConnection` |
| LangGraph Checkpoint | 独立的 autocommit 连接池和 `AsyncPostgresSaver` |

Checkpoint 不是调查文档或 FD 的权威存储。

## 端口和环境变量

| 变量 | 默认 | 作用 |
|---|---|---|
| `ALERTFLOW_HTTP_ADDR` | `:8080` | Go HTTP。Compose 发布为 `18080` |
| `ALERTFLOW_GRPC_ADDR` | `:9090` | Go gRPC，仅 Compose 网络内 |
| `AGENT_GRPC_ADDR` | `0.0.0.0:9000` | Python gRPC，不映射到宿主机 |
| `AGENT_HTTP_ADDR` | `0.0.0.0:8000` | Python FastAPI，不映射到宿主机 |
| `ALERTFLOW_GRPC_TARGET` | `127.0.0.1:9090` | Python 访问 Java gRPC。Java 再转发到 Go |
| `ALERTFLOW_AGENT_GRPC_TARGET` | 空则产物接口 503 | Go 访问 Java gRPC，Java 再访问 Python |
| `INTEGRATION_GRPC_ADDR` | `:9091` | Java gRPC |
| `INTEGRATION_HTTP_ADDR` | `:8081` | Java HTTP，不映射到宿主机 |
| `ALERTFLOW_KNOWLEDGE_ENABLED` | `false` | 为 `true` 时诊断图调用 Knowledge Agent。Compose 里还要加 `--profile knowledge` |
| `KNOWLEDGE_A2A_TOKEN` | 空 | Java 和 Knowledge Agent 的 Bearer token。空值会拒绝 A2A 调用 |
| `JAVA_TOOL_OPTIONS` | `-Xms128m -Xmx512m` | Integration 的 JVM 参数。Kafka 堆是 512m，Nacos 堆是 512m，三者容器上限都是 768m。Knowledge Agent 也是 768m，镜像固定到 rag `dc929b6a7b11edd7b567b06529c1da49560f0a68` |
| `KNOWLEDGE_AGENT_MODE` | `false` | Knowledge Agent 容器内设为 `true`。Legacy RAG 不设置 |
| `ALERTFLOW_MCP_ENDPOINT` | 空 | Java MCP Gateway |
| `ALERTFLOW_DATABASE_URL` | 本地 5432 | 两侧共用一个实例、两个 schema |
| `AGENT_SHUTDOWN_TIMEOUT` | `20` | 等待正在执行的任务的秒数 |

镜像健康检查访问容器内 `http://127.0.0.1:8000/health/live`，使用 Python 标准库，不另外安装 curl。

## 本地和 Compose

单元测试数据库是 `postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable`。

```bash
go test ./...
cd agent && uv sync && .venv/bin/python -m pytest -q tests
docker compose -f deploy/docker-compose.yml config
deploy/e2e.sh
```

`deploy/e2e.sh` 在闭环之外，会在 agent 容器内请求 `/health/live` 和 `/health/ready`，并确认 `search_faults` 能召回本次服务的 FD。
