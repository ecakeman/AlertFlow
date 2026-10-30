# REV6.4 实施基线（P0 审计交付物）

- 审计时间：2026-10-30
- 分支 / HEAD：`main` / `a5ca5d2a6b1fdd5bedbc1eada7aed5680df6a6dc`（`feat: 让一个服务映射多个 Deployment 和 StatefulSet`）
- 与远端关系：`origin/main...HEAD` = `0 0`，工作区干净
- 规格依据：`AlertFlow_V2_Cursor_Design_Rev6.4_后端规整与系统认知部署整合版.md`（2453 行）
- 本文档地位：**当前唯一规划基线**。`docs/v2/IMPLEMENTATION_BASELINE.md`（269 行）基于 Rev6 及更早口径，其中"P3 新增三个 Specialist"等条目与 Rev6.4 §2.6/§13 直接冲突，仅作历史记录保留，不再作为实施依据。
- 证据分级：`F`=代码/文件审查，`T`=命令与测试，`O`=真实服务观察。本节所有"已实现"结论都有 `F` 或 `T` 支撑；只有 `O` 支撑的条目一律标 `NOT RUN`。

---

## 1. 审计方法与边界

只读审计，未修改任何产品代码。手段：

- `git log/status/rev-list`、`git show --stat`、`git diff --stat 169ccab..a5ca5d2`
- `rg` 全仓符号检索（重点：`RetrievalRequest`、`InvestigationScope`、`TimelineAttachment`、`AgentSender.Encode`、`incident.OnTransition`）
- `go build ./...`、`go vet ./...`、`go test -count=1 -p 1 ./...`（临时 `pgvector/pgvector:pg16` 容器，端口 55432，用后删除）
- `cd agent && uv sync --frozen && uv run --frozen pytest -q`
- `docker compose --profile <p> config --quiet`（6 个 profile）

未做：真实 Kind 集群、真实 Prometheus/Loki、真实 RAGFlow、真实 LLM 调查、Fault Lab 联调。这些全部属于后续阶段，当前一律 `NOT RUN`。

---

## 2. 版本基线

| 组件 | 规格要求 | 仓库实际 | 判定 |
|---|---|---|---|
| Go | go1.25.7 | `backend/go.mod` 声明一致，本机工具链 1.26.6 | 一致（patch 未钉） |
| Gin | v1.10.1 | 一致 | PASS |
| gRPC Go | v1.80.0 | 一致 | PASS |
| client-go / api / apimachinery | v0.35.9 | 一致 | PASS |
| go-redis | v9.7.3 | 一致 | PASS |
| Prometheus MCP | `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0`，endpoint `/mcp` | `docker-compose.yml` 一致 | PASS（不得替换为 `prometheus/prometheus-mcp`） |
| Loki MCP | `grafana/loki-mcp` v0.6.0 本地构建，endpoint `/stream` | 一致 | PASS |
| LangGraph | 按 P0 核对实际解析版 | `agent/pyproject.toml` 钉 `langgraph==1.2.11` | PASS |
| mcp | 1.30.0 | 一致 | PASS |
| RAGFlow | `infiniflow/ragflow:v0.27.3` | 一致 | PASS |
| A2A Python SDK | 1.3.0 | 一致 | PASS |

已知偏差（不阻塞，但必须在 P3/P5 处理）：

1. `Dockerfile:3` 基础镜像为 `distroless:latest`，违反"禁止 latest"。
2. `backend/go.mod` 只钉 minor 版本，patch 未锁定。
3. 仓库无 `deploy/k8s/versions.lock.yaml`（P3 新建）。
4. CI 不构建 Loki MCP（`.github/workflows/test.yml:124-125` 只构建 `alertflow` 与 `alertflow-agent`）。

---

## 3. 进程角色与端点

| 进程 | 镜像 | 端口 | 说明 |
|---|---|---|---|
| alertflow-api | `alertflow:local` | REST `8080` / gRPC `9090` | **同进程两端口**，`ALERTFLOW_ROLE=api`；故障注入不得用 `scale 0` |
| monitor | 同镜像 | 无监听端口 | `ALERTFLOW_ROLE=monitor`，HTTP 探测 + Observer |
| publisher | 同镜像 | 无 | Outbox → Kafka |
| consumer | 同镜像 | 无 | Kafka → `db.ApplyCheckEvent` |
| delivery | 同镜像 | 无 | Delivery worker → Agent |
| agent | `alertflow-agent:local` | FastAPI `8000` / AgentTask gRPC `9000` | 单 Lead Agent + 三个进程内 Manager |
| postgres | `pgvector/pgvector:pg16` | `5432` | AlertFlow 主库 + Agent 库（同实例不同库/角色） |
| kafka | `apache/kafka:3.9.1`（KRaft 单 broker） | `9092`/`29092` | 需 PVC，advertised.listeners 用 Service DNS |
| redis | `valkey/valkey:8`（可选） | `6379` | 仅缓存，不是事实源 |
| prometheus-mcp | `ghcr.io/tjhop/prometheus-mcp-server:v0.18.0` | `8080`/`/mcp` | 只读 |
| loki-mcp | 本地构建 `alertflow-loki-mcp:v0.6.0` | `/stream` | 只读 |

**K8s 拓扑约束**：除 api（REST+gRPC 同进程）与 agent（HTTP+gRPC 同进程）外，其余角色必须独立 Pod。禁止把多个 `ALERTFLOW_ROLE` 合并进一个 Pod，也禁止把 Agent 与 API 合并——"应用由多个 Pod 组成"是 Rev6.4 的硬约束。

---

## 4. Compose profiles

| profile | 内容 | 说明 |
|---|---|---|
| （默认） | postgres、kafka、api、monitor、publisher、consumer、delivery、web-ui | 不含 Agent |
| `agent` | agent | 主调查链 |
| `demo` | fault-lab 等演示容器 | 非本轮主路径 |
| `observability` | prometheus、blackbox-exporter、loki、alloy、prometheus-mcp、loki-mcp | 端点与 K8s 化对象一致 |
| `ragflow` | ragflow、ragflow-redis、ragflow-mysql、ragflow-es、ragflow-minio | 独立网络，不与 AlertFlow Redis 冲突 |
| `runbook` | ragflow-a2a-adapter | 旧名 `multi-agent` 已废弃（避免暗示内部 Multi-Agent） |

6 个 profile 的 `docker compose config --quiet` 全部 exit 0（本轮实测）。

---

## 5. 阶段状态矩阵（截至本基线）

| 阶段 | 判定 | 依据 |
|---|---|---|
| P0 | **PARTIAL** | 审计内容可追溯，但 `REV6_BASELINE.md`（本文）、`BACKEND_BASELINE.md`、`COGNITION_*` 当时缺失；旧 baseline 依据过期规格 |
| P1 | **PARTIAL** | 7.1–7.6 代码级 PASS（含测试），7.7 C0–C5、7.8 B1–B4 **未实施** |
| P2 | **PARTIAL** | Observer 多 target/StatefulSet/重连/gap 代码级 PASS；真实集群 `NOT RUN`；`observer-rbac.yaml` 缺 fault-lab 授权 |
| P3 | **NOT RUN / BLOCKED** | `deploy/k8s/` 仅 `observer-rbac.yaml`；kind/kubectl/kustomize 当时未安装 |
| P4 | **BLOCKED** | 依赖 P3 |
| P5 | **BLOCKED** | 依赖 P3；观测栈仍是 Compose 形态 |
| P6 | **BLOCKED** | 依赖 P3–P5 |

---

## 6. 已实现 / 部分实现 / 未实现

### 6.1 已实现（有代码 + 测试）

- Go 主链路：Monitor 调度 → `PG 事务(Incident + Outbox)` → Publisher → Kafka → Consumer → Delivery lease/retry/dead → Go gRPC → Agent → callback。可逐条对上 `scheduler.go`、`outbox.go`、`consume.go`、`deliver.go`。
- 双向 gRPC：`AlertFlowControlService` 9 个 RPC + `AgentTaskService` 2 个 RPC，与 `proto/alertflow/agent/v1/agent.proto` 完全一致；Bearer 未配置时拒绝；1 MiB 上限。
- Observer：多 target（Deployment/StatefulSet，含同名不同 kind）、owner UID 归属、watch 410/Gone → gap + reconcile、403 → unavailable、Connect 失败后 Sync 重连、DB 写失败不写 ok。
- 认知侧已有：LangGraph 单 Lead 主图、`runtime/context.py` 分块 Prompt、`runtime/verify.py` fact catalog + 三类 findings + 脱敏、`runtime/memory.py` EpisodeCard + pgvector 召回。
- A2A adapter：working/终态收敛、重启恢复、FAILED/CANCELED 不计成功证据、`citations_available` 语义、MIME 一致。
- 配置优先级：非空 shell > `.env` > Compose 默认，`test_compose_priority.py` 覆盖。

### 6.2 部分实现

- Evidence：有 fact catalog（`f1..fN`，`_MAX_FACTS=100`），但无逐条 evidence registry（DB 只有整块 `run_evidence` jsonb）、无 JSON Pointer、无 `fact_kind/unit/trust_level`、无 `coverage`/`provenance`、无 `invalid_document_citations`。
- Memory：EpisodeCard + pgvector 语义召回；无 `TimelineNode`/`TimelineAttachment` 主轴，无附件表。
- Context：`context.py` 有分块渲染，但检索入口分散在 `tools.py`/`mcp.py`，无统一 `RetrievalRequest`。
- 时间窗：**4 套互不相同的算法**——`graph.py:220-259`（occurred−60min..resolved_at）、`tools.py:88-115`（occurred−24h）、`tools.py:179-235`（1h→24h 自适应）、`mcp.py:336-348`（`clamp_window`）；规格要求的唯一 `InvestigationWindow` 不存在。

### 6.3 未实现

- 三个 Manager 的 typed 契约：`RetrievalRequest`、`InvestigationScope`、`TimelineNode`、`EvidenceRecord`、`FactCatalogEntry` 全仓 0 命中。
- `build_investigation_window()`。
- `backend/internal/agentmessage/`（B1 目标包）。
- `deploy/k8s/` 的 Kustomize 体系、脚本、versions.lock。
- `docs/v2/COGNITION_CONTRACTS.md`、`REV6_ACCEPTANCE.md`。

---

## 7. P1 缺陷核对表（7.1–7.6）

| 编号 | 条目 | 状态 |
|---|---|---|
| 7.1 A2A 状态与恢复（8 条） | working 先落库、同 message_id 三态、GetTask 收敛、重启恢复、FAILED 不计证据、无引用 artifact、MIME 一致 | 代码级 **PASS**（7 条）；Postgres ledger 真实持久化 **NOT RUN** |
| 7.2 Runbook evidence（5 条） | content/schema/citations 入 evidence、`source_type=runbook` 进 fact catalog、双 ID 才成 citation、verifier 只验本次、不另建框架 | 代码级 **PASS**；真实 RAGFlow 链路 **NOT RUN** |
| 7.3 RAGFlow 配置（5 条） | 缺配置前置报错、v0.27.3 payload 不变、五态区分、README 修正、Redis 独立网络 | 代码级 **PASS**；真实 completion **NOT RUN** |
| 7.4 Prometheus MCP（6 条） | v0.18.0 + `/mcp`、两种包装解析、窗口计入上限、拒 `offset/@`、精确标签、`probe_success≠up` | 代码级 **PASS**；真实 Prometheus **NOT RUN** |
| 7.5 Loki MCP（3 条） | v0.6.0 + `/stream`、只存可归属结果、四态区分 | 代码级 **PASS**；真实 Loki **NOT RUN** |
| 7.6 配置与 profiles（5 条） | 优先级、脚本不覆盖、不 source `.env`、默认不起观测栈、agent 不接 ragflow 网 | **PASS**（含 6/6 profile config） |

结论：7.1–7.6 不需要重做，只保留回归验证；真正待实施的是 **7.7 认知收敛（C0–C5）** 与 **7.8 后端规整（B0–B4）**。

---

## 8. `deploy/k8s/` 盘点

当前只有 `deploy/k8s/observer-rbac.yaml`（32 行，1 个 ServiceAccount + 1 个 Role + 1 个 RoleBinding，仅 `alertflow` namespace，无 Secret 权限、无写权限、无 ClusterRole）。

缺口：`fault-lab` namespace 的只读 Role/RoleBinding（P4 主故障目标跨 namespace）；Monitor 专用 ServiceAccount 尚未在清单中出现；`ALERTFLOW_K8S_ENABLED` 与 config 卷注入未定义。

---

## 9. P2–P6 拟修改文件与测试入口

| 阶段 | 拟新增/修改 | 测试入口 |
|---|---|---|
| P2 | `deploy/k8s/observer-rbac.yaml`（加 fault-lab 授权、Monitor SA）；`backend/internal/observe/kubernetes/*` 仅修 3 处小缺口 | `go test ./internal/observe/kubernetes`；`kubectl auth can-i` 正反验证 |
| P3 | `deploy/k8s/{base,overlays,scripts,versions.lock.yaml,README.md}`；`Dockerfile` 钉镜像 | `kustomize build`、`kubectl get deploy,sts,ds,pods -A`、`smoke-test.sh` |
| P4 | `deploy/k8s/base/config`（cluster_id/namespace/targets）、Monitor SA | Timeline 中 `alertflow`/`fault-lab` 双 service_id 归属、`runtime_object_states` 重建去重 |
| P5 | `deploy/k8s/overlays/observability/**`、`deploy/observability*/**` | 原生 PromQL/LogQL 与经 MCP 同值比对；Alloy Pod 日志可查 |
| P6 | `deploy/k8s/scripts/{inject,restore}-fault.sh`、Fault Lab sibling 最小补丁（fault_id） | `fault-lab` `http_503` 注入 → 三源证据 → 报告 → 恢复核验 |

---

## 10. 明确不做（本轮）

- 不重跑 P0 全量审计（只做 `c32a8c5..a5ca5d2` 增量复核）。
- 不重写 7.1–7.6 已 PASS 的实现。
- 不部署 RAGFlow（内存不足，标 `SKIPPED`），但必须完成 RunbookQueryV1/ArtifactV1 契约 + Mock A2A 端到端契约测试。
- 不做多 Node / HA / HPA / Operator / Grafana 平台 / OTel Collector。
- 不合并 Pod，不用 `alertflow-api scale 0` 制造故障。

---

## 11. 与旧基线的差异

| 项 | `IMPLEMENTATION_BASELINE.md`（Rev6 口径） | 本基线（Rev6.4） |
|---|---|---|
| 认知架构 | Supervisor + 三 Specialist 并行 | 单 Lead + Context/Evidence/Memory 三 Manager |
| P3 定义 | Multi-Agent 接入 | 原生 K8s 多 Pod 部署 |
| 后端专项 | 无 | B0–B4 |
| 阶段顺序 | P4/P5 部署在最后 | K8s 部署提前到 P3 |

上述变更导致旧的 P3 规划（`Send` fan-out、`specialist.*` 事件）**整体作废**，仓库中也没有任何相关代码落地，无返工成本。
