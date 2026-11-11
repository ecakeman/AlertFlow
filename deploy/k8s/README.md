# AlertFlow Kubernetes 应用部署说明

本目录只包含 AlertFlow **应用组件** 的 Kubernetes 清单：

| 组件 | 镜像 | HTTP | gRPC |
|---|---|---:|---:|
| Go Backend | `ghcr.io/ecakeman/alertflow-backend:v0.1.0` | 8080 | 9090 |
| Java Integration | `ghcr.io/ecakeman/alertflow-integration:v0.1.0` | 8081 | 9091 |
| Python Main Agent | `ghcr.io/ecakeman/alertflow-agent:v0.1.0` | 8000 | 9000 |
| Frontend（Nginx） | `ghcr.io/ecakeman/alertflow-frontend:v0.1.0` | 80 | — |

PostgreSQL、Kafka、Redis、Nacos 使用**外部已有环境**，本目录不部署。Knowledge Agent 为可选组件，本期不提供其清单（见文末说明）。

## 目录与清单

| 文件 | 内容 |
|---|---|
| `00-namespace.yaml` | `Namespace alertflow` + 专用 ServiceAccount `alertflow-go` |
| `10-rbac.yaml` | `Role alertflow-k8s-reader` + `RoleBinding`（仅 apps/deployments、statefulsets 的 get） |
| `20-configmap.yaml` | 三个服务的非敏感配置：`alertflow-go-config`、`alertflow-integration-config`、`alertflow-agent-config` |
| `30-services.yaml` | 四个 ClusterIP Service：`alertflow`、`integration`、`agent`、`frontend` |
| `40-deployments.yaml` | 四个 Deployment：单副本、`strategy: Recreate`、探针、资源限制 |
| `secret.example.yaml` | Secret `alertflow-secrets` 的字段示例，**占位值，不能用于真实环境** |

ConfigMap 的键名就是环境变量名；Deployment 通过 `envFrom` 注入 ConfigMap，敏感值通过 `valueFrom.secretKeyRef` 从 `alertflow-secrets` 注入。

## 前置条件

1. **Kubernetes 集群与 kubectl**，当前 context 有权限创建 Namespace 级资源。
2. **外部 PostgreSQL**（建议 16 + pgvector）：
   - 预建数据库 `alertflow`；
   - Go 启动时用 goose 自动迁移，会执行 `CREATE EXTENSION vector / pgcrypto` 并创建 `system`、`cognitive` schema，因此连接账号需要有建扩展/建 schema 的权限（或用 DBA 预先安装扩展并授予权限）；
   - Java Flyway 在首次启动时创建 `gateway` schema（`create-schemas: true`），`gateway_app` 账号需要对库有 CREATE 权限，可参考 `deploy/postgres/init-roles.sh`；
   - Knowledge 库（`knowledge`）仅在使用 Knowledge Agent 时需要，本期可选。
3. **外部 Kafka**：broker 地址可达；topic `alertflow.agent-task-ready.v1` 允许自动创建或提前创建。
4. **外部 Redis**：地址可达（Go 用作旁路缓存，未配置时功能降级）。
5. **外部 Nacos**：地址可达；JIT 注册中心用于 Knowledge Agent 发现，未启用时可忽略连接错误。
6. **四个镜像已构建并推送到集群可拉取的仓库**（见下一节）。集群节点能够访问该 registry。
7. 集群网络能够访问上述外部中间件与大模型 API。

## 1. 构建并推送镜像

以下命令均假设仓库根目录为当前目录，仓库地址 `ghcr.io/ecakeman/*` 可替换为你的 registry。正式部署必须使用明确 tag，不要用 `latest`。

```bash
# Go Backend（构建上下文 = 仓库根目录）
docker build -f deploy/Dockerfile.backend \
  -t ghcr.io/ecakeman/alertflow-backend:v0.1.0 .
docker push ghcr.io/ecakeman/alertflow-backend:v0.1.0

# Java Integration（构建上下文 = 仓库根目录）
docker build -f integration/Dockerfile \
  -t ghcr.io/ecakeman/alertflow-integration:v0.1.0 .
docker push ghcr.io/ecakeman/alertflow-integration:v0.1.0

# Python Main Agent（构建上下文 = agent/，Dockerfile 在 deploy/）
(cd agent && docker build -f ../deploy/Dockerfile.agent \
  -t ghcr.io/ecakeman/alertflow-agent:v0.1.0 .)
docker push ghcr.io/ecakeman/alertflow-agent:v0.1.0

# Frontend（Nginx 运行阶段；Dockerfile 路径以最终前端交付为准，下面按 frontend/Dockerfile 示例）
docker build -f frontend/Dockerfile \
  -t ghcr.io/ecakeman/alertflow-frontend:v0.1.0 frontend
docker push ghcr.io/ecakeman/alertflow-frontend:v0.1.0
```

镜像要求：

- Go / Java / Python 必须是非 root 进程，且 Dockerfile 用**数字 UID** 设置 `USER`（如 `USER 10001`）。清单里设置了 `runAsNonRoot: true`，非数字用户名会导致 Pod 校验失败。
- Frontend 使用标准 Nginx（root master 绑定 80 端口），清单对 Frontend 不设置 `runAsNonRoot`；其 `nginx.conf` 必须提供 `location = /healthz { return 200; }`（探针依赖），并把 `/api/` 代理到 `http://alertflow:8080`。
- Go 镜像需要 `/readyz` 路由（本仓库后端实现），Java 镜像的 Actuator 需开启健康探针（本仓库 `application.yaml` 已开启）。

如果镜像仓库地址或 tag 不同，修改 `40-deployments.yaml` 中四个 `image` 字段即可。

## 2. 修改外部地址占位

`20-configmap.yaml` 与 `secret.example.yaml` 中使用 `*.example.internal` 的地址全部是占位，部署前必须替换为**集群网络内可达**的外部地址，例如：

```text
postgres.example.internal:5432   → 你的 PostgreSQL 地址
kafka.example.internal:9092      → 你的 Kafka broker
redis.example.internal:6379      → 你的 Redis
nacos.example.internal:8848      → 你的 Nacos
```

注意：不能照搬 Docker Compose 里的 `postgres` / `kafka` / `redis` / `nacos` 服务名，除非外部环境确实在集群 DNS 中提供同名服务。

## 3. 创建 Namespace 与 Secret（按顺序）

```bash
# 3.1 命名空间 + Go 专用 ServiceAccount
kubectl apply -f deploy/k8s/00-namespace.yaml

# 3.2 Secret（二选一）
# 方式 A：用 kubectl 命令创建（推荐，避免在仓库中留下明文副本）
kubectl -n alertflow create secret generic alertflow-secrets \
  --from-literal=ALERTFLOW_DATABASE_URL='postgres://alertflow:CHANGE_ME@postgres.example.internal:5432/alertflow?sslmode=disable' \
  --from-literal=ALERTFLOW_REDIS_URL='redis://:CHANGE_ME@redis.example.internal:6379/0' \
  --from-literal=ALERTFLOW_KAFKA_BROKERS='kafka.example.internal:9092' \
  --from-literal=INTEGRATION_DATABASE_PASSWORD='CHANGE_ME' \
  --from-literal=KNOWLEDGE_A2A_TOKEN='' \
  --from-literal=ALERTFLOW_MAIN_MCP_TOKEN='CHANGE_ME' \
  --from-literal=ALERTFLOW_MCP_TOKEN='CHANGE_ME' \
  --from-literal=ALERTFLOW_LLM_API_KEY='' \
  --from-literal=NACOS_USERNAME='' \
  --from-literal=NACOS_PASSWORD=''

# 方式 B：复制字段示例到你自己的文件（放在仓库外，避免被误提交）后 apply
cp deploy/k8s/secret.example.yaml /tmp/alertflow-secret.yaml
# 编辑 /tmp/alertflow-secret.yaml，替换全部占位值
kubectl apply -f /tmp/alertflow-secret.yaml
```

说明：

- `secret.example.yaml` 只是字段示例，**不要直接 apply，也不要提交真实值**；它没有数字前缀，不在下面的 apply 顺序里。
- `INTEGRATION_DATABASE_URL` / `INTEGRATION_DATABASE_USER` 默认是非敏感配置，放在 `20-configmap.yaml`。若外部地址或账号也需要保密，可改为从 Secret 注入（把 Deployment 的这两个变量改成 `secretKeyRef`，并从 ConfigMap 删除同名键，避免同一变量有两个来源）。
- `KNOWLEDGE_A2A_TOKEN`、`ALERTFLOW_LLM_API_KEY`、`NACOS_USERNAME/PASSWORD` 是可选项，Deployment 里对应 `secretKeyRef` 带 `optional: true`；填占位值或留空均可，但不要填真实值到示例文件。
- `ALERTFLOW_MAIN_MCP_TOKEN`（Java 校验）与 `ALERTFLOW_MCP_TOKEN`（Python 调用）是 MCP 鉴权的两端，**必须一致**；建议用 `openssl rand -hex 24` 生成。不配置时 Pod 正常启动，但 Agent 不会注册 MCP 工具（功能降级）。

## 4. 部署应用（按顺序）

```bash
kubectl apply -f deploy/k8s/10-rbac.yaml
kubectl apply -f deploy/k8s/20-configmap.yaml
kubectl apply -f deploy/k8s/30-services.yaml
kubectl apply -f deploy/k8s/40-deployments.yaml
```

注意：**不要**执行 `kubectl apply -f deploy/k8s/`，那会把 `secret.example.yaml` 示例 Secret 一起创建。
Secret 必须先于 Deployment 存在，否则 Go / Java / Python Pod 会因缺 Secret 卡在 `CreateContainerConfigError`。

## 5. 验证

```bash
# Pod 状态（等待全部 Running 且 READY 1/1）
kubectl -n alertflow get pods -w

# 四个 Deployment / Service
kubectl -n alertflow get deploy,svc

# 组件日志
kubectl -n alertflow logs deploy/alertflow --tail=100
kubectl -n alertflow logs deploy/integration --tail=100
kubectl -n alertflow logs deploy/agent --tail=100
kubectl -n alertflow logs deploy/frontend --tail=100

# 排障
kubectl -n alertflow describe pod <pod-name>
```

预期：四个 Pod `READY 1/1`。探针与端口对应关系：

| 组件 | liveness | readiness |
|---|---|---|
| Go | `GET :8080/healthz` | `GET :8080/readyz`（只检查 DB） |
| Java | `GET :8081/actuator/health/liveness` | `GET :8081/actuator/health/readiness` |
| Python | `GET :8000/health/live` | `GET :8000/health/ready` |
| Frontend | `GET :80/healthz` | `GET :80/healthz` |

Go readiness 只依赖数据库；Kafka、模型 API 不可用不会让 Go 处于未就绪状态。

## 6. 访问 Web 控制台

```bash
kubectl -n alertflow port-forward svc/frontend 13000:80
```

浏览器打开 `http://127.0.0.1:13000`。浏览器只访问 Frontend，由 Frontend 的 Nginx 在集群内部把 `/api/` 代理到 `alertflow:8080`；Java / Python / gRPC 端口不对外暴露。

本期不使用 Ingress Controller。生产环境如需统一入口，由已有网关或 Ingress 暴露 `frontend:80`，不在本清单范围内。

## 7. 启用 Kubernetes 工作负载监控

默认关闭（`ALERTFLOW_K8S_ENABLED: "false"`）。RBAC 已为 Go 的 ServiceAccount `alertflow-go` 配置好：

- `Role alertflow-k8s-reader`：`apps` 组的 `deployments`、`statefulsets`，仅 `get`；
- `RoleBinding alertflow-go-k8s-reader`：绑定到 `alertflow` namespace 下的 `alertflow-go`；
- 没有 `ClusterRole` / `ClusterRoleBinding`。

启用步骤：

```bash
# 1. 修改 ConfigMap 开关与被观察 namespace（namespace 必须与 RBAC 授权的 namespace 一致）
kubectl -n alertflow patch configmap alertflow-go-config --type merge -p \
  '{"data":{"ALERTFLOW_K8S_ENABLED":"true","ALERTFLOW_K8S_NAMESPACE":"alertflow"}}'

# 2. 重启 Go Backend 使配置生效
kubectl -n alertflow rollout restart deploy/alertflow

# 3. 在 Web 控制台为服务新增 Kubernetes 监控目标：
#    resource_kind = Deployment | StatefulSet
#    resource_name = 目标工作负载名（namespace 由 Go 环境变量统一决定，表单不填）
```

**namespace 限制**：本期为单 namespace 授权。若要监控 `alertflow` 以外的 namespace（例如 `default`），需要把 `10-rbac.yaml` 的 Role / RoleBinding 复制到目标 namespace（改 `metadata.namespace`，RoleBinding 的 subject 仍指向 `alertflow` namespace 的 `alertflow-go`），并把 `ALERTFLOW_K8S_NAMESPACE` 改为目标 namespace。

观察器只做单次 `Get`，不使用 Informer / Watch；RBAC 拒绝、网络错误、超时等属于观察器自身错误，不会被记成工作负载故障。

## 8. 配置与资源说明

- ConfigMap 键名就是环境变量名，可直接按需增删；修改后 `kubectl -n alertflow rollout restart deploy/<name>` 生效。
- `JAVA_TOOL_OPTIONS` 默认 `-Xms128m -Xmx512m`，与 Java 容器 `768Mi` 内存上限匹配；如需调整堆，同时调整 `resources.limits.memory`。
- `AGENT_MAX_CONCURRENT_TASKS` / `AGENT_TASK_QUEUE_CAPACITY` 默认 4 / 128，与 Compose 基线一致。
- MCP：`alertflow-agent-config` 的 `ALERTFLOW_MCP_ENDPOINT` 指向 `http://integration:8081/mcp`；token 两端分别是 Java 的 `ALERTFLOW_MAIN_MCP_TOKEN` 与 Python 的 `ALERTFLOW_MCP_TOKEN`，均从 Secret 注入且带 `optional: true`，两者必须一致。`INTEGRATION_MCP_UPSTREAMS` 预留给 Java 的静态 MCP upstream（`provider=endpoint`），本期留空。
- 模型：`ALERTFLOW_LLM_BASE_URL` / `ALERTFLOW_LLM_MODEL` 默认 `https://api.openai.com/v1` / `gpt-4o-mini`；Key 在 Secret 的 `ALERTFLOW_LLM_API_KEY`（可选，留空时 Agent 使用 ScriptedModel）。
- 资源 requests/limits 是按小规模演示环境（约 6G 内存）给出的保守值，未做真实压测；上线前按集群监控数据调整。总量参考：Go 256Mi、Java 768Mi、Agent 768Mi、Frontend 128Mi（均指 limits）。
- `terminationGracePeriodSeconds`：Go / Java / Python 为 45 秒（与后端有 deadline 的优雅退出一致），Frontend 为 15 秒。没有配置 `preStop sleep`。

## 9. Knowledge Agent（可选，本期不部署）

本期 K8s 清单不包含 Knowledge Agent，也不包含 PVC。如需启用知识检索：

1. 单独构建/部署 Knowledge Agent（独立仓库、独立镜像），使用 `/data/knowledge` 的 PVC；
2. 在集群内增加对应 Deployment 与 ClusterIP Service（例如 `knowledge-agent:8000`）；
3. 在 `alertflow-integration-config` 中把 `INTEGRATION_AGENT_CARDS` 指向该地址，并通过 Secret 提供 `KNOWLEDGE_A2A_TOKEN`；
4. Python 侧把 `ALERTFLOW_KNOWLEDGE_ENABLED` 改为 `"true"`（Agent 还会通过 Java 调用 Knowledge Agent）；
5. 外部 PostgreSQL 需提供 `knowledge` 库（含 pgvector 扩展）。

## 10. 已知限制（与任务书一致）

- 单副本，`strategy: Recreate`，升级期间有短暂中断；不提供高可用，不配置 HPA。
- 没有 Ingress、NetworkPolicy、PVC（Knowledge 可选）、多集群 / 多 namespace 授权。
- 不部署 PostgreSQL、Kafka、Redis、Nacos；不包含 Helm / Operator / CRD。
- 控制台没有登录与权限体系，只适合本地或可信内网演示；请通过 port-forward 访问，不要直接暴露到公网。
- 集群内 gRPC 通道没有 TLS；只在集群内部使用。
- Knowledge Agent 不在本期清单内，不提供 PVC / 独立备份方案。
- **本清单未在真实 Kubernetes 集群上运行验证**：交付时只做了 YAML 解析与静态一致性校验（资源名、namespace、SA/RoleBinding、Service 端口、探针端口、ConfigMap/Secret 引用、镜像 tag），以及无集群条件下的 `kubectl --dry-run=client` 尝试（因缺少 API Server 无法完成 discover）。首次部署请按上文顺序执行并检查 Pod 事件与日志。

## 11. 常见问题

| 现象 | 排查方向 |
|---|---|
| `ImagePullBackOff` / `ErrImagePull` | 镜像仓库地址、tag、集群拉取凭据（imagePullSecrets） |
| `CreateContainerConfigError` | Secret `alertflow-secrets` 不存在或缺必填键（DB / Redis / Kafka / Java DB 密码） |
| Go Pod CrashLoop，日志 `database` 错误 | Secret 中 `ALERTFLOW_DATABASE_URL` 地址/账号/密码，或网络不通 |
| Go readiness 一直失败 | `/readyz` 只检查 DB，确认数据库连通与迁移成功（Go 启动会先跑 goose 迁移） |
| Java liveness 重启循环 | Spring Boot 启动较慢，可先加大 `initialDelaySeconds`；确认 Actuator 探针已开启 |
| Agent readiness 失败 | 业务库连接、`/health/ready` 的 gRPC / worker / 连接池状态，见 Agent 日志 |
| 前端页面 502 | Nginx `/api/` 代理目标是否为 `alertflow:8080`；Go Service / Pod 是否就绪 |
| Kubernetes target 一直不更新 | `ALERTFLOW_K8S_ENABLED`、`ALERTFLOW_K8S_NAMESPACE`、资源名/类型、RoleBinding 是否在目标 namespace；观察器错误不记故障 |
| `kubectl port-forward` 被占用 | 换宿主机端口，如 `13001:80` |
