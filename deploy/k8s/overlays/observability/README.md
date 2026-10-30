# 观测栈 overlay（observability）

规格依据：`AlertFlow_V2_Cursor_Design_Rev6.4` §6 P0 第 14 条、§7.4/§7.5、§9、§11。
本目录是**自包含**的 Kustomize overlay（无 Helm、无 Operator、无 Grafana、无 OTel
Collector），只部署观测设施；业务核心栈由 `deploy/k8s/base/` 部署。

## 内容与版本

| 工作负载 | 清单 | 镜像（版本只在 kustomization.yaml 的 `images:` 维护） | 端点 |
| --- | --- | --- | --- |
| Prometheus | `prometheus/`（StatefulSet，1Gi PVC，72h/1GB retention） | `prom/prometheus:v3.15.1` | `prometheus.observability.svc:9090` |
| Blackbox exporter | `blackbox/`（Deployment） | `prom/blackbox-exporter:v0.29.1` | `blackbox-exporter.observability.svc:9115` |
| Loki | `loki/`（StatefulSet，2Gi PVC，72h retention + compactor） | `grafana/loki:3.5.12` | `loki.observability.svc:3100` |
| Alloy | `alloy/`（DaemonSet，读 `/var/log/pods`） | `grafana/alloy:v1.21.3` | 推送到 Loki |
| Prometheus MCP | `mcp/prometheus-mcp.yaml` | `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` | `http://prometheus-mcp.observability.svc:8080/mcp` |
| Loki MCP | `mcp/loki-mcp.yaml` | `alertflow-loki-mcp:v0.6.0`（本仓库构建） | `http://loki-mcp.observability.svc:8080/stream` |

所有 Service 都是 `ClusterIP`，没有 NodePort/Ingress。Prometheus MCP 以
`--mcp.tools=core` 启动，服务端只注册 9 个工具（含 Agent 允许的 5 个），
`delete_series`/`clean_tombstones`/`snapshot`/`reload`/`quit` 等管理工具不会注册。

## 应用前提（顺序很重要）

1. **核心业务栈已 Ready**：`kubectl -n alertflow get deploy,sts` 全部 AVAILABLE。
   观测栈的 probe 目标 `alertflow-api.alertflow.svc:8080` 与
   `fault-lab.fault-lab.svc:8090` 必须已存在；先 apply 观测栈不会报错，但
   `probe_success=0`、Alloy 也采不到日志。
2. **三个 Namespace 存在**：`alertflow`、`fault-lab`、`observability`。base 会建
   （本 overlay 也重复声明 `observability`，幂等）。Alloy 的 Role 要在
   `alertflow`/`fault-lab` 建对象，所以这两个 Namespace 必须先存在。
3. **镜像已预导入节点**：kind 节点的 containerd 配了不可达的 HTTP proxy
   （`127.0.0.1:7897`），kubelet **不能自己拉镜像**，`crictl pull` 会失败于
   `proxyconnect tcp: dial tcp 127.0.0.1:7897: connect: connection refused`。
   apply 前必须把下面 7 张镜像导入节点（`kind load docker-image` 对多平台镜像会报
   `content digest ... not found`，不要用）：

   ```text
   for img in \
     prom/prometheus:v3.15.1 \
     prom/blackbox-exporter:v0.29.1 \
     grafana/loki:3.5.12 \
     grafana/alloy:v1.21.3 \
     ghcr.io/tjhop/prometheus-mcp-server:v0.18.0 \
     alertflow-loki-mcp:v0.6.0 \
     docker.io/kindest/local-path-helper:v20241212-8ac705d0 ; do
     echo "== $img"
     docker save "$img" |
       docker exec -i alertflow-lab-control-plane \
         ctr -n k8s.io images import --platform linux/amd64 --snapshotter=overlayfs -
   done
   ```

   **必须串行导入**。6.6 GiB 内存下并行 `docker save | ctr import` 会触发内存压力；
   上一次并行导入期间 containerd 被突然杀掉，bolt 元数据留下 stale sandbox，
   重启即 `level=fatal ... failed to get metadata for stored sandbox` 循环，只能重建集群。
4. **PVC 依赖 local-path helper 镜像**：`docker.io/kindest/local-path-helper:v20241212-8ac705d0`
   缺失时 PVC 会卡在 `ProvisioningFailed: ... create process timeout`（helperPod 拉不到
   镜像）。`prometheus-0`/`loki-0` 会一直 Pending。上面第 3 步已包含该镜像。

## 应用与检查

```text
kubectl apply -k deploy/k8s/overlays/observability
kubectl -n observability get pods,pvc,svc
kubectl -n observability rollout status statefulset/prometheus statefulset/loki
kubectl -n observability rollout status daemonset/alloy deployment/blackbox-exporter
kubectl -n observability rollout status deployment/prometheus-mcp deployment/loki-mcp
```

`loki-0` 首次启动 readiness 需要约 20s（ingester 初始化）。Alloy 每节点一个 Pod，
只有 `1/1 READY` 才代表 positions 与 Loki push 正常。

冒烟：

```text
# blackbox → 生成 probe_* 指标（service_id 由 relabel 生成）
kubectl -n observability port-forward svc/prometheus 9090:9090 &
curl -s 'http://127.0.0.1:9090/api/v1/query?query=probe_success' | head -c 600

# Alloy 是否把 label 带到 Loki
kubectl -n observability port-forward svc/loki 3100:3100 &
curl -s -G 'http://127.0.0.1:3100/loki/api/v1/label/service_id/values'
```

完整的 P5 三层一致性验证脚本：`deploy/observability-lab/verify-p5.sh`。

## 资源预算与依据

| 容器 | requests | limits |
| --- | --- | --- |
| Prometheus | 100m / 300Mi | 500m / 512Mi |
| Loki | 100m / 300Mi | 500m / 512Mi |
| Alloy | 50m / 50Mi | 200m / 200Mi |
| Blackbox exporter | 10m / 16Mi | 100m / 64Mi |
| Prometheus MCP | 20m / 32Mi | 200m / 128Mi |
| Loki MCP | 20m / 32Mi | 200m / 128Mi |
| **合计** | **300m / 730Mi** | **1700m / 1544Mi** |

依据：节点总内存 6.6 GiB，控制面约 1.5 GiB，业务核心栈（PG/Kafka/Redis/7 个
Deployment）与 fault-lab 也要占内存。观测栈 requests 730Mi 约占总量 11%，
limits 1.5Gi 约 23%，给业务栈留出余量；Prometheus/Loki 的 512Mi limit 与规格建议
一致（规格 §9），单实例、无 HA、无 HPA/PDB。Prometheus 另有
`--storage.tsdb.retention.size=1GB` 兜底，PVC 1Gi，避免磁盘无限增长；Loki 用
compactor `retention_enabled: true` + `delete_request_store: filesystem` 把 72h 之外
的 chunk 真删掉，PVC 2Gi。

### 内存超卖是有意的，但必须实测 RSS

节点 allocatable 约 6.62 GiB，控制面（apiserver/etcd/controller/scheduler/dns/kindnet）约 1.5 GiB，
core 业务栈 requests 约 1.78 GiB，本 overlay requests 730Mi，而 limits 合计 1544Mi（若把三个
namespace 的 limits 相加会到 7.6 GiB，**超卖** 6.6 GiB 物理内存）。这是有意的：limits 只是
上限，真实占比由实际 RSS 决定，观测栈的 requests 已落在总内存 11% 左右，K8s 调度仍能保证
observability 的 Pod 不会因 requests 竞争被饿死。**但"不会 OOM"必须用实测证明**，不能只靠
requests 数字：

apply 后等 10 分钟（让 Prometheus 抓完几个 scrape、Loki compactor 跑过一轮），执行并把输出存进
`deploy/observability-lab/evidence/`：

```bash
# 1) K8s 视角的 CPU/内存（需要 metrics-server；kind 默认没有则用 2/3）
kubectl -n observability top pods
# 2) 节点容器视角（kind 节点容器本身）
docker stats --no-stream alertflow-lab-control-plane
# 3) 容器级 RSS：crictl 直接问 containerd，不依赖 metrics-server
# 注意：本机 crictl 版本没有 --no-stream（直接 `crictl stats`，输出一帧快照）
docker exec alertflow-lab-control-plane crictl stats \
  | awk '{print $1, $2, $3, $4, $5, $6}'
# 4) 节点剩余内存
docker exec alertflow-lab-control-plane free -m
```

判据：Prometheus/Loki 的 RSS 应明显低于 512Mi limit（否则把 limit 调到 640Mi 并记录原因）；
Alloy 每 Node 一个 Pod、RSS 低于 200Mi limit；节点 `free -m` 的 available 必须仍 > 500MiB，
否则先降 Prometheus/Loki 的 limit 或缩短 retention，再谈加组件。

**实测（2026-10-30T13:51Z，apply 后 >10 分钟，单节点 kind）**：

| 容器 | RSS（`crictl stats`） | limit |
| --- | --- | --- |
| prometheus | 102.3 MB | 512Mi |
| loki | 116.1 MB | 512Mi |
| alloy（修复后稳态） | 84.1 MB（启动/discovery 瞬时约 140 MB） | 200Mi |
| blackbox-exporter | 28.98 MB | 64Mi |
| prometheus-mcp | 79.31 MB | 128Mi |
| loki-mcp | 10.29 MB | 128Mi |

`docker stats --no-stream alertflow-lab-control-plane` → `2.837GiB / 6.617GiB (42.88%)`；
`free -m` → total 6775、used 3606、available **3169**、swap used 1032。
观测栈 RSS 合计 ≈ 420 MB（Alloy 稳态），低于 requests 730Mi，节点 available 3.1 GiB，无内存压力，
无需调整 limits。

## 已知约束与坑

- **`loki.source.file` 不展开 glob**：`discovery.relabel` 产出的
  `__path__=/var/log/pods/*<uid>/<container>/*.log` 不能直接当 `loki.source.file.targets`，
  组件会按字面路径 `stat` 并报 `failed to create source ... stat failed`，Alloy 因无 source
  在打印日志前退出（表现为 liveness `:12345` connection refused、`--previous` 只有
  controller-runtime 噪音）。必须先过 `local.file_match`（`path_targets` + `sync_period`）
  由它把 glob 展开成真实文件 target（见 `alloy-configmap.yaml`；P0_AUDIT §6.1 有原始错误）。
- **Alloy 必须以 uid 0 运行**：`kindest/node:v1.33.0` 的 `/var/log/pods` 是
  `0750 root:root`，CRI 日志文件 `0640 root:root`。Alloy 不 privileged、不挂
  `docker.sock`、不写节点日志目录，只把 `/var/log/pods` 以 `readOnly: true` 挂进来；
  positions 写独立的 hostPath `/var/lib/alloy`。因此**不用** `loki.source.kubernetes`
  fallback，也**不要**改成 `runAsNonRoot`（会 Permission denied，见
  `deploy/observability-lab/P0_AUDIT.md` 的运行时证据）。
- `local-path` 的 PV 是宿主机目录，kubelet 不会应用 `fsGroup`，所以 Prometheus/Loki
  也固定 uid 0 写 PVC；这是实验集群取舍，不值得为它引入 CSI。
- 本 overlay **不引用 `../base`**：base 的 `kustomization.yaml` 由 deploy-agent 维护，
  跨目录引用会让两个写入域互相耦合；`observability` Namespace 的重复声明是无害的。
- 验证输出统一留档：`verify-p5.sh` 自动写 `deploy/observability-lab/evidence/p5-verify-<UTC>.log`；
  资源实测（`top pods`/`crictl stats`/`free -m`）也存同一目录，最终回报必须贴原始行。
- `services/` 之外没有 Ingress：MCP 只给集群内 Agent 用（ClusterIP + 短名），
  不要用 NodePort 暴露，v0.18.0 镜像带有 TSDB 管理工具，即使 `--mcp.tools=core`
  收口了也不应对外。

## 清理

```text
kubectl delete -k deploy/k8s/overlays/observability
```

PVC 不会被删除（`kubectl delete -k` 不删 statefulset 的 volumeClaimTemplates
产生的 PVC）；本地实验要回收磁盘再手动删 PVC。
