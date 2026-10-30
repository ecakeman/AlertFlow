# AlertFlow on Kubernetes（kind / Kustomize）

本目录是 AlertFlow 的 **本地 kind 部署层**：把同一份 Go 二进制（`alertflow:local` + `ALERTFLOW_ROLE`）、
Agent、PostgreSQL、Kafka、Redis、Fault Lab 拆成独立 workload，并用 Kustomize 管理。
目标环境是**单机 kind 实验集群**，不是生产编排方案（见 §8 已知限制）。

对应规格：《AlertFlow_V2_Cursor_Design_Rev6.4》§6 P0 第 5/11/13 条、§8 P2（K8s 采集与 RBAC）、§9 P3（部署）。
观测栈（Prometheus/Loki/Alloy/MCP/Blackbox）的清单由 observability-agent 维护在
`overlays/observability/`，本目录只负责“加载镜像 + 触发 apply + 冒烟”。

## 1. 目录结构

```
deploy/k8s/
├── README.md                 # 本文件：预算、流程、验收、限制
├── versions.lock.yaml        # 全部镜像/工具版本（唯一来源，禁止 latest）
├── observer-rbac.yaml        # P2：alertflow + fault-lab 两个 ns 的最小只读 Role/RoleBinding
├── base/                     # 环境无关层（namespace=alertflow）
│   ├── namespaces/           # alertflow / observability / fault-lab / runbook
│   ├── config/               # ConfigMap alertflow-env（非秘密参数）、ConfigMap config-seed、PVC
│   ├── rbac/                 # Monitor 独立 SA（alertflow-observer）
│   ├── secrets/              # 只有 *.example 模板，真实密钥不进仓库
│   ├── services/             # 5 个 ClusterIP Service（DNS 契约）
│   └── workloads/            # 每角色一个 Deployment；postgres/kafka 为 StatefulSet
├── overlays/
│   ├── local-kind/           # 单 Node kind：kind-config.yaml + base 引用
│   ├── fault-lab/            # ns fault-lab：Fault Lab Deployment/Service/PVC
│   ├── runbook/              # ns runbook：a2a-adapter（可选，需先给 Agent 镜像加 a2a-sdk）
│   └── observability/        # 观测栈（不属于本目录作者，由 observability-agent 提供）
└── scripts/                  # 10 个可重复执行的运维脚本
```

## 2. 前置条件

| 依赖 | 要求 | 检查 |
| --- | --- | --- |
| WSL2 / Ubuntu 24.04 | 内存 ≥ 6 GiB 可用（本机 available ≈ 3.7 GiB，见预算） | `free -m` |
| Docker | 29.x，可 `docker images` | `docker version` |
| kind | v0.33.0（`~/bin/kind`） | `kind version` |
| kubectl | v1.33.0（`~/bin/kubectl`） | `kubectl version --client` |
| kustomize | v5.7.1（`~/bin/kustomize`） | `kustomize version` |
| 集群 | `kind-alertflow-lab` 已创建，context 名 `kind-alertflow-lab` | `kubectl get nodes` |
| 节点镜像 | `kindest/node:v1.33.0`（从 `docker.m.daocloud.io/kindest/node:v1.33.0` retag） | `docker images kindest/node` |
| 本地镜像 | `alertflow:local`、`alertflow-agent:local`、`fault-lab:local`、`alpine:3.19`、`pgvector/pgvector:pg16`、`apache/kafka:3.9.1`、`redis:7-alpine`、`kindest/local-path-helper:v20241212-8ac705d0`（PVC 供应必需） | `scripts/build-and-load-images.sh` 会校验 |

> 本机无法直连 Docker Hub，节点镜像走 `docker.m.daocloud.io` 镜像源；**节点内 containerd 不能
> 联网拉取**，所有业务/中间件/helper 镜像一律由 `scripts/build-and-load-images.sh` 用
> `docker save | ctr import` 串行导入，清单统一 `imagePullPolicy: IfNotPresent` 写短名。

> **已知环境依赖（缺一项就部署失败，不是可选）**
> - **节点 containerd 不能联网拉取镜像**（节点内 HTTP proxy 指向 `127.0.0.1:7897`，即它自己）。
>   所有镜像必须由 `scripts/build-and-load-images.sh --load-only` 从宿主导入；`deploy-core.sh`
>   会在 apply 前逐个校验渲染结果里的镜像，缺失即失败并提示导入命令。
> - **`kindest/local-path-helper:v20241212-8ac705d0` 必须导入**：kind 自带 local-path-provisioner
>   通过 helper Pod 创建卷，缺这个镜像时 PVC 会卡在
>   `ProvisioningFailed: ... create process timeout`（postgres/kafka/config 的 PVC 都会受影响）。
> - 导入必须**串行**（一次一张）；并行 `docker save | ctr import` 在 6.6 GiB 内存下会把
>   containerd 压崩（实测：bolt 留下 stale sandbox，重启即 fatal 循环，只能重建集群）。
> - 构建期需要国内代理：`GOPROXY=https://goproxy.cn,direct`、`NPM_REGISTRY=https://registry.npmmirror.com`
>   （脚本已默认传入；直连 proxy.golang.org 会 150s 超时）。

## 3. 资源预算（部署前必读）

### 3.1 集群可分配量（实测）

| 项 | 值 | 来源 |
| --- | --- | --- |
| Kind node allocatable CPU | 16 核 | `kubectl get node -o jsonpath={.status.allocatable}` |
| Kind node allocatable memory | 6,938,268 Ki ≈ **6.62 GiB** | 同上（仅用于试算天花板，见 §3.5） |
| Node allocatable pods | 110 | 同上 |
| WSL 总内存 / 当前 available | 6.6 GiB / ≈3.7 GiB | `free -m` |
| StorageClass | `standard`（kind 自带 rancher local-path） | `kubectl get sc` |
| 观测侧总 requests/limits | 1.32 GiB / 2.99 GiB | observability-agent 实测值（2026-10-30） |

### 3.2 核心业务 workload（阶段一口径，部署后按 §3.6 回填实测）

| Pod | 类型 | requests | limits | 说明 |
| --- | --- | --- | --- | --- |
| alertflow-api | Deployment | 100m / 192Mi | 500m / 512Mi | 8080 + 9090，含 DB 迁移与 config 写者 |
| alertflow-monitor | Deployment | 50m / 128Mi | 300m / 384Mi | K8s 采集 + 检查执行，无 Service |
| alertflow-publisher | Deployment | 30m / 64Mi | 200m / 192Mi | 无入站 |
| alertflow-consumer | Deployment | 30m / 64Mi | 200m / 192Mi | 无入站 |
| alertflow-delivery | Deployment | 30m / 64Mi | 200m / 256Mi | 出站 9000 / webhook |
| alertflow-agent | Deployment | 150m / 384Mi | 1 / 1024Mi | 8000 + 9000 |
| alertflow-postgres | StatefulSet | 100m / 256Mi | 1 / 768Mi | PVC 5Gi |
| alertflow-kafka | StatefulSet | 200m / 512Mi | 1 / 1024Mi | KRaft 单 broker，PVC 5Gi |
| alertflow-redis | Deployment | 20m / 32Mi | 200m / 192Mi | 无 PVC |
| **小计（9 Pod）** | | **710m / 1696Mi** | **4600m / 4544Mi** | requests ≈ 1.66 GiB |
| fault-lab | Deployment（ns fault-lab） | 50m / 128Mi | 500m / 384Mi | PVC 1Gi |
| **核心 + fault-lab** | | **760m / 1824Mi ≈ 1.78 GiB** | **5100m / 4928Mi** | requests 占 allocatable **26.9% 内存 / 4.8% CPU** |
| runbook a2a-adapter（可选） | Deployment | 30m / 96Mi | 300m / 384Mi | 需 `a2a-sdk==1.3.0` |

### 3.3 观测栈（数字由 observability-agent 实测，本目录只汇总）

| 组件 | requests | limits |
| --- | --- | --- |
| prometheus | 200m / 256Mi | 500m / 1Gi |
| blackbox-exporter | 20m / 32Mi | 100m / 64Mi |
| loki | 100m / 256Mi | 500m / 512Mi |
| alloy（DaemonSet） | 50m / 128Mi | 500m / 256Mi |
| prometheus-mcp | 30m / 96Mi | 200m / 192Mi |
| loki-mcp（frugal 模式） | 30m / 64Mi | 200m / 192Mi |
| **小计** | **430m / 832Mi** | **2000m / 3064Mi** |

### 3.4 三阶段预算合计与余量

| 阶段 | 内容 | requests 合计 | 占 allocatable | limits 合计 | 占 allocatable |
| --- | --- | --- | --- | --- | --- |
| A | 核心 9 Pod | 710m / 1696Mi | 12.0% / 25.6% | 4600m / 4544Mi | 28.8% / 67.1% |
| B | + fault-lab | 760m / 1824Mi | 12.8% / 26.9% | 5100m / 4928Mi | 31.9% / 72.8% |
| C | + 观测栈 6 个 | 1190m / 2656Mi | 18.6% / 40.9% | 7100m / 7992Mi | 44.4% / **117.6%** |

余量（按 requests 口径，阶段 C）：CPU 14.81 核、内存 ≈3.91 GiB，可调度空间充足；
按 limits 口径内存已 overcommit（7992Mi > 6.62 GiB allocatable），**但 limits 只是上限，不是预留**。
真正的风险在宿主：WSL 只有 6.6 GiB，控制面 + containerd + 各容器 RSS 才是实际占用（见 §3.6 实测）。

### 3.5 预算口径说明（为什么不能“只调低 limits”）

1. **requests 决定能否被调度**，limits 决定单容器被节流/OOM 的天花板；把 limits 调低只会让
   PostgreSQL/Kafka/Agent 在负载峰值被内核 OOMKill，不会让宿主内存变多。
2. Kind 节点的 allocatable 6.62 GiB 来自“宿主内存 - 系统预留”，但宿主同时还跑 WSL、Docker
   daemon、kind node 容器自身开销（≈1.3–1.5 GiB），因此**不能把 6.62 GiB 当可用内存用**。
3. 因此采用**分阶段上线**：先 A（核心），`kubectl top`/`docker stats` 观察真实 RSS，再上 B、C；
   任何阶段出现 OOMKilled/Evicted，先降观测侧采样量（Loki 保留期、Prometheus retention）或
   收紧该组件 limits 至实测 p95 的 1.2–1.5 倍，而不是全局压低。
4. Redis 显式 `--maxmemory 128m`、Kafka `-Xmx512m`、Kafka 单分区副本 = 1，都是“给单机实验
   设上限”，不是生产配置。

### 3.6 实测占用（部署后回填，`scripts/*.sh` 每步都会打印）

| 阶段 | 时间 | `kubectl top pods -A`（或 docker stats） | node 内存 requests / limits | 事件 |
| --- | --- | --- | --- | --- |
| A 核心（9 Pod） | 2026-10-30 21:44 | 节点容器 2.219 GiB / 6.617 GiB，CPU 16.3% | 业务 requests 1696 Mi；节点合计 2844 Mi（41%，含 kube-system 组件） | 无 OOM/Evicted；`kafka` 首启重启 3 次（§7 已说明） |
| B fault-lab（+10 Pod） | 2026-10-30 21:53 | 节点容器 2.67 GiB / 6.617 GiB，CPU 38.55%（含同时段冒烟压测） | 业务 requests 1824 Mi；节点合计 2844 Mi（41%）/ limits 6862 Mi（101%，已 overcommit） | 无 OOM/Evicted |
| C 观测 | 待填（观测栈上线后回填；观测栈自带 requests 1.32 GiB，见 §3.3） | 待填 | 待填 | |

## 4. 部署流程

```bash
# 0) 进入目录（脚本自动定位仓库根、优先使用 ~/bin 下的工具）
cd deploy/k8s

# 1) 创建集群（已存在则跳过，幂等；不会删除已有集群）
scripts/create-cluster.sh

# 2) 构建镜像并导入 kind 节点（app / agent / alpine / PG / Kafka / Redis / 可选 fault-lab）
#    节点 containerd 不能联网拉取，脚本用 docker save | ctr import 逐镜像导入并校验。
scripts/build-and-load-images.sh                  # 自动探测 sibling repo
scripts/build-and-load-images.sh --with-fault-lab --fault-lab-repo ~/projects/fault-lab

# 3) 准备密钥（真实值只在本机 $HOME/.config/alertflow/k8s-secrets.env，0600，repo 外）
mkdir -p ~/.config/alertflow
cp base/secrets/secrets.env.example ~/.config/alertflow/k8s-secrets.env
chmod 600 ~/.config/alertflow/k8s-secrets.env
$EDITOR ~/.config/alertflow/k8s-secrets.env    # 至少填 POSTGRES_PASSWORD、AGENT_* token、FAULT_LAB_CONTROL_TOKEN
scripts/create-secrets.sh                      # 缺失 token 会自动生成并写回本机文件（不回显）

# 4) 部署核心（含 RBAC、PVC、迁移、rollout 等待；渲染结果先做静态检查）
scripts/deploy-core.sh

# 5) 部署 Fault Lab（主故障演练对象）
scripts/deploy-fault-lab.sh

# 6) 冒烟测试：真实 HTTP/gRPC/DB/Kafka 响应，不只是 Pod Ready
scripts/smoke-test.sh
```

访问方式（kind 无 Ingress，全部走 port-forward）：

```bash
kubectl -n alertflow port-forward svc/alertflow-api 8080:8080   # UI: http://127.0.0.1:8080
kubectl -n alertflow port-forward svc/alertflow-agent 8000:8000
kubectl -n alertflow port-forward svc/alertflow-api 9090:9090   # Control gRPC
kubectl -n fault-lab port-forward svc/fault-lab 8090:8090
```

## 5. 故障演练（P2 主场景）

```bash
scripts/inject-fault.sh                 # 默认 http_503：Fault Lab /health 返回 503
# 等待一个 check interval（seed 为 30s）→ 观察 incident / delivery / Agent 任务
kubectl -n alertflow logs deploy/alertflow-monitor -f
kubectl -n alertflow logs deploy/alertflow-agent -f
scripts/restore-fault.sh                # PUT 回注入前的 state（备份 /tmp/alertflow-fault-lab-state.json）
```

`FAULT_MODE` 支持 `http_503|slow|flap|connection_reset`；Fault Lab 的 `/healthz`（自身存活）
永远是 200，被注入的是 `/health`，所以**不会把自己探死**。演练前请先跑通 `smoke-test.sh`。

## 6. 验证清单（对应规格 §9 P3 验收 8 条）

| # | 验收点 | 验证方式 |
| --- | --- | --- |
| 1 | 每个角色独立 workload（非单 Pod 多进程） | `kubectl -n alertflow get deploy,sts` 共 9 个 |
| 2 | API 同时暴露 8080 与 9090 | `smoke-test.sh` 端口检查 + gRPC channel ready |
| 3 | Agent 8000 + 9000 | `smoke-test.sh`（9000 需 `AGENT_TASK_GRPC_TOKEN` 非空） |
| 4 | PG/Kafka PVC `Bound` | `kubectl -n alertflow get pvc` |
| 5 | Go 与 Agent 各自 database + pgvector + Kafka 无 localhost | `smoke-test.sh` psql/pgvector/`--list` 检查 |
| 6 | config.yaml 持久化且 seed 不覆盖 | 一次性 Pod 挂 PVC 只读检查 `cluster_id`；改配置后重启 Pod 不丢 |
| 7 | 跨 namespace Service DNS | `smoke-test.sh` 在 Pod 内访问 `fault-lab.fault-lab.svc.cluster.local` |
| 8 | 所有 Pod 有 requests/limits 与合理探针 | `kubectl -n alertflow get pods -o json` + 本文 §3、§7 |

RBAC 正反向（`system:serviceaccount:alertflow:alertflow-observer`）：

```bash
kubectl auth can-i list pods -n alertflow --as=system:serviceaccount:alertflow:alertflow-observer     # yes
kubectl auth can-i list pods -n fault-lab --as=system:serviceaccount:alertflow:alertflow-observer     # yes
kubectl auth can-i get secrets -n alertflow --as=system:serviceaccount:alertflow:alertflow-observer   # no
kubectl auth can-i create pods -n alertflow --as=system:serviceaccount:alertflow:alertflow-observer   # no
kubectl auth can-i list namespaces --as=system:serviceaccount:alertflow:alertflow-observer            # no
```

`smoke-test.sh` 已包含以上全部断言（真实 HTTP/gRPC/DB/Kafka 响应 + RBAC + endpoints）。

### 6.1 实测记录（2026-10-30，kind v1.33.0，单节点）

| 检查 | 实际结果 |
| --- | --- |
| `kustomize build` 5 个 target | base 18 对象 / local-kind 18 / fault-lab 3 / runbook 2 / observability 28，全部 exit 0 |
| 工作负载 | 9 个核心 workload 全 1/1 Running；Kafka `RESTARTS=3`（首启抖动）；`fault-lab` 1/1 |
| PVC | `alertflow-config` 1Gi、`data-alertflow-postgres-0` 5Gi、`data-alertflow-kafka-0` 5Gi、`fault-lab-data` 1Gi 全部 `Bound` |
| API HTTP | `GET /api/setup/status` → `200 {"needsSetup":true}`；未认证 `GET /api/services` → `401` |
| Agent HTTP/gRPC | `/healthz` → `200 {"ok":true}`；gRPC 9090（api）与 9000（agent）channel ready |
| DB | `pg_isready` OK；`alertflow` 与 `alertflow_agent` 均可 `select 1`；pgvector `0.8.1` 装在 `alertflow_agent` |
| Kafka | 经 Service DNS `--list` 成功（`__consumer_offsets`、`alert-events`） |
| Redis | `redis-cli ping` → `PONG` |
| config seed | PVC 上 `config.yaml` 存在且是 seed 内容（未被覆盖） |
| 跨 namespace DNS | Agent Pod 内可达 `fault-lab.fault-lab.svc.cluster.local:8090` 与 `alertflow-api...:8080` |
| RBAC 正向 | alertflow/fault-lab 的 pods/deployments/statefulsets `list` = yes |
| RBAC 反向 | secrets `get`、pods `create`、kube-system pods `list`、namespaces `list`、clusterroles `create` = no |
| 故障演练 | `inject-fault.sh` → `/health` 503、`/healthz` 200；`restore-fault.sh` → `/health` 200；期间 `incidents` 表记录 2 条 |
| `smoke-test.sh` | 退出码 0（全部 PASS） |

## 7. 探针与生命周期

| workload | readiness | liveness | 说明 |
| --- | --- | --- | --- |
| api | HTTP `GET /api/setup/status`（含 DB 查询） | TCP 8080 | readiness 覆盖 DB；liveness 只看进程，避免 DB 抖动触发重启循环 |
| agent | HTTP `GET /healthz`（仅进程） | TCP 8000 | 不调用 LLM |
| postgres | `pg_isready -U alertflow -d alertflow` | 同左（阈值更宽） | |
| kafka | `kafka-broker-api-versions.sh --bootstrap-server localhost:9092` | 同左 | 用 broker API，不用裸 TCP |
| redis | `redis-cli ping` | 同左 | |
| monitor/publisher/consumer/delivery/fault-lab | 见各自清单 | 见各自清单 | 无 health endpoint 的角色**不伪造 TCP probe** |

**Kafka 首次启动抖动（实测）**：单节点 KRaft 首次格式化 / 选主期间，`alertflow-kafka-0`
可能重启 1–3 次（`RESTARTS` 非 0 是正常的），等待最多 5 分钟即可稳定 1/1；前提是
Service 已配置 `publishNotReadyAddresses: true` 且暴露 Controller 端口 9093，否则 broker
会因 `alertflow-kafka.alertflow.svc:9093` 不可达而注册超时。

所有容器 `SIGTERM` + 有界 graceful shutdown；Go 侧 `readOnlyRootFilesystem` + `runAsNonRoot`
（uid 10001）、Agent 名义上 10000（**当前临时以 root 运行**：上游镜像里
`alertflow_agent/schemas/context.py` 的权限是 600，非 root 读不了，见 §12 第 9 条）、
`automountServiceAccountToken: false`（Agent 不挂 SA token）。
`replicas` 全部为 1，**不宣称 HA**；不设 nodeName/nodeSelector，不做 HPA/PDB。

## 8. 已知限制（务必阅读）

1. **单 Node 实验集群**：config PVC 是 `ReadWriteOnce` + `standard`(local-path)，在 kind 单节点上
   多个 Pod 可同时挂载；**换成多 Node 后 RWO 卷无法跨节点挂载**，必须改 RWX（如 NFS/CephFS）
   或为每个节点准备独立副本并在 README 记录限制。本仓库不默认共享 RWO。
2. **删集群即丢数据**：PVC 都在 kind 节点容器内，`kind delete cluster` 会一并删除；
   重装不能“恢复”数据。`delete-cluster.sh` 必须显式 `--yes --name alertflow-lab` 才执行。
3. **无 Ingress / LoadBalancer / TLS**：只用于本机验证，全部 port-forward。
4. **不追求 HA**：所有 workload 单副本，PG/Kafka 单实例，Kafka 副本因子 1。
5. **内存 overcommit**：阶段 C 的 limits 合计超过 node allocatable（见 §3.4），
   这是刻意的上限而非预留；宿主内存紧张时按 §3.5 收敛观测侧，而不是全局压 limits。
6. **镜像不在集群内拉取**：节点 containerd 不能联网，全部靠 `docker save | ctr import`
   串行导入（含 `kindest/local-path-helper`，缺它 PVC 会 ProvisioningFailed）。新增镜像必须
   同时更新 `versions.lock.yaml` 与 `build-and-load-images.sh`，否则 Pod 会 `ImagePullBackOff`。
7. **runbook overlay 是可选项**：需要 Agent 镜像额外带 `a2a-sdk==1.3.0` 与
   `alertflow_agent.a2a_adapter` 模块；主线 `alertflow-agent:local` 未安装该依赖，默认不部署。

## 9. 与规格的对应关系

| 规格 | 落点 |
| --- | --- |
| §6 P0-5 | 部署前盘点 `deploy/k8s/`（原有 `observer-rbac.yaml` 复用并扩展，未覆盖删除） |
| §6 P0-11 | config 单写者 API + 原子 rename `Save` + seed 只在缺失时写入 + RWO 单节点限制说明 |
| §6 P0-13 | 同镜像多角色拆 Deployment（`ALERTFLOW_ROLE`），端口 8080/9090、8000/9000 同 Pod |
| §8.1 采集配置 | `base/config/config-seed.yaml` 的 `settings.kubernetes.targets`（6 Deployment + 2 StatefulSet + fault-lab） |
| §8.4 RBAC | `observer-rbac.yaml`（两 ns 最小只读，无 ClusterRole/Secret/写权限） |
| §9.1–9.2 | `base/namespaces`、`base/workloads`、`base/services` |
| §9.4 标签 | `app.kubernetes.io/{name,component,part-of}` + `alertflow.io/service-id` |
| §9.5–9.6 | PVC（postgres/kafka/config/fault-lab）、Secret 分层、config 权威文件 |
| §9.7 版本 | `versions.lock.yaml`（含 distroless 固定 digest） |
| §9.8 探针/资源 | 本文 §7、§3 |
| §9.9 脚本契约 | `scripts/*.sh`（前置检查、幂等、错误不吞、Secret 不回显、部署后验证） |

## 10. 卸载

```bash
# 预览：不会删除任何东西
scripts/delete-cluster.sh
# 确认后（会删除整个 kind 集群与全部 PVC 数据）
scripts/delete-cluster.sh --yes --name=alertflow-lab
```

数据损失范围预览：`alertflow-config`（含 UI 修改过的 config.yaml 与图片）、
`data-alertflow-postgres-0`（alertflow 与 alertflow_agent 两个 database）、
`data-alertflow-kafka-0`、`fault-lab-data`。

## 11. 交给其他工作域的事项（本目录不写）

- 根 README 的 K8s 使用说明、`.env.k8s.example`、`docs/v2/` 两份文档：属于其他写入域，需另行更新。
- `overlays/observability/` 全部清单与图片来源：observability-agent 维护；本目录只提供
  `deploy-observability.sh` 执行入口与预算汇总。

## 12. 常见问题（排查顺序）

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| Pod `ImagePullBackOff` | 节点里没有该镜像（节点不能联网拉取） | `scripts/build-and-load-images.sh --load-only`，核对 `ctr -n k8s.io images ls -q` |
| PVC 卡 `ProvisioningFailed: create process timeout` | 节点缺 `kindest/local-path-helper` | 导入清单里补 helper 镜像后重跑 `--load-only` |
| Pod `CrashLoopBackOff` + `database error` | Secret 的 postgres-password 与已初始化的 PG 数据不一致 | 恢复旧密码；或在确认可丢数据后删 postgres PVC 重新初始化 |
| Agent 的 9000 未监听 | `AGENT_TASK_GRPC_TOKEN` 为空（Agent 不启动 task gRPC） | 确认 Secret key `agent-task-grpc-token` 非空后重启 Agent |
| Agent 走 Scripted 模型、不调真实 LLM | 独立 ConfigMap `alertflow-env-local` 不存在（本地密钥文件里 `LLM_BASE_URL`/`LLM_MODEL` 为空） | 填 `$HOME/.config/alertflow/k8s-secrets.env` 后重跑 `create-secrets.sh`，再 `kubectl -n alertflow rollout restart deploy/alertflow-agent`。注意不要写进 `alertflow-env`：它由 kustomize 管理，`deploy-core.sh` 每次 apply 都会重置回空占位 |
| Agent `RuntimeError: LLM_MODEL and LLM_BASE_URL are required when LLM_API_KEY is set` | Secret 里有 `LLM_API_KEY`，但两个非秘密端点值为空 | 同上：`create-secrets.sh` 会从本地文件生成 `alertflow-env-local`（Agent 用 `envFrom` 引用，后者覆盖前者） |
| Agent `RuntimeError: Failed to bind to address :9000` / `unparseable host:port ":9000"` | grpc-python 不接受裸 `:port` | 清单已固定 `AGENT_GRPC_ADDR=0.0.0.0:9000`（ConfigMap `alertflow-env`）；不要用默认值 |
| Agent `PermissionError: .../schemas/context.py` | 镜像内该文件权限 600（上游源码权限问题），非 root 读不了 | 当前 pod `securityContext` 临时以 root 运行（`base/workloads/agent-deployment.yaml`，有注释）；长期应由上游把文件改成 644 并重建镜像后改回 10000 |
| Kafka `0/1` 且日志 `Could not establish connection ... :9093` | Service 未暴露 Controller 9093，或未设 `publishNotReadyAddresses`（broker Ready 前 Endpoints 为空 → 单节点 KRaft 自己连不上自己） | 清单已含 `controller: 9093` 与 `publishNotReadyAddresses: true`；首启重启 1–3 次属正常，等 5 分钟 |
| UI 改的配置重启后还在吗 | config.yaml 在 PVC 上，seed 只在文件不存在时写入 | 持久；想重置需先备份再删 `alertflow-config` PVC（会丢 UI 修改） |
| `kubectl top` 报错 | kind 默认没有 metrics-server | 用 `docker stats` 或 `kubectl get pods -o wide` 观察 |
| 节点 NotReady / containerd 反复重启 | 宿主内存压力或 stale sandbox 元数据 | 由集群管理员重建；先降观测侧负载，再按 §3 预算分阶段重上 |
