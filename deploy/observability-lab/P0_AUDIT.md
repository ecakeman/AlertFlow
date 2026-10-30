# P0 审计：观测栈现状 → K8s 化缺口

范围：规格 §6 P0 第 14 条、§7.4/§7.5、§9、§11。只读审计，未修改被审计文件。
审计对象（8 个文件，全部逐行读完）：

- `deploy/observability/prometheus.yml`（18 行）
- `deploy/observability/blackbox.yml`（8 行）
- `deploy/observability/loki.yml`（24 行）
- `deploy/observability/alloy.alloy`（15 行）
- `deploy/observability/loki-mcp/Dockerfile`（12 行）
- `deploy/observability-lab/README.md`（33 行）
- `deploy/observability-mcp/README.md`（72 行）
- `docker-compose.yml` 观测段（260–320 行，dev profile，仅作现状参考）

P5 产物在 `deploy/k8s/overlays/observability/`（自包含 Kustomize overlay）。

## 1. 现状行为 → K8s 化缺口

| # | 现状行为（file:line） | K8s 化缺口 | P5 处置（overlay 文件） |
| --- | --- | --- | --- |
| 1 | `prometheus.yml:4-18` 只有一个 `job_name: blackbox`，target `http://alertflow:8080/api/setup/status`（compose DNS 短名），`__address__` 重写为 `blackbox-exporter:9115` | K8s 内 `alertflow` 解析不到；没有 fault-lab 主故障 probe；**没有 `service_id` relabel**，P5 无法按 service_id 精确区分 | `prometheus/prometheus-configmap.yaml`：两个 job `blackbox-fault-lab`→`http://fault-lab.fault-lab.svc.cluster.local:8090/health`、`blackbox-alertflow-api`→`http://alertflow-api.alertflow.svc.cluster.local:8080/api/setup/status`；每个 job 加 `target_label: service_id`（值 `fault-lab`/`alertflow`，按 `__param_target` 匹配） |
| 2 | `prometheus.yml:2` 只有 `scrape_interval: 15s`，无 timeout/evaluation | 无显式 `scrape_timeout`（默认 10s）、无 rules 目录语义 | ConfigMap：`scrape_timeout: 10s`、`evaluation_interval: 30s` |
| 3 | compose `prometheus:`（`docker-compose.yml:260-262`）没有 `--storage.tsdb.retention.*`，靠容器临时目录 | K8s 需要 StatefulSet+PVC 才能重启不丢 TSDB；retention 必须有限（规格 §9） | `prometheus/prometheus-statefulset.yaml`：1Gi PVC、`--storage.tsdb.retention.time=72h`、`--storage.tsdb.retention.size=1GB` |
| 4 | `blackbox.yml:1-8` `http_2xx` module：5s timeout、GET、`valid_status_codes: [200]` | 本身可复用；但 fault-lab 的 `/health` 在故障模式下返回 503/延迟/连接重置，module 的 `valid_status_codes: [200]` 正好用来判失败 | `blackbox/blackbox-configmap.yaml` 保持同一 module；Deployment 单副本 + Service `:9115` |
| 5 | `loki.yml:1-24` 只有 filesystem 存储（`/loki/chunks`、`/loki/rules`）、`replication_factor: 1`、`ring: inmemory`；**无 retention、无 compactor** | 无 PVC 语义（容器重建即丢）；retention 无限增长，违反"retention 必须有限" | `loki/loki-configmap.yaml` + `loki-statefulset.yaml`：2Gi PVC，`limits_config.retention_period: 72h`，`compactor.retention_enabled: true` + `delete_request_store: filesystem`，`compactor.compaction_interval: 10m`；保留 `auth_enabled: false`、`ring: inmemory`、schema v13/filesystem tsdb |
| 6 | `alloy.alloy:1-3` `discovery.docker` host `unix:///var/run/docker.sock`；`alloy.alloy:5-8` `loki.source.docker` 挂同一个 socket；`alloy.alloy:10-15` `loki.write` 指向 `http://loki:3100/loki/api/v1/push` | **K8s 节点上没有 docker.sock 语义（containerd + CRI）**，`loki.source.docker` 路线不可用；compose 里 `docker-compose.yml:296` 挂 socket 的做法在 K8s 中也不成立（规格 §11.2 明确禁止 docker.sock） | `alloy/alloy-configmap.yaml`：`discovery.kubernetes role="pod"`（限 `alertflow`/`fault-lab`/`observability` + 字段选择器 `spec.nodeName=$NODE_NAME`）+ `discovery.relabel` 生成 `__path__=/var/log/pods/*$uid/$container/*.log` 与 `namespace/pod/container/app/service_id/cluster` + `loki.source.file` + `loki.process{stage.cri}` + `loki.write` 到 `http://loki.observability.svc.cluster.local:3100/loki/api/v1/push` |
| 7 | `alloy.alloy` 没有任何 service_id 概念；compose 依赖 docker 容器名做 `service_name` | 规格 §11.2 要求 service_id 来自 Pod label `alertflow.io/service-id`，**不得靠 Pod 名猜**；fault-lab 与 alertflow 必须严格区分 | `alloy-configmap.yaml` 的 relabel：`service_id ← __meta_kubernetes_pod_label_alertflow_io_service_id`；无该 label 的 Pod 不打 service_id（不猜）；`cluster` 常量 `kind-alertflow-lab`（与 base/config-seed 一致） |
| 8 | 没有 Alloy RBAC | DaemonSet 需要独立 SA + 最小权限；`discovery.kubernetes` 需要 list/watch pods | `alloy/alloy-rbac.yaml`：SA `alloy`（observability ns）+ 三个 namespace 各一个 Role（**只** `pods` `get/list/watch`）+ RoleBinding；无 ClusterRole、无 pods/log、无 nodes |
| 9 | `deploy/observability/loki-mcp/Dockerfile:1-12` 构建 v0.6.0，`debian:bookworm-slim` 运行、`PORT=8080`、无 USER | CI/清单规范路径不一致（规格要求镜像由本仓库构建；CI 需固定 context）；镜像默认 root | 新增 `deploy/observability/loki-mcp/Dockerfile`（与旧路径等价，注释指向 CI）；K8s Deployment 强制 `runAsNonRoot: true`/`runAsUser: 65532`、`readOnlyRootFilesystem: true`、drop ALL |
| 10 | `observability-mcp/README.md` 只写 compose 端口（8080/8088/8089）与 `docker run`，无 K8s 章节 | 干净环境无法从文档复现 K8s 部署（端点、ClusterIP-only、镜像导入） | 已追加 "Kubernetes overlay" 章节（含镜像表、`--mcp.tools=core`、CI `build-loki-mcp` job、kind 导入命令）；overlay 的 `README.md` 写应用顺序与资源预算 |
| 11 | `observability-lab/README.md:7-18` 示例用 `service` label（`PROMETHEUS_SERVICE_SELECTORS={"checkout":"service=\"checkout\""}`），`LOKI_MCP_URL=http://loki-mcp:8080/stream` | P5 要求 `service_id` 精确绑定（`{"fault-lab":"..."}`）；compose DNS `loki-mcp:8080` 在 K8s 是 `loki-mcp.observability.svc:8080` | overlay 全链路统一 `service_id`；Agent 侧 env 建议见 §5（selector 映射由 lead/backend 配置） |
| 12 | P5 之前没有验证脚本 | 三层一致性（原生 API → MCP → Agent Evidence）无自动化检查 | 新增 `deploy/observability-lab/verify-p5.sh`（原生 Prometheus API / 原生 LogQL / 两个 MCP 各查同值，自动 port-forward，PASS/FAIL 汇总） |

## 2. Alloy 采集路径：hostPath vs `loki.source.kubernetes`（规格 §11.2 / §6 P0-14）

结论：**走 hostPath + `loki.source.file`，不 fallback 到 `loki.source.kubernetes`**。

实测证据（本地真实镜像，非推断）：

- **EV-1 只读挂载可 tail 文件（PASS）**。`grafana/alloy:v1.21.3` 容器把目录以 `/var/log/pods:ro` 挂入，`loki.source.file` 正常 tail：
  `level=info msg="start tailing file" ... path=/var/log/pods/demo/demo.log`；
  Alloy 自身指标 `loki_source_file_read_lines_total=2`、`read_bytes_total=149`、`files_active=1`；
  日志成功推进到 `grafana/loki:3.5.12`。说明"只读 hostPath + 文件 tail"不是阻塞。
- **EV-2 CRI 格式解析（PASS）**。fake CRI 行
  `2026-10-30T12:00:00.123456789Z stdout F alertflow-smoke-marker fault-lab log line`
  经 `stage.cri {}` 后，Loki 查到 `ts=2026-10-30T12:00:00.123457Z | alertflow-smoke-marker fault-lab log line`，
  stream 标签 `stdout`。⇒ CRI 前缀被剥掉、行首时间戳自动成为 entry timestamp、无需再写
  `stage.timestamp`（Loki 精度到微秒，纳秒被截断）。
- **EV-3 target labels 即 stream labels（PASS，决定性）**。把 `cluster/namespace/pod/container/app/service_id`
  直接作为 target labels 后，Loki 中同一条日志的 stream 为
  `{app=alertflow-api, cluster=kind-alertflow, container=alertflow-api, filename=/var/log/pods/pod-uid-123/alertflow-api/0.log, namespace=alertflow, pod=alertflow-api-abc, service_id=alertflow, service_name=alertflow-api, stream=stdout}`，
  entry timestamp 保留 CRI 纳秒（`1793365200000000001`）。
  ⇒ `service_id` 由 `discovery.relabel` 从 Pod label 复制即可，不需要 `stage.labels`/`stage.static_labels`。
- **EV-4 重复 target 去重（PASS）**。`discovery.kubernetes role=pod` 对多端口容器会为同一容器生成多个
  target；`loki.source.file` 对 `__path__` 与标签集完全相同的 target 去重（同 target 重复投递两次，
  `read_lines_total=1`、`files_active_total=1`）。⇒ 无需额外 `discovery.relabel` 去重逻辑（`__meta_*`
  标签默认被剥掉，端口标签不进最终标签集）。
- **EV-5 运行目录权限（运行时实测，2026-10-30T13:09，节点 `alertflow-lab-control-plane`）**：
  `/var/log/pods` 由 kubelet 运行时创建，镜像 `kindest/node:v1.33.0` 内**不存在**该目录
  （`docker run --rm --entrypoint /bin/sh docker.m.daocloud.io/kindest/node:v1.33.0 -c
  'ls -ld /var/log/pods'` → `ls: cannot access '/var/log/pods': No such file or directory`，exit 1），
  所以权限只能运行时测量。原始输出：

  ```text
  $ docker exec alertflow-lab-control-plane sh -c 'ls -ld /var/log/pods; stat -c "%a %U:%G %n" /var/log/pods'
  drwxr-x--- 11 root root 4096 Oct 30 13:09 /var/log/pods
  750 root:root /var/log/pods

  $ docker exec alertflow-lab-control-plane sh -c 'find /var/log/pods -name "*.log" | head -1 | xargs stat -c "%a %U:%G %n"'
  640 root:root /var/log/pods/kube-system_kindnet-64v9k_ded12c76-2379-45a8-a35a-51f18b13b21d/kindnet-cni/0.log

  $ head -2 /var/log/pods/kube-system_kindnet-64v9k_.../kindnet-cni/0.log
  2026-10-30T13:09:43.486276646Z stderr F I1030 13:09:43.485883       1 main.go:390] probe TCP address alertflow-lab-control-plane:6443
  2026-10-30T13:09:43.487877819Z stderr F I1030 13:09:43.487681       1 main.go:109] connected to apiserver: https://alertflow-lab-control-plane:6443
  ```

  结论：目录 `0750 root:root`、CRI 日志文件 `0640 root:root`；目录名格式
  `<namespace>_<pod>_<podUID>`、文件路径 `<dir>/<container>/<n>.log`，与 Alloy relabel 的
  `__path__ = "/var/log/pods/*<podUID>/<container>/*.log"` 匹配（`*` 覆盖
  `<namespace>_<pod>_` 前缀）。
  ⇒ Alloy 必须以 uid 0 运行（`runAsUser: 0`），但**不 privileged、不挂 docker.sock、不写节点
  日志目录**，`/var/log/pods` 只读挂载；positions 写独立 hostPath `/var/lib/alloy`（每 Node
  独立）。root + readOnly hostPath 是 K8s 日志 Agent 的标准做法（Fluent Bit DaemonSet 同样），
  不构成 fallback 理由。真实 CRI 行也验证了 `stage.cri` 需要的格式（`<time> <stream> <flag> <msg>`）。

- **EV-6 静态标签坑**。`stage.labels { values = { cluster = "kind-alertflow" } }` 的语义是"值 = 从
  extracted map 取的字段名"，写常量会得到空标签并被丢弃；静态常量必须用 `stage.static_labels`。
  生产配置不用任何 labels stage（target labels 已经够），这条只作为审计记录。

为什么不需要 `loki.source.kubernetes`（API tail）：

1. 它要用 `pods/log` + `pods` 权限，且日志流经 API server/kubelet，吞吐与延迟都比读本地文件差；
2. 规格 §11.2 明确要求"读取每个 Node 的 `/var/log/pods`（readOnly hostPath）"；只有"宿主机/Kind
   无法安全读取 `/var/log/pods`"才允许给证据后 fallback；EV-1 已证明只读挂载 + tail 可行，EV-5 的
  运行时权限原始输出在集群重建后补录；
3. 最小 RBAC：hostPath 路线只需 `pods get/list/watch`（discovery 用），不需要 `pods/log`。

RBAC 最小集（`alloy/alloy-rbac.yaml`）：

- SA `alloy` 在 `observability`；`alertflow`/`fault-lab`/`observability` 三个 namespace 各
  `Role`（`pods`：`get/list/watch`）+ `RoleBinding`；
- 没有 `ClusterRole`/`ClusterRoleBinding`：`discovery.kubernetes` 的 `namespaces{}` 限定后只走
  namespaced list/watch；
- 不需要 `nodes`、`namespaces`、`pods/log`（不用 `attach_metadata`，node 过滤用 field selector）。

## 3. MCP tools/list 实测核对（规格 §7.4/§7.5、§11.1/§11.3）

方法：直接 `docker run` 指定镜像，POST `initialize` → `tools/list`（真实响应，非文档推断）。

### 3.1 Agent 侧 allowlist 逐字（`agent/src/alertflow_agent/integrations/mcp.py`）

```python
# mcp.py:16-20（原文）
PROMETHEUS_TOOLS = ("query", "range_query", "metric_metadata", "label_names", "label_values")
LOKI_TOOLS = ("loki_query",)
SCOPED_TOOLS = frozenset({"query", "range_query", "label_values", "loki_query"})
EVIDENCE_TOOLS = SCOPED_TOOLS
APPROVED_TOOLS = frozenset(PROMETHEUS_TOOLS) | frozenset(LOKI_TOOLS)
```

即 Prometheus 侧允许的 **5 个**名字逐字为：`query`、`range_query`、`metric_metadata`、
`label_names`、`label_values`；Loki 侧只允许 `loki_query`。每次 MCP 调用前 Agent 先
`initialize` 再 `tools/list`，缺任一名字即抛 `MCPError("missing_tool")`（mcp.py:120-140 的
`source` 选择与 `_tools_for_settings` 拼接逻辑），所以"服务端没注册"会直接表现为调用失败而非静默。

### 3.2 Prometheus MCP v0.18.0 `/mcp` 实测事实

- 默认 `--mcp.tools=all` 暴露 **28** 个工具（含 `delete_series`、`clean_tombstones`、`snapshot`、
  `reload`、`quit` 等管理工具）。
- 本 overlay 用 `--mcp.tools=core`：实测注册 **9** 个：
  `docs_list`、`docs_read`、`docs_search`、`label_names`、`label_values`、`metric_metadata`、
  `query`、`range_query`、`series`。
- **事实一（当前契约不破）**：上表的 5 个允许名字全部在 core 的 9 个里，逐个字符串相等，
  Agent 的 `tools/list` 校验不会失败。
- **事实二（开关的安全性不能说满）**：`--mcp.tools=core` 是**粗粒度档位**，不是"只放这 5 个"。
  实测 v0.18.0 **不支持**显式列表（`--mcp.tools=query,range_query` 会打印
  `msg="Failed to find tool to register"` 并只注册 core 的 9 个），因此无法用该 flag 精确收口到 5 个；
  剩余面 `series` 与 `docs_*` 是只读的，仍由网络层（ClusterIP-only、不暴露 NodePort/Ingress）
  和 Agent allowlist 双重约束。若未来需要"服务端也只暴露 5 个"，必须换镜像/升级（§7.4 禁止），
  当前方案里只能用"网络不可达 + Agent 白名单"表达，这一点在 §11.1 的可接受范围内但要写进风险。

### 3.3 被 core 挡掉的 19 个，以及"未来会不会用到"

被挡 19 个：`alertmanagers`、`build_info`、`clean_tombstones`、`config`、`delete_series`、
`exemplar_query`、`flags`、`healthy`、`list_alerts`、`list_rules`、`list_targets`、`quit`、`ready`、
`reload`、`runtime_info`、`snapshot`、`targets_metadata`、`tsdb_stats`、`wal_replay_status`。

| 类别 | 工具 | 未来被 Agent 用到的可能性 | 说明 |
| --- | --- | --- | --- |
| 写/管理（必须挡） | `delete_series`、`clean_tombstones`、`snapshot`、`reload`、`quit` | 不应使用 | 会改 TSDB/生命周期，任何调查场景都不允许 |
| 只读诊断（可能有用） | `list_targets`、`targets_metadata`、`list_rules`、`list_alerts`、`config`、`flags`、`runtime_info`、`healthy`、`ready`、`build_info`、`tsdb_stats`、`wal_replay_status`、`alertmanagers` | 低—中 | 若将来做"抓取目标是否含 fault-lab""规则是否加载""Prometheus 自身健康"这类调查，会想用 `list_targets`/`targets_metadata`/`list_rules`；当前 P5 的 Evidence 契约只用 5 工具，故挡掉不阻塞 |
| 查询扩展（可能有用） | `exemplar_query` | 低 | 仅当引入 exemplars 时才有意义 |

结论：**当前 5 工具不被挡 = 已验证；"core 开关等于最小权限" = 不成立**。真正的最小权限由
`--mcp.tools=core`（去掉 5 个写工具）+ ClusterIP-only（外网/其他 ns 不可达）+ Agent allowlist
三层叠加实现；`series` 和 `docs_*` 属于 core 的固定附加面，属已知残余风险，不属 P5 阻塞项。

### 3.4 参数 schema 与 Agent 实参对照

- `query`：仅 `query` 必填，`timestamp`/`truncation_limit` 可选；
- `range_query`：`query` 必填，`start_time`/`end_time`/`step`/`truncation_limit` 可选；
- `label_values`：`label` 必填，`matches` 数组 + `start_time`/`end_time`/`truncation_limit` 可选；
- `label_names`/`metric_metadata` 无必填（`metric` 可选）。

与 Agent `_mcp_request`（`agent/src/alertflow_agent/runtime/tools.py:421-501`）构造的实参逐项一致：
`query`→`{query,timestamp,truncation_limit}`、`range_query`→`{query,start_time,end_time,step,truncation_limit}`、
`label_values`→`{label,matches:[selector],start_time,end_time,truncation_limit}`；时间用 RFC3339，
v0.18.0 文档明确接受 RFC3339/Unix epoch/时长字符串，故不会因格式被拒。

### 3.5 Loki MCP v0.6.0（本仓库构建）`/stream`

暴露 3 个工具：`loki_query`（必填 `query`；可选 `start`/`end`/`limit`/`format`/`token`/`url`/`org`/
`username`/`password`）、`loki_label_names`、`loki_label_values`。Agent 只调用 `loki_query`
（`LOKI_TOOLS=("loki_query",)`），实参 `{query,start,end,limit}` —— 名称与 schema 匹配；
镜像同时服务 `/sse` 与 `/mcp`，Agent 必须用 `/stream`。`url`/`token` 等可选参数由服务端 env
提供，清单不下发 secret。

## 4. 验证命令（供 lead 在集群 Ready 后执行）

一键脚本：`deploy/observability-lab/verify-p5.sh`（默认自动 `kubectl port-forward` 四个 Service，
退出码 0=PASS/1=FAIL/2=前置不足；**所有输出自动 tee 到
`deploy/observability-lab/evidence/p5-verify-<UTC>.log`**，最终回报贴该文件的原始行）。
等价的单条命令：

```text
# 原生 Prometheus API（probe_* 才是 Blackbox 结论；up 只代表 scrape 成功）
kubectl -n observability port-forward svc/prometheus 19090:9090 &
curl -sG http://127.0.0.1:19090/api/v1/query --data-urlencode 'query=probe_success{service_id="fault-lab"}'
curl -sG http://127.0.0.1:19090/api/v1/query --data-urlencode 'query=probe_duration_seconds{service_id="fault-lab"}'
curl -sG http://127.0.0.1:19090/api/v1/query --data-urlencode 'query=probe_http_status_code{service_id="fault-lab"}'

# 原生 LogQL（Alloy 采到的 fault-lab 日志；窗口 1h）
kubectl -n observability port-forward svc/loki 13100:3100 &
curl -sG http://127.0.0.1:13100/loki/api/v1/query_range \
  --data-urlencode 'query={service_id="fault-lab"}' \
  --data-urlencode "start=$(( $(date +%s) - 3600 ))000000000" \
  --data-urlencode "end=$(date +%s)000000000" --data-urlencode 'limit=100'

# 经 Prometheus MCP 查同一个 PromQL（/mcp）
kubectl -n observability port-forward svc/prometheus-mcp 19091:8080 &
#   POST initialize → 取 Mcp-Session-Id → POST tools/call
#   {"name":"query","arguments":{"query":"probe_success{service_id=\"fault-lab\"}"}}

# 经 Loki MCP 查同一个 LogQL（/stream）
kubectl -n observability port-forward svc/loki-mcp 19092:8080 &
#   POST initialize → POST tools/call
#   {"name":"loki_query","arguments":{"query":"{service_id=\"fault-lab\"}",
#    "start":"<RFC3339 -1h>","end":"<RFC3339 now>","limit":100}}
```

MCP 的 `initialize`/`tools/call` 原始 curl 流程（session 头、SSE/JSON 两种响应）见
`verify-p5.sh` 的 `mcp_call` 实现。

## 5. P5 验收映射与待执行项

| P5 验收项 | 落点 | 状态 |
| --- | --- | --- |
| fault-lab 独立 Service 为主故障 probe | `prometheus-configmap.yaml` job `blackbox-fault-lab` | **已验证**（集群实测 probe_success/probe_*） |
| alertflow-api 自观察 probe | job `blackbox-alertflow-api` | **已验证**（`/api/setup/status` 返回 200） |
| relabel 生成 `service_id` 且精确匹配 | 两个 job 的 relabel（按 target 域名匹配，不靠子串） | **已验证**（两个 service_id 各自独立 series） |
| Prometheus 固定版本 + PVC + 有限 retention | StatefulSet `prom/prometheus:v3.15.1`、1Gi、72h/1GB | **已验证**（prometheus-0 1/1，PVC Bound） |
| Loki 单实例 + PVC + 24–72h retention | StatefulSet `grafana/loki:3.5.12`、2Gi、72h + compactor | **已验证**（loki-0 1/1，查询返回真实 stream） |
| Alloy DaemonSet 每 Node 一 Pod、读 `/var/log/pods` | `alloy-daemonset.yaml`（本机单节点） | **已验证**（alloy 1/1，日志显示 tail 真实 Pod 路径） |
| Alloy 独立 SA + 最小 Role（pods get/list/watch） | `alloy-rbac.yaml` | **已验证**（discovery 未出现 403） |
| service_id 区分 alertflow / fault-lab | relabel `__meta_kubernetes_pod_label_alertflow_io_service_id` | **已验证**（`/label/service_id/values` = ["alertflow","fault-lab"]） |
| Prometheus MCP v0.18.0 `/mcp` 五工具 | `mcp/prometheus-mcp.yaml`（`--mcp.tools=core`） | **已验证**（集群内 tools/list 含 5 工具、无 delete_series） |
| Loki MCP v0.6.0 `/stream` `loki_query` | `mcp/loki-mcp.yaml` + `deploy/observability/loki-mcp/Dockerfile` | **已验证**（tools/list 含 loki_query，返回与原生同段日志） |
| 原生→MCP→Agent 三层一致 | `verify-p5.sh` | **PASS**（正常态 `evidence/p5-verify-20261030T135503Z.log` 全绿；注入态 `...T135136Z.log` 记录 2 项预期 WARN） |
| 任一 MCP 禁用不阻塞调查 | Agent 侧（`PROMETHEUS_MCP_URL`/`LOKI_MCP_URL` 置空） | 不属本域，给出建议：`PROMETHEUS_SERVICE_SELECTORS={"fault-lab":"service_id=\"fault-lab\"","alertflow":"service_id=\"alertflow\""}`、`LOKI_SERVICE_SELECTORS={"fault-lab":"{service_id=\"fault-lab\"}","alertflow":"{service_id=\"alertflow\"}"}` |

已知 P0 结论：本机 Kind node image `kindest/node:v1.33.0` 上 hostPath 路线可行（EV-1/EV-5），
无 fallback 需求；`loki.source.kubernetes` 仅在后续遇到"节点不允许 root 读 `/var/log/pods`"时
才作为备选，且需要 `pods/log` 权限。

## 6. 集群实测：Alloy glob 坑、P5 三层结果与资源占用（2026-10-30）

### 6.1 关键新发现：`loki.source.file` 不展开 glob（已修复）

首版 Alloy 配置把 `discovery.relabel` 产出的 `__path__=/var/log/pods/*<uid>/<container>/*.log`
直接交给 `loki.source.file.targets`。DaemonSet Pod 在打印任何 Alloy 启动日志前被 liveness 杀掉
（`exitCode 143`），`--previous` 只有 controller-runtime 的 `log.SetLogger(...) was never called`
噪音；用同配置、同挂载、去掉探针的 debug Pod 复现后拿到真因：

```
failed to create source, skipping component_id=loki.source.file.pods
  error="failed to tail file, stat failed: stat /var/log/pods/*<uid>/<container>/*.log: no such file or directory"
```

⇒ `loki.source.file` 按字面路径 `stat`，**不会**像 `local.file_match` 那样做 glob 展开；
进程退不退出取决于是否有可 tail 的源（本 Pod 的路径恰好不存在 → 无 source → 进程退出）。

修复（`deploy/k8s/overlays/observability/alloy/alloy-configmap.yaml`）：

```alloy
local.file_match "pods" {
  path_targets = discovery.relabel.pods.output
  sync_period  = "5s"
}
loki.source.file "pods" {
  targets    = local.file_match.pods.targets
  forward_to = [loki.process.pods.receiver]
  tail_from_end = false
}
```

并把 DaemonSet liveness `initialDelaySeconds` 由 20 提到 60、`failureThreshold` 6（首启+discovery 同步更从容）。
修复后 `alloy fmt` exit 0、`kubectl rollout status ds/alloy` exit 0，`alloy-th57b 1/1 Running 0 restarts`。

实跑证据（`kubectl -n observability logs ds/alloy | grep 'start tailing file'`）：

```
msg="start tailing file" component_id=loki.source.file.pods
  path=/var/log/pods/alertflow_alertflow-consumer-557dd77d66-rv577_3aaf281d-.../consumer/0.log
msg="start tailing file" component_id=loki.source.file.pods
  path=/var/log/pods/observability_prometheus-0_f793d556-.../prometheus/0.log
msg="start tailing file" component_id=loki.source.file.pods
  path=/var/log/pods/alertflow_alertflow-kafka-0_3e982a4a-.../kafka/2.log
```

### 6.2 P5 三层一致（`deploy/observability-lab/verify-p5.sh`，exit 0）

evidence：`deploy/observability-lab/evidence/p5-verify-20261030T135136Z.log`

- 原生 Prometheus API：`up{service_id="fault-lab"}=1`、`up{service_id="alertflow"}=1`；
  `probe_success{service_id="alertflow"}=1`、`probe_http_status_code=200`、
  `probe_duration_seconds=0.00499`；`probe_success{service_id="fault-lab"}=0` 与
  `probe_http_status_code=503` 是 fault-lab 当时处于 `http_503` 注入态（`/control/v1/state`
  读到 `health.mode=http_503`）的**正确**观测结果，脚本记为 WARN 而非 FAIL。
- 原生 LogQL：`{service_id="fault-lab"}` 近 1h 100 行、`{service_id="alertflow"}` 100 行；
  `/loki/api/v1/label/service_id/values` = `["alertflow","fault-lab"]`。
- MCP：Prometheus MCP `tools/list` 含 `query/range_query/metric_metadata/label_names/label_values`
  且无 `delete_series`；`query` 返回 `probe_success ... => 0 @[...]`，值落在原生最近 10 分钟采样集合 `[0 1]` 内；
  Loki MCP `loki_query` 返回文本含原生 LogQL 同一行且带 `service_id` 流标签。
- 结论行：`RESULT: PASS（三层一致；2 项 WARN 为故障注入期间的预期不健康状态）`。

**正常态复跑（fault-lab mode=normal，`evidence/p5-verify-20261030T135503Z.log`）**：

```
PASS  probe_success{service_id="fault-lab"} == 1（目标健康）
PASS  probe_http_status_code{service_id="fault-lab"} = 200
PASS  probe_success{service_id="alertflow"} == 1（目标健康）
PASS  LogQL {service_id="fault-lab"} 近 1h 有 100 行
---   Loki label service_id values: {"status":"success","data":["alertflow","fault-lab"]}
PASS  MCP probe_success=1 与原生 API 一致（原生最近采样 [0 1]）
PASS  MCP 返回日志含原生 LogQL 同一行（内容一致）
RESULT: PASS（原生 API、LogQL、MCP 三层一致）
```

exit 0，无 WARN。结合上一份注入态日志，证明链路对 healthy / 503 两种状态
的观测都正确（不是"永远 PASS"的假绿）。

### 6.3 资源占用实测（apply 后 >10 分钟，`crictl stats` 单节点）

| 容器 | MEM | 约束（limit） |
| --- | --- | --- |
| prometheus | 102.3 MB | 512Mi |
| loki | 116.1 MB | 512Mi |
| alloy（修复后稳态） | 84.1 MB（启动/discovery 瞬时约 140 MB） | 200Mi |
| blackbox-exporter | 28.98 MB | 64Mi |
| prometheus-mcp | 79.31 MB | 128Mi |
| loki-mcp | 10.29 MB | 128Mi |

节点：`alertflow-lab-control-plane 2.837GiB / 6.617GiB (42.88%)`；`free -m`：total 6775、
used 3606、available 3169、swap used 1032。⇒ 观测栈实际 RSS 合计 ≈ 420 MB（Alloy 稳态），
低于 overlay requests 合计 730Mi，更远低于 limits 1544Mi；节点 available 3.1 GiB，无内存压力。
（注：Loki 刚启动 RSS 会随 compactor/ingester 增长，72h retention + 2Gi PVC 上限内可控。）
