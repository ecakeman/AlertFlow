# 运行时与 HTTP

这份说明对应已经落地的代码，覆盖 Go HTTP 路由、Python HTTP、gRPC 边界、Kubernetes 工作负载观察以及两侧的启动与关闭。MCP、A2A 与 AgenticRAG 的业务细节不在本文展开。

## Gin

Go 对外 HTTP 在 `backend/internal/api/server.go`。`Handler()` 返回 `gin.New()` 的 Engine，由 `backend/cmd/alertflow/main.go` 里的标准库 `http.Server` 监听。默认地址是 `:8080`，Compose 映射为宿主机 `18080`。

路由如下：

| 方法 | 路径 | 含义 |
|---|---|---|
| GET | `/healthz` | 进程存活，返回 `{"status":"ok"}` |
| GET | `/readyz` | 就绪检查。2 秒超时内对数据库执行 `SELECT 1`，通过返回 200 `{"status":"ok"}`；失败返回 503 `{"status":"unavailable","database":"down"}`。只检查数据库，不检查 Kafka 或模型 |
| GET | `/api/v1/dashboard/summary` | 总览聚合：服务/目标/Episode 计数、最近 Episode、最近 30 分钟监控趋势（5 分钟分桶，最多 6 桶） |
| GET | `/api/v1/services` | 服务列表，支持 `limit`（默认 50，最大 200）、`offset`、`status`（NORMAL、ABNORMAL、UNKNOWN） |
| POST | `/api/v1/services` | 创建 Service 及其监控目标，返回 201 |
| GET | `/api/v1/services/:service_id` | Service 详情，含 targets |
| PATCH | `/api/v1/services/:service_id` | 修改服务元数据（name、description、enabled） |
| DELETE | `/api/v1/services/:service_id` | 软删除服务：写入 `deleted_at`、服务与目标 enabled 置为 false；历史 Episode、点位和产物保留 |
| GET | `/api/v1/services/:service_id/targets` | 该服务下未删除的监控目标 |
| POST | `/api/v1/services/:service_id/targets` | 新增监控目标，返回 201 |
| GET | `/api/v1/services/:service_id/timeline` | 服务时间轴（最近 100 条） |
| POST | `/api/v1/services/:service_id/events` | 应用侧事件上报 |
| GET | `/api/v1/episodes` | Episode 列表，支持 `status`、`service_id`、`from`、`to`、`limit`（默认 20，最大 100）、`offset` |
| GET | `/api/v1/episodes/:episode_id` | Episode 详情 |
| GET | `/api/v1/episodes/:episode_id/timeline` | Episode 三区域时间轴 |
| GET | `/api/v1/episodes/:episode_id/artifacts` | 产物全文，`type=INVESTIGATION` 或 `DIAGNOSIS_REPORT` |
| POST | `/api/v1/episodes/:episode_id/retry-diagnosis` | 重试诊断 |
| POST | `/api/v1/episodes/:episode_id/retry-archive` | 重试归档 |
| PATCH | `/api/v1/targets/:target_id` | 修改监控目标；字段全部可选，合并后按目标类型重新校验 |
| DELETE | `/api/v1/targets/:target_id` | 软删除监控目标，允许之后用同一 `target_key` 重建 |

JSON 对象仍用 `DisallowUnknownFields`。应用事件正文仍只读前 1MiB。方法不匹配返回 405。错误体仍是 `{"error_code","message"}`，删除类接口成功返回 204。

Episode 视图在本次交付中补充了 `service_name` 与 `updated_at`（`active_task_status` 等原有字段继续保留）；服务列表返回聚合后的 `monitor_status`；监控目标补充 `last_check_at`。

中间件：

- `X-Request-ID`：请求已带则沿用，否则生成。
- 访问日志：`slog` 记录方法、路径、状态码、耗时和 Request ID，不记录正文。
- Recovery：panic 变成 500 JSON，进程不退出。
- `SetTrustedProxies` 使用空列表，不信任转发头。

产物全文仍通过 `ArtifactSource.GetArtifact(*http.Request, ...)` 调 Python gRPC。未配置 agent 地址时返回 503 `AGENT_UNAVAILABLE`。

## Kubernetes 工作负载观察

范围只有 `Deployment` 和 `StatefulSet`，namespace 由 Go Backend 统一配置，不在 target 里保存。观察使用 client-go 单次 `Get`，不使用 Informer / Watch，也没有 `List` 权限。

配置：

| 变量 | 默认 | 含义 |
|---|---|---|
| `ALERTFLOW_K8S_ENABLED` | `false` | 是否启用 Kubernetes 工作负载监控。Compose 默认关闭 |
| `ALERTFLOW_K8S_NAMESPACE` | `default` | 被观察工作负载所在 namespace |

启用时 Go 使用 InClusterConfig 创建 client，不读取本地 kubeconfig。Kubernetes 清单里 Go 使用专用 ServiceAccount `alertflow-go`，Role 只授予 `apps` 组 `deployments`、`statefulsets` 的 `get`。没有集群却把开关打开时，启动日志会给出明确错误，K8s 目标的采样会按观察器自身错误处理，不会伪造工作负载故障。

监控目标：

- `target_type=KUBERNETES` 时，`address` 必须为空字符串，`config` 必须是 `{"resource_kind":"Deployment"|"StatefulSet","resource_name":"非空"}`。
- `interval_ms`、`timeout_ms` 为 0 时补默认值 10000、3000 毫秒；失败/恢复阈值沿用原有的 `>0` 校验。

worker 按 `TargetType` 显式分发到 HTTP、TCP、KUBERNETES 三种探测；遇到未知类型只记结构化错误日志并跳过，不会退化成 HTTP 探测。

状态判定：

- `Deployment`：期望副本取 `spec.replicas`（为空视为 1）。期望为 0 视为健康；`availableReplicas >= desired` 健康；`Progressing=False` 且 reason 为 `ProgressDeadlineExceeded` 视为异常；其余副本不足视为异常，错误分类 `REPLICAS_UNAVAILABLE`。
- `StatefulSet`：期望副本取 `spec.replicas`（为空视为 1）。期望为 0 视为健康；`readyReplicas >= desired` 健康；否则异常，错误分类 `REPLICAS_UNAVAILABLE`。不比较 `updatedReplicas`。
- 资源不存在（API 返回 404）属于业务失败采样：错误分类 `RESOURCE_NOT_FOUND`，按失败阈值计数，可以形成 Episode。

观察器自身错误（RBAC 拒绝、网络错误、超时、429/5xx、client 未初始化）不算业务失败：不调用 `ApplyMonitorSample`、不增加连续失败计数、不创建 Episode，`last_check_at` 也不更新，只写结构化日志。这样 Kubernetes API 抖动不会被记成工作负载故障。

`raw_result` 只保存精简证据：kind、name、desired、ready、available、updated、conditions、observed_at。`diagnostic_fields` 写入 `status_class`、`error_class` 和 `state_fingerprint`。

`StateFingerprint` 是 `MonitorSample` 上的字符串字段，会写入 `diagnostic_fields`、参与 `MonitorField` 和输入哈希。指纹包含目标类型、归一化后的失败原因、期望副本、Ready/Available 等状态，不包含 `resourceVersion`、UID、时间戳和 labels。因此仅有 `resourceVersion` 变化不会产生新的输入版本；从 1/3 变为 2/3 这类状态变化会产生新版本。

局限：单 namespace、单集群，只读 `Deployment` / `StatefulSet`，不观察 Pod、Node 或容器指标。

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

Go 侧的关闭是有界的：`main.go` 收到 SIGINT/SIGTERM 后，HTTP `Shutdown` 带 deadline（10 秒），gRPC 先 `GracefulStop`，超时后调用 `Stop()`；这些等待都结束后进程退出。Kubernetes 清单中 Go / Java / Python 的 `terminationGracePeriodSeconds` 设为 45 秒，与这个退出时间一致；Frontend 为 15 秒。

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
| `ALERTFLOW_K8S_ENABLED` | `false` | 是否启用 Kubernetes Deployment / StatefulSet 监控。Compose 默认关闭 |
| `ALERTFLOW_K8S_NAMESPACE` | `default` | 被观察工作负载所在 namespace，由 Go 统一配置 |
| `AGENT_GRPC_ADDR` | `0.0.0.0:9000` | Python gRPC，不映射到宿主机 |
| `AGENT_HTTP_ADDR` | `0.0.0.0:8000` | Python FastAPI，不映射到宿主机 |
| `ALERTFLOW_GRPC_TARGET` | `127.0.0.1:9090` | Python 访问 Java gRPC。Java 再转发到 Go |
| `ALERTFLOW_AGENT_GRPC_TARGET` | 空则产物接口 503 | Go 访问 Java gRPC，Java 再访问 Python |
| `INTEGRATION_GRPC_ADDR` | `:9091` | Java gRPC |
| `INTEGRATION_HTTP_ADDR` | `:8081` | Java HTTP，不映射到宿主机 |
| `ALERTFLOW_KNOWLEDGE_ENABLED` | `false` | 为 `true` 时诊断图调用 Knowledge Agent。Compose 里还要加 `--profile knowledge` |
| `KNOWLEDGE_A2A_TOKEN` | 空 | Java 和 Knowledge Agent 的 Bearer token。空值会拒绝 A2A 调用 |
| `JAVA_TOOL_OPTIONS` | `-Xms128m -Xmx512m` | Integration 的 JVM 参数。Kafka 堆是 512m，Nacos 堆是 512m，三者容器上限都是 768m。Knowledge Agent 也是 768m，镜像固定到 rag `a41981c6265018e5a203e9340b3800686a31a37b` |
| `KNOWLEDGE_UPLOAD_MAX_BYTES` | `33554432` | 传给 Knowledge Agent 的单文件上限 |
| `KNOWLEDGE_INGEST_MAX_ATTEMPTS` | `5` | 传给 Knowledge Agent 的摄取尝试上限 |
| `KNOWLEDGE_AGENT_MODE` | `false` | Knowledge Agent 容器内设为 `true`。Legacy RAG 不设置 |
| `ALERTFLOW_MCP_ENDPOINT` | 空 | Java MCP Gateway |
| `ALERTFLOW_DATABASE_URL` | 本地 5432 | 两侧共用一个实例、两个 schema |
| `AGENT_SHUTDOWN_TIMEOUT` | `20` | 等待正在执行的任务的秒数 |

镜像健康检查：Go 镜像访问容器内 `http://127.0.0.1:8080/healthz`，Python 镜像访问 `http://127.0.0.1:8000/health/live`（使用 Python 标准库，不另外安装 curl）；Java 镜像访问 `/actuator/health/liveness`。

## 本地和 Compose

单元测试数据库是 `postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable`。

```bash
go test ./...
cd agent && uv sync && .venv/bin/python -m pytest -q tests
docker compose -f deploy/docker-compose.yml config
deploy/e2e.sh
```

`deploy/e2e.sh` 在闭环之外，会在 agent 容器内请求 `/health/live` 和 `/health/ready`，并确认 `search_faults` 能召回本次服务的 FD。脚本开始前先记录已经启用的 `service_monitor_targets.target_id`，只暂停这些行，退出时按这些 id 恢复。本次新建的目标不在这份名单里。SIGKILL 不会执行恢复。

Knowledge Agent 的上传上限和摄取尝试次数由 Compose 传入，默认 32MiB 和 5 次。`0` 或负数会在 Knowledge Agent 启动时被拒绝。索引刷新失败不会把已经 READY 的摄取任务改回 PENDING。这些命令是否在本次交付执行、真实结果如何，以交付报告为准；不要把未执行过的检查写成已经通过。Kubernetes 清单未在真实集群上运行验证。
