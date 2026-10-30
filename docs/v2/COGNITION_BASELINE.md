# COGNITION_BASELINE — Python Agent 认知侧 P0 审计基线

审计对象：`agent/`（HEAD `a5ca5d2` + 本轮工作区改动），WSL Ubuntu 24.04。
方法：只读代码与测试（`grep`/逐文件精读），不依赖线上环境；凡无代码+测试双重证据者标 **UNVERIFIED**。
规格依据：`AlertFlow_V2_Cursor_Design_Rev6.4…md` §6 第 7–10 条、§7.1–7.3、§7.7。
结论落地见 `docs/v2/COGNITION_CONTRACTS.md`（C1–C5 目标契约）。

---

## 1. §6-7 主图与 runtime/*：I/O、持久化、调用者、写入者、测试

### 1.1 主图（`runtime/graph.py`）

- 状态结构：`GraphState(TypedDict, total=False)`（graph.py:32），字段含 `run_id/mode/event/evidence/
  raw_checks/series/coverage/skills/plan/messages/report/verification/guard/prior/fact_catalog/
  llm_report/provided_context/mcp_tools/window/evidence_registration/evidence_registry/report_verification` 等。
- 节点装配：`Runner._graph()`（graph.py:345）：
  `bootstrap → collect_alertflow → collect_diagnostics → analyze → recall → select_skills →
  initial_plan → lead_model ↔ (lead_tools | replan) → write_report → verify → persist → remember → END`，
  与 §7.7.9 要求一致；无独立 Specialist/Reviewer Agent 节点（`delegate_history_review` 是工具，不是图节点）。
- 入口调用者：`main.py:102 runner.execute(row["id"], resume=...)`，Runner 在 `main.py:36` 构造；
  run 来源为 Go 侧 gRPC `AgentTaskService.StartInvestigation/StartChat` 落库后的 run 行。
- 持久化（写入者）：`persist()`（graph.py:896）→ `store.save_evidence()` + `store.commit_report()`；
  `remember()`（graph.py:989）→ `runtime.memory.remember()` → episodes/service_profile/timeline_attachments。
- 测试：`tests/test_graph.py`（节点产物 `ana:status@0`、report/usage）、`tests/test_agent_context.py`
  （`diagnostic_context` 直采路径与 coverage from/to）、`tests/test_diagnostics.py`、`tests/test_chat_worker.py`。

### 1.2 runtime/ 模块边界与 I/O

| 模块 | 输入 | 输出/写入 | 主要调用者 | 测试 |
| --- | --- | --- | --- | --- |
| `context.py` | node 名 + state | prompt 文本 / `Fit` / 六区块 `PromptContextBundle` | `model.call_model`（model.py:89） | test_agent_context, test_model_budget, test_context_manager |
| `context_window.py` | `list[Block]` + budget | `Fit{text,tokens,floor,blocks,texts}` | context.py | test_model_budget, test_compose_priority |
| `context_manager.py` | `RetrievalRequest` | `RetrievalBatch`（不生成 evidence_id） | 新增：Runner 持有（graph.py:124） | test_context_manager |
| `tools.py` | tool name+args+state | tool result / 写 `state["evidence"]`、coverage gaps | graph.lead_tools（graph.py:735→754） | test_tools/test_mcp/test_timeline_tool/test_runbook* |
| `window.py` | run event / diagnostic_context | `InvestigationWindow`/`InvestigationScope` | graph 初始化、context 初始化（新增） | test_cognition_window |
| `evidence.py` | `list[EvidenceCandidate]` | `EvidenceRecord` + fact catalog + `VerificationResult` | graph.write_report/verify（新增） | test_evidence_manager |
| `verify.py` | draft report + evidence + catalog | `{passed,findings,warnings,draft}` | graph.verify_node（graph.py:862） | test_verify |
| `memory.py` | state + report | EpisodeCard / TimelineAttachment 写入 | graph.remember | test_memory_timeline |
| `recall.py` | state + repo | `prior` 块（profile/hits/attachments） | graph.recall（graph.py:958） | test_memory_timeline |
| `model.py` | context fit + messages | LLM 调用 + usage 记账 | graph 各 model 节点 | test_model_budget |
| `planner.py`/`guards.py`/`llm.py`/`chat.py`/`schemas.py` | state / 模型响应 | plan / guard 决策 / chat 结果 | graph、chat_worker | test_model_budget 等 |

### 1.3 审计发现（本批前后）

- 时间窗曾有 **4 套算法**：`graph.py` 初始 coverage、`graph._collect_timeline`、`tools._history/_timeline`、
  `integrations.mcp.clamp_window`。本批已统一为 `runtime/window.py` 的唯一 `InvestigationWindow`，
  上述 4 处改为消费 window（见 CONTRACTS C1）。
- 证据此前是**无 schema 的 dict**，`build_catalog` 靠 ident 前缀猜事实；本批新增 `EvidenceManager`
  六项校验 + 稳定 ID + 8 类提取器，并接入 write_report/verify/persist。
- 图节点集合、调用方向、持久化入口与 §7.7.9 一致：**PASS**；tools 尚未全部迁到 `ContextManager.retrieve`：**PARTIAL**。

---

## 2. §6-8 prompts 与 skills

| 文件 | 大小/角色 | 调用者 | 写入者 | 测试 |
| --- | --- | --- | --- | --- |
| `prompts/system.md` | 2.0K→(本批+六区块/trust 说明)；固定角色/安全/证据优先级/时间规则 | `context._read`（context.py:402）经 `build_blocks` | 仓库维护，代码只读 | 间接（prompt 内容断言在 test_agent_context 之外无专门测试） |
| `prompts/lead.md` | 297B→(本批+runbook 语义)；Lead 循环/工具/结束条件 | 同上（node=lead） | 同上 | test_runbook 间接 |
| `prompts/planner.md` / `replan.md` / `report.md` / `subagent.md` | 80B/68B/904B/91B；单职责 | 同上 | 同上 | test_model_budget 间接 |
| `prompts/chat.md` | **不存在** | chat 复用 report/lead 路径 | — | — |
| `skills/_semantics.md` + 4 个 `SKILL.md` | 静态 playbook（service-unavailable/intermittent-failure/latency-degradation/tls-evidence） | `tools._skill`（load_skill）、`context`（semantics/skill 块） | 仓库维护 | test_tools 间接 |

发现：
- prompt 分层与 §7.7.8 要求一致（system/lead/planner/replan/report 分工明确）→ **PASS**。
- 六区块此前只存在于规格，prompt 与代码块名仍是 `system/node_prompt/event/...`；本批 `build_prompt_bundle()`
  产出六区块 Markdown 并在 system.md/lead.md 说明 trust 级别 → **已补齐（C4/C5）**。
- skills 没有任何协议/动态数据，符合“skills 只做调查提示” → **PASS**。

---

## 3. §6-9 协议：proto / MCP / A2A / RAGFlow

### 3.1 Go gRPC（`proto/alertflow/agent/v1/agent.proto`，客户端 `integrations/alertflow.py`）

- 服务与真实方法名（proto:10-25）：
  `AlertFlowControlService.GetRuntimeConfig/ListIncidents/GetIncident/GetMetrics/ListChecks/GetTimeline/
  GetSourceCoverage/GetKubernetesState/SubmitAgentCallback`；
  `AgentTaskService.StartInvestigation/StartChat`。
- Agent 侧实际调用：`alertflow.py:49 GetRuntimeConfig`、`:61 incidents`、`:66 incident`、`:78 metrics`、
  `:102 checks`、`:112 timeline`、`:139 source_coverage`、`:152 kubernetes_state`、`submit_callback`。
- 字段映射（三时间戳）：`_incident` 解析 `startedAt/resolvedAt/createdAt/updatedAt`；
  `_timeline_event` 解析 `occurredAt/observedAt/recordedAt`；`_check` 解析 `ts/status/latencyMs/...`。
- 结论：proto 方法名/参数有代码证据；**真实 gRPC 链路未在本轮执行**（无集群）→ 行为级 **UNVERIFIED**，
  接口级 **PASS**。

### 3.2 MCP tools/list（`integrations/mcp.py`）

- initialize + `tools/list` 后才 `tools/call`（mcp.py:102 `if name not in await _tool_names(session)`）。
- Prometheus 工具名：`query/range_query/metric_metadata/label_names/label_values`（mcp.py:16）；
  Loki：`loki_query`（mcp.py:17）；scoped/evidence 工具集合见 mcp.py:18-20。
- 失败语义：`MCPError.kind`（timeout/disconnect/auth/rate_limit/empty/malformed/too_large/missing_tool/
  unmapped/unscoped/budget）→ coverage gap（tools.py `_MCP_NOTES`）。
- 测试：`tests/test_mcp.py`（21）、`tests/test_mcp_http.py`（3，真实 HTTP MCP server）。→ **PASS**（mock/HTTP 层）；
  真实 tjhop/grafana 镜像版本行为 **UNVERIFIED**（需集群）。

### 3.3 A2A（`a2a_adapter/server.py`、`integrations/runbook.py`）

- Agent Card：`name=AlertFlow Runbook Specialist`、`version=0.1.0`、`default_input/output_modes=
  ["application/json"]`、skill `runbook_research`、HTTP+JSON、`streaming=false`。
- 请求：`Message{message_id, role=ROLE_USER, parts=[Part(text=<RunbookQueryV1 JSON>, media_type="application/json")]}`
  → `SendMessageRequest`；客户端轮询 `GetTaskRequest` 到 `COMPLETED/FAILED/CANCELED`。
- 响应：`Task.artifacts[-1].parts[0].text` 为 ArtifactV1 JSON；`artifact_id/name="runbook-research.v1"`（保留兼容别名）。
- 任务状态/恢复（§7.1）：working 先持久化、SendMessage 不同步等检索、重复 message_id 三分支、
  GetTask 收敛超时、重启不悬空、FAILED/CANCELED 不产生成功证据、cancel 幂等——均有测试：
  `tests/test_runbook.py`（16，含 2 个 Postgres 用例在无 `ALERTFLOW_TEST_DATABASE_URL` 时 SKIPPED）。
- 鉴权：**此前不可配置**；本批新增 `A2A_AUTH_TOKEN`（服务端 Bearer，Card 公开）与
  `A2A_ADAPTER_TOKEN`（客户端 Bearer）→ 已补。
- 测试：A2A HTTP 端到端（uvicorn + 真 A2A SDK）**PASS**；Postgres ledger 真持久化 **UNVERIFIED**（SKIPPED）。

### 3.4 RAGFlow v0.27.3（`a2a_adapter/ragflow.py`、fixtures）

- 请求：`POST {base_url}/api/v1/agents/chat/completions`，body 精确
  `{"agent_id", "query", "stream": false}`（fixtures/completion-request.json 断言无 `messages`）。
- 响应：`data.data.content` → summary；`data.data.reference.chunks[]` 的
  `document_id|doc_id` + `chunk_id|id` → 双 ID citation；**此前输出键为 `id`，与规范 chunk_id 不一致**，本批修正。
- 配置缺失：`base_url/api_key/agent_id` 任一为空 → POST 之前 `RuntimeError("ragflow is not configured")`（有测试）。
- 映射链路：本批新增 `query_from_runbook_request()`：RunbookQueryV1 → 可读 prompt（服务/症状/窗口/已核实事实/问题），
  不再把 JSON 原样塞给 RAGFlow；Mock 与真实适配器共用同一条路径。
- 真机：未配置真实 RAGFlow → **SKIPPED**（fixture 仅证明 v0.27.3 字段级契约，不证明真实部署）。

---

## 4. §6-10 时间语义：occurred_at / started_at / resolved_at

| 语义 | 代码证据 | 测试证据 | 结论 |
| --- | --- | --- | --- |
| `incident_start.occurred_at` = 事件发生时间，用作窗口上界 | `runtime/window.py:56-122`；graph 初始化用事件 occurred_at | `tests/test_cognition_window.py::test_incident_start_window` | **PASS**（Agent 侧） |
| `incident_start.started_at` 为权威开始时间，下界收缩用 | `window.py` 仅当 `started_at ≤ end` 时用；Go 侧 `backend/internal/db/apply_test.go:13 TestApplyCheckEventTransitions` 断言 `StartedAt=1000` | Agent 侧无“已验证 started_at”独立校验；依赖 Go 事件字段 | **PARTIAL**：机制存在，但“已验证”无 Agent 侧测试 |
| `incident_resolve.resolved_at` 仅有值时才作上界 | `window.py:77-90`（回退 occurred_at + `RESOLVE_TIME_MISSING`） | `test_cognition_window.py::test_missing_resolved_at_falls_back`；Go 测试断言 `OccurredAt=1100/ResolvedAt=1100` | **PASS** |
| `resolved_at` 缺失/不可信时不得推断 | 同上 | 同上 | **PASS** |
| 统一窗口被所有 adapter 消费（不得各自推导） | window.py + graph/tools 4 处改造 | `test_cognition_window.py` 四来源联合请求拒绝越窗；`test_context_manager.py` Prometheus 时间等于 scope 窗口 | **PASS（本批）** |
| 事后观察窗口不得当根因证据 | `window_kind=post_event_observation` 契约；tools 扩窗写 `historical_recall` | `test_cognition_window.py::test_post_event_window_kind` | **PASS（契约）**，图内尚无自动“事后观察”节点 → 使用面 PARTIAL |

**UNVERIFIED**：生产库中 `occurred_at` 的真实取值分布与时钟偏差；`resolved_at` 在真实事件中的填充率。

---

## 5. 记忆表：读写路径与数据量

| 表 | 写入路径 | 读取路径 | 数据量 |
| --- | --- | --- | --- |
| `episodes` | `MemoryRepo.save_episode`（INSERT…ON CONFLICT … version+1；embedding 单独 UPDATE），调用者 `runtime/memory.py::remember` | `get_episode`/`list_recent`（service+days+limit 50）/`search`（pgvector cosine, embedding IS NOT NULL）/`count_episodes` | **UNVERIFIED**（无生产库凭据；代码无上限约束） |
| `service_profile` | `upsert_profile`（merge_profile 后） | `get_profile`（recall 读取） | 按 service 1 行，**可重建**（PASS by design） |
| `episode_corrections` | `record_correction`（仅外部/测试调用） | 无读取代码 | **UNVERIFIED**；当前无自动纠正写入 |
| `timeline_attachments`（新增） | `MemoryRepo.save_attachment`（ON CONFLICT DO NOTHING），调用者 remember | `list_attachments`/`timeline_nodes`（旧 Episode 投影） | 新表，随 run 增长；唯一约束保证幂等 |

- 旧表**全部保留且读取路径未改**；新增 `timeline_attachments` 与新 Timeline schema；`episodes` 通过
  `memory_store.episode_to_node()` 投影为 TimelineNode，实现新旧并存（测试 `test_memory_timeline.py`）。
- `episode_corrections` 读路径缺失、写入无调用者 → 规格要求“保留 + 兼容读取”：保留 **PASS**，兼容读取 **PARTIAL**（无消费方）。
- **新链路已通 / 历史未迁移**：`timeline_attachments` 的 DDL 为幂等写法
  （`CREATE TABLE IF NOT EXISTS` + `CREATE INDEX IF NOT EXISTS`，见 `agent/schema.sql:168-190`）；
  旧 `episodes`/`service_profile`/`episode_corrections` 无 ALTER、无 DROP、无回填/迁移脚本，
  历史数据按原样保留并通过 `episode_to_node()` 只读投影；不得表述为“记忆系统已完成”。

---

## 6. P0 差距清单与本批处理

| # | P0 差距（审计前） | 本批 | 遗留 |
| --- | --- | --- | --- |
| 1 | 4 套时间窗算法 | 统一为 `runtime/window.py` 单一 `InvestigationWindow`，4 处消费 | 图内事后观察节点、`window_expansions` 未入 report |
| 2 | 无跨源 schema | `schemas/context.py`（scope/window/request/result/item/batch/六区块） | — |
| 3 | 证据无校验、ID 可被模型影响 | `EvidenceManager.register_batch` 六项校验 + 父级稳定 ID + 幂等 | tools 仍写旧 evidence dict，由 `candidates_from_evidence` 投影 |
| 4 | `verify_report` 无确定性校验字段 | `VerificationResult` + findings 映射 + runbook 双 ID 校验；报告级 `fact_ids`/`supporting_fact_ids`/`weakening_fact_ids`/`conclusion_basis_hypothesis_id` 已进 verify，未知 fact → `invalid_fact_refs`（`tests/test_report_facts.py`） | 旧报告无 fact 字段仍兼容；`confidence` 仍由 verifier 置空 |
| 5 | 六区块未落地 | `build_prompt_bundle` + 区块渲染 + system/lead prompt 更新 | chat 节点复用未接 |
| 6 | Memory 仅 Episode | `timeline_attachments` 表 + TimelineNode/Attachment schema + remember/recall 接线 | Go 原始行实时投影、纠正读取 |
| 7 | Runbook 无版本化请求/响应 | `RunbookQueryV1`/`ArtifactV1` + 双 ID + Mock A2A E2E + 鉴权配置化 | artifact 状态以 Task 终态为准的断言仅走 mock；真机 SKIPPED |
| 8 | RAGFlow citation 键不一致（`id` vs `chunk_id`） | `parse_completion` 统一 `document_id+chunk_id`，旧别名兼容 | — |
| 9 | 预算/checkpoint/失败隔离细节 | 预算记账收敛到 `runtime/budget.py`（reserve/commit/refund/resume）；attempt 含失败/超时；resume 悬空 attempt 标 `indeterminate` 不重复记账（`tests/test_budget_ledger.py`） | checkpoint 真实恢复链路无集成测试；provider 真实 usage 与估算口径以既有单测为准 |
| 10 | Postgres ledger/A2A 真持久化 | 既有代码+2 个跳过测试 | 需 `ALERTFLOW_TEST_DATABASE_URL` 才能验证 |

---

## 7. 回归证据（命令与结果）

```bash
cd agent && export PATH="$HOME/.local/bin:$PATH"
uv sync --frozen --extra dev        # exit 0
uv run --frozen --extra dev pytest -q
# 202 passed, 3 skipped, 1 warning in 6.54s   (exit 0)
```

- 基线（改动前）：126 passed / 3 skipped（`uv run` 需 `--extra dev`，默认 frozen sync 不含 pytest）。
- 新增测试：`test_cognition_window.py`(16)、`test_evidence_manager.py`(14)、`test_memory_timeline.py`(6)、
  `test_context_manager.py`(9)、`test_runbook_contract.py`(20)、`test_unified_retrieval.py`(3)、
  `test_report_facts.py`(4)、`test_budget_ledger.py`(4)。
- 3 skipped 为 Postgres ledger 两例 + 一处依赖 `ALERTFLOW_TEST_DATABASE_URL` 的用例（既有，未改）。
- 真机/外部依赖：RAGFlow 真调用、Go gRPC 集群链路、Prometheus/Loki 真 MCP 均 **SKIPPED/UNVERIFIED**，
  Mock/fixture 结果不得标记为真实集成通过（§7.7.14-14）。
