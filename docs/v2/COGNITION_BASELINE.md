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
# 231 passed, 4 skipped, 1 warning in 9.12s   (exit 0)
```

- 基线（改动前）：126 passed / 3 skipped（`uv run` 需 `--extra dev`，默认 frozen sync 不含 pytest）。
- 新增测试：`test_cognition_window.py`(16)、`test_evidence_manager.py`(14)、`test_memory_timeline.py`(6)、
  `test_context_manager.py`(9)、`test_runbook_contract.py`(20)、`test_unified_retrieval.py`(3)、
  `test_report_facts.py`(4)、`test_budget_ledger.py`(4)、`test_mcp_guidance.py`(7)、
  `test_memory_remember.py`(6)、`test_catalog_quota.py`(3)；C7.4 追加 5 例 MCP selector/状态语义测试。
- 4 skipped：Postgres ledger 两例（`tests/test_runbook.py:386/429`）、`test_memory_remember.py:108`
  （均依赖 `ALERTFLOW_TEST_DATABASE_URL`）、`test_events.py:38`（本地角色无法 CREATE DATABASE，既有）。
- 真机/外部依赖：RAGFlow 真调用、Go gRPC 集群链路、Prometheus/Loki 真 MCP 均 **SKIPPED/UNVERIFIED**，
  Mock/fixture 结果不得标记为真实集成通过（§7.7.14-14）。


---

## 8. P6 真实 run 驱动的修复（2026-10-30）

真实 run：`624d23a8-033f-486b-86a4-ad05a13e67f8`（service `fault-lab`，incident 4，503 注入态）。
报告证据从集群只读取回后解析（`/tmp/p6_run.json`、`/tmp/p6_report.json`、`/tmp/p6_evidence.json`）。

### 8.1 Lead 未查询指标/日志（已修）

- 现象：`fact_count_by_source_type` = {checks 11, timeline 5, go_metric 1, diagnostic 6, kubernetes 12}，
  prometheus/loki 为 0；`tool_calls=3`，但两个 MCP endpoint 在 Pod 内 initialize 均 200。
- 根因：`prompts/lead.md` 未提指标/日志工具；`runtime/context.py` 的 mcp_tools 块只是
  “Optional evidence tools”，没有使用判据。
- 修复：lead/planner 提示词 + mcp_tools 块给出“本 run 只要持有 HTTP 检查样本（任何检查行）
  且源已配置，就必须各查一次指标/日志”的判据；`finish_investigation` 对未查询的已启用来源记
  `*_NOT_QUERIED` gap 并在 `report.coverage.sources` 标记为 `empty`；不硬编码调用次数断言。
- 判据修正（incident 6 真机 run 复测暴露）：原 `_http_runtime_fault` 只认 monitor 事件的
  5xx/错误文本，事件形状不同（非 5xx 的 monitor 告警）时兜底静默失效。现改为优先判断
  `state["raw_checks"]` 是否非空（有 HTTP 检查样本即触发），事件 5xx 仅作无检查数据时的补充
  判据；完全没有 HTTP 检查数据才允许跳过。
- 验证：`tests/test_mcp_guidance.py`（9）覆盖 503 故障必查、有检查但事件非 5xx 仍必查、
  无检查数据不记、事件 5xx 无检查数据仍必查、未配置源不记、run 报告 coverage.sources 形状。
  真实 run 复测由 lead 触发（本 Agent 未新增真实 LLM 调用）。

### 8.2 remember 失败（已修）

- 现象：agent Pod 日志 `remember failed: ValueError`；agent DB `episodes`/`timeline_attachments` 均为 0。
- 根因（用真实 state 复现）：P6 证据里的检查行来自 Go JSON，数字是 float
  （`statusCode 503.0`、`id 2.0`、`attempts 3.0`）；`error_stats` 以 `str(status_code)` 生成键 `"503.0"`，
  `memory.build_card` 再 `int("503.0")` → `ValueError: invalid literal for int() with base 10: '503.0'`。
- 修复：`normalize_check` 用 `_int_or_none` 归一数字；`error_stats` 用 `_code_key` 规范化状态码键；
  `build_card` 用 `int(float(code))` 防御解析；`graph.remember` 改 `log.exception` 并附异常文本，
  不再只记录类型名。
- 验证：
  - 真实 P6 state + `InMemoryMemoryRepo`：`saved=True`、`attachment_saved=True`（anchor incident 4，
    trust `validated_agent_report`）。
  - 本地 pgvector（`alertflow-p1-pg`，127.0.0.1:55432）新建 `alertflow_agent_repro` 并应用
    `agent/schema.sql`（含 `timeline_attachments`）：真跑 `MemoryRepo` 写入 episodes=1、
    timeline_attachments=1；重跑 version=2、attachment_saved=False（幂等）；旧 `get_profile`/
    `list_recent` 可读。
  - `tests/test_memory_remember.py`（6，其中 1 例依赖 `ALERTFLOW_TEST_DATABASE_URL`）。

### 8.3 数据质量三项（已修）

1. `tl:coverage` 曾被 `INVALID_SOURCE` 拒绝：`timeline` 是合法来源，`SOURCE_MAP` 已映射；
   用真实 P6 证据重跑登记：accepted 36 / rejected 0（修前 35/1）。
2. `catalog_truncated` 原先全局截断（后出现的来源会被整类挤掉）：改为按来源配额
   （`_select_with_quota`），`catalog_truncated_by_source`/`facts_kept_by_source_type` 进
   `evidence_registration`，并追加 `RESULT_TRUNCATED` gap；`graph._register_evidence` 另把
   `CATALOG_TRUNCATED` 写进 `coverage.gaps`，报告 coverage 可见。
   真实 P6 重跑：kept={diagnostic 37, checks 20, go_metric 3, kubernetes 20, timeline 20}，
   dropped={diagnostic 34, checks 41, kubernetes 12, timeline 35}（prometheus/loki 本 run 无数据）。
3. `report.coverage` 已补 `sources`（按来源 `{enabled,calls,status}`），见 8.1。

### 8.4 MCP 候选缺 selector 与 empty/unavailable 混淆（已修，2026-10-30 第二次真机 run）

- 现象：run `9defce67-947c-4b26-a3b4-2c950019e3c3` 中 loki 调用 status `ok`，但候选以
  `SELECTOR_MISSING` 被拒；prometheus 调用 status `unavailable` 且伴随 `PROMETHEUS_EMPTY`，
  无法区分“工具执行失败”与“空结果”。
- 根因：MCP 证据 `data` 未保存实际发出的 selector（只有 summary 文本），登记层按
  `source_locator/query_metadata` 找不到 selector；`summarize_result` 对上游 error payload
  （`{"status":"error","error":...}`）走“无 rows/value”分支，被当成 empty。
- 修复：
  - `runtime/tools.py::_mcp` 把整形后实际发出的 `query`（label_values 取 `matches[0]`）写入
    evidence `data["selector"]`；`runtime/evidence.py::candidates_from_evidence` 将其放入
    `source_locator["selector"]`，旧证据缺该字段时回退 `payload["query"]`。
  - `integrations/mcp.py`：`MCPError(kind, message)` 保留错误文本；`summarize_result` 先识别
    上游 error payload → `MCPError("upstream", 文本)`；`_text` 的 malformed/auth 也带文本。
  - `runtime/tools.py`：empty → `mcp_activity.status="empty"`（不记 error）；其它失败 →
    `"unavailable"` + `mcp_activity[source]["error"]`（错误文本前 200 字）；`graph._coverage_sources`
    仅在存在 error 时输出 `error` 字段。
- 验证：`tests/test_mcp.py::test_mcp_evidence_candidate_is_registered_with_selector`、
  `test_empty_mcp_result_is_empty_not_unavailable`、
  `test_mcp_transport_error_is_unavailable_with_detail`、
  `test_upstream_mcp_error_is_not_reported_as_empty`、
  `test_upstream_mcp_error_detail_reaches_report_coverage`（5 例）。
  真实 run 复测由 lead 触发；预期 `fact_count_by_source_type` 中 loki 出现在修好
  selector 后 >0，prometheus 仅在有真实数据时 >0（若上游仍返回错误则 coverage 记
  `unavailable` + 错误文本，而不是静默 empty）。


> 说明：Mock/fixture 结果不标记为真实集成通过；P6 真机复测（指标/日志实际被调用、
> remember 实际写库）仍以 lead 侧新 run 为准，本文件只记录代码修复与本地/真实数据复现证据。
