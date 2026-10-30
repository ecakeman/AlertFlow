# COGNITION_CONTRACTS — C1–C5 目标契约（v2 认知层）

本文件定义 AlertFlow Python Agent 认知层的目标契约，是 `docs/v2/COGNITION_BASELINE.md` 审计结论的落地依据。
实现均在 `agent/src/alertflow_agent/`，不新增独立部署、HTTP/gRPC/队列；三个 Manager 是**进程内逻辑组件**，
唯一对外入口仍是现有 Lead Agent（`runtime/graph.py`）。

状态标记：**已落地**＝代码+测试存在；**部分**＝契约存在但旧路径仍在使用；**待办**＝仅契约。

---

## 0. 总边界（C1 前置约束）

- 单 Lead Agent + 三个进程内 Manager：`ContextManager`、`EvidenceManager`、`MemoryManager`。
  不引入三 Specialist、不拆领域 Agent、不新增进程。
- 时间窗唯一来源：`runtime/window.py::build_investigation_window()` 由父级（graph）计算一次，
  其余模块通过 `window_from_state()` / `window_from_context()` 消费，**任何 adapter 不得自行推导窗口**。
- 唯一检索入口：`ContextManager.retrieve(RetrievalRequest) -> RetrievalBatch`（异步）。
  旧工具路径（tools.py 直调 `AlertFlowClient`/MCP）作为过渡期 thin adapter 保留，后续批次迁到 retrieve。
- 唯一证据登记入口：`EvidenceManager.register_batch()`；evidence_id 由父级生成，**不接受模型自报**。
- 六区块 Prompt Context 由 `context.build_prompt_bundle()` 产出；裁剪复用 `context_window.assemble()`。
- Memory 以 TimelineNode + TimelineAttachment 为长期主轴；旧 Episode 表只增加兼容投影，**不迁移、不删表**。

---

## C1 — 跨源 schema 与统一时间窗（已落地）

文件：`schemas/context.py`、`schemas/evidence.py`、`schemas/memory.py`、`runtime/window.py`。

### C1.1 InvestigationWindow

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| window_start | datetime (aware, UTC) | 必须 ≤ window_end；naive datetime 直接拒绝 |
| window_end | datetime (aware, UTC) | 不得晚于当前时间（超出则收敛并记 `WINDOW_END_IN_FUTURE`） |
| window_kind | Literal | `root_cause` / `post_event_observation` / `historical_recall` |
| anchor_field | str | 锚点字段名（`occurred_at` / `resolved_at` / `context.timeWindow`） |
| lookback_seconds | int ≥ 0 | = min(CHECK_LOOKBACK_MINUTES×60, MCP_MAX_WINDOW_SECONDS) |
| time_accuracy | Literal | `exact` / `derived` / `reconciled_after_gap` / `unknown` |
| gap_codes | list[str] | `RESOLVE_TIME_MISSING`、`OCCURRED_AT_MISSING`、`STARTED_AFTER_WINDOW_END`、`WINDOW_END_IN_FUTURE` |

时间规则（实现于 `runtime/window.py:56`）：

1. `incident_start`：上界 = 已验证 `occurred_at`；下界 = max(已验证 `started_at`（若存在且 ≤ end）, end - lookback)。
2. `incident_resolve`：仅当 `resolved_at` 存在时用它作上界；否则回退事件时间并写 `RESOLVE_TIME_MISSING`，`time_accuracy=derived`。
3. 缺 `occurred_at`：上界 = now，`OCCURRED_AT_MISSING`，accuracy=`unknown`。
4. `window_start` 与 `window_end` 同时下发：`AlertFlowQueryRequest` / `PrometheusQueryRequest` / `LokiQueryRequest`
   的 start/end 必须等于 scope.window，`RetrievalRequest` 构造期校验不一致直接抛
   `WINDOW_MISMATCH` / `SERVICE_SCOPE_MISMATCH` / `RUN_SCOPE_MISMATCH`。
5. 事后观察与历史召回使用独立 `window_kind`，不得冒充根因窗口（`runtime/tools.py::_timeline` 扩窗写
   `state["window_expansions"]`，kind=`historical_recall`）。

### C1.2 InvestigationScope

`schema_version="1"`、`run_id`、`service_id`、`incident_id?`、`trigger_type`（沿用 `incident_start` /
`incident_resolve` / 手动）、`occurred_at`、`started_at?`、`resolved_at?`、`observed_at?`、`recorded_at?`、
`time_accuracy`、`window: InvestigationWindow`。`started_at` 不得用 Agent run 创建时间替代。

### C1.3 RetrievalRequest / RetrievalResult

- `RetrievalRequest`：`request_id`、`scope`、`purpose`（`initial_context`/`targeted_followup`/`chat_explanation`/
  `historical_recall`）、`source_requests`（≥1，联合类型 `AlertFlowQueryRequest | PrometheusQueryRequest |
  LokiQueryRequest | TimelineRecallRequest`，禁止无约束 dict）、`result_limit`、`response_byte_limit`、
  `deadline_at?`、`budget_reservation_id?`、`dedupe_key?`。
- `RetrievalResult`：`request_id`、`run_id`、`service_id`、`source_type`、`source_system`、`status`
  （`ok`/`empty`/`disabled`/`unavailable`/`timeout`/`invalid_request`/`failed`/`truncated`）、
  `query_metadata`（不含密钥）、`items`、`collected_at`、`observed_range?`、`truncation`、
  `coverage_gaps`、`error?`。
- `RetrievedItem`：`source_record_id?`、`record_type`、`service_id`、`occurred_at?`、`observed_at?`、
  `recorded_at?`、`summary`、`data`（白名单归一化）、`source_locator`、`content_digest`。
- `CoverageGap.code`：`SOURCE_DISABLED`/`SOURCE_UNAVAILABLE`/`QUERY_TIMEOUT`/`NO_MATCHING_DATA`/
  `SELECTOR_MISSING`/`SERVICE_SCOPE_MISMATCH`/`WINDOW_GAP`/`RESULT_TRUNCATED`/`PERSISTENCE_FAILED`/
  `INVALID_RESPONSE`。gap 不取代 status。
- `RetrievalBatch`：`batch_id`、`run_id`、`scope`、`results`（按 source_type 固定排序）、`started_at`、
  `completed_at`、`budget_usage`、`overall_coverage`（`complete`/`partial`/`gap`）。

---

## C2 — EvidenceManager（已落地）

文件：`runtime/evidence.py`、`schemas/evidence.py`。

### C2.1 API

```python
EvidenceManager(run_id, scope, *, max_rows=500, max_bytes=65536, max_facts=100)
manager.register_batch(candidates: Iterable[EvidenceCandidate]) -> EvidenceRegistrationResult
manager.catalog() -> list[dict]          # 兼容 verify.build_catalog 的 {fact_id, evidence_id, path, value}
manager.registry() -> dict[str, dict]    # {evidence_id: {source_type, source_system, ledger_key, content_digest}}
manager.verify_report(draft, evidence, tokens=None, gaps=None) -> VerificationResult
candidates_from_evidence(state, scope, window)   # 旧 evidence dict → 规范化 candidate 投影
```

### C2.2 六项校验（register_batch 逐候选执行，单条拒绝不影响其他）

1. **service**：候选 `service_id` 必须等于 scope.service_id → 否则 `SERVICE_MISMATCH`。
2. **incident**：双方都有 incident_id 时必须相等 → 否则 `INCIDENT_MISMATCH`。
3. **window**：候选 window 的 start/end/kind 必须等于 scope.window → 否则 `WINDOW_MISMATCH`。
4. **selector**：prometheus/loki 必须带 selector → 否则 `SELECTOR_MISSING`。
5. **rows**：payload 中 `rows/lines/samples/events/objects/checks` 任一列表超过 `max_rows` → `ROWS_EXCEEDED`。
6. **bytes**：payload JSON 字节数超过 `max_bytes` → `BYTES_EXCEEDED`。
   另：`payload_schema` 空或 payload 非 dict/空 → `SCHEMA_INVALID`/`PAYLOAD_EMPTY`；无 source 映射 → `INVALID_SOURCE`。

### C2.3 evidence_id 规则（`make_evidence_id`, runtime/evidence.py:94）

`ev:<sha256(run_id | source_type | source_locator JSON | window_start | window_end | window_kind | content_digest)[:16]>`：
父级生成、run 内稳定、重复登记进 `duplicate_evidence_ids`，payload 变化得到新 id 而不覆盖旧记录。
`EvidenceCandidate` 使用 `extra="forbid"`，候选自带 `evidence_id` 会在构造期被拒绝（模型不能自报 ID）。

### C2.4 Fact Catalog

`FactCatalogEntry`：`fact_id`、`evidence_id`、`path`、`value`（**仅标量**）、`value_type`、`unit?`、`fact_kind`、
`occurred_at?`、`observed_at?`、`recorded_at?`、`service_id`、`trust_level`。
来源映射：`SOURCE_MAP`（旧 source_type → 规范 source_type/source_system）；8 个提取器：
checks / diagnostic / timeline / kubernetes / go_metric / prometheus / loki / runbook。
`timeline` 是合法来源：`tl:coverage` 这类“时间线来源覆盖状态”事件必须登记为 `timeline`，
不得作为未知来源拒绝。
上限 `max_facts=100`；**截断按来源配额**（`_select_with_quota`）：行数超限时先给每个出现来源
`max(1, max_facts // 来源数)` 保底名额，剩余名额按 `_FACT_PRIORITY` 顺序补足，再按
`(来源优先级, evidence_id, path)` 排序，保证同一输入稳定且没有来源被“先到先得”整类挤掉。
截断时 `catalog_truncated=True`、`catalog_truncated_by_source` 给出每来源丢弃数、
`facts_kept_by_source_type` 给出每来源保留数，并追加 `RESULT_TRUNCATED` gap；
`graph._register_evidence` 另外把 `CATALOG_TRUNCATED` 写进 `state["coverage"]["gaps"]`，
报告 coverage 因此能看到“本 run 事实被截断”。

### C2.5 verify_report

只做确定性校验：schema、fact_id 存在性、evidence_id 存在性、hypothesis 的 supporting/weakening 引用、
Runbook citation 双 ID（`invalid_document_citations`）。`passed=True` 表示**结构与引用合法**，
不表示因果被证明。findings 映射到 `VerificationResult` 的五个字段。

---

## C3 — Memory Timeline（已落地，新表与旧表并存）

文件：`schemas/memory.py`、`memory_store.py`、`runtime/memory.py`、`runtime/recall.py`、`schema.sql`。

### C3.1 TimelineNode

`node_id`（`source_system:source_record_id`，旧 Episode 投影为 `episode:<id>`）、`service_id`、`incident_id?`、
`node_type`（`runtime_event`/`incident_transition`/`check_transition`/`diagnostic`/`kubernetes_change`/
`agent_diagnosis`/`recovery_verification`/`correction`）、`source_system`、`source_record_id`、三时间
（occurred/observed/recorded）、`time_accuracy`、`summary`、`facts[]`、`attachments[]`、`source_locator`。

### C3.2 TimelineAttachment

`attachment_id`、`service_id`、`anchor_type`（`runtime_event`/`incident`/`time_window`）、`anchor_id`、
`attachment_type`（`diagnosis_report`/`evidence_summary`/`runbook_advice`/`recovery_result`/`human_correction`/
`metrics_summary`/`logs_summary`/`diagnostic_summary`）、`source_system`、`source_record_id`、`run_id?`、
`incident_id?`、`occurred_at?`、`recorded_at`、`summary`、`trust_level`（`validated_agent_report`/
`expert_advice`/`human_verified`/`derived_summary`）、`source_refs[]`、`content_digest`、`metadata`。
唯一键：`(anchor_type, anchor_id, attachment_type, source_system, source_record_id)` —— resume/remember 重试幂等。

DB：`agent/schema.sql` 末尾 `timeline_attachments` + 两个索引；`MemoryRepo.save_attachment/list_attachments/
timeline_nodes` 与 `InMemoryMemoryRepo` 同语义实现。**旧表 episodes / service_profile / episode_corrections 全部保留**：
- `episodes` 读取路径（get_episode/list_recent/search/get_profile）不变；Episode 通过 `episode_to_node()` 投影为 TimelineNode。
- `service_profile` 仍是派生缓存，可由 episodes 重建。
- `episode_corrections` 保留；新的人工纠正写 `human_correction` attachment，旧表不迁移。

### C3.3 remember / recall

`runtime/memory.py::remember()`：先写 episode（原行为），再写 `diagnosis_report` attachment
（anchor 优先 incident，其次 time_window+run_id；trust_level 由 `verification.passed` 决定
`validated_agent_report`/`derived_summary`；失败只 log warning，不打断调查）。
`recall.py::recall_prior()` 增 `attachments` 字段，`render_block()` 输出「近期时间轴附件」段。
embedding 只做候选召回，不参与时间排序；不复制 Go 原始行/原始日志/指标全文。

### C3.4 TimelineRecallRequest

`request_id`、`service_id`、`incident_id?`、`time_start?`、`time_end?`、`event_types[]?`、`query_text?`、`top_k`、
`include_attachments`、`include_corrected_history`；`source_type="memory_reference"`。
响应至少含 `nodes[]`、`total_candidates`、`returned_count`、`has_more`、`time_order`、`semantic_rank_used`、
`coverage_gaps[]`。

---

## C4 — ContextManager 与六区块 Prompt Context（已落地）

文件：`runtime/context_manager.py`、`runtime/context.py`、`runtime/context_window.py`、`schemas/context.py`。

### C4.1 retrieve

`ContextManager(*, alertflow, mcp, memory, settings)`：
`async retrieve(RetrievalRequest) -> RetrievalBatch`；同步工具层用 `retrieve_now()`（内部 asyncio.run）。
规则：
- 逐 source 分发，**单来源失败不取消其他来源**（结果分别置 failed/disabled/timeout/invalid_request）。
- `AlertFlowQueryRequest.resources` 仅支持 `incident`/`checks`/`diagnostics`/`service_timeline`/
  `kubernetes_state`/`aggregated_metrics`；Go gRPC 是业务事实唯一入口。
- Prometheus/Loki 使用统一窗口（与 scope.window 的 ISO 时间相等），MCP 调用失败按 `MCPError.kind` 映射 gap。
- Memory 用 `timeline_nodes + list_attachments`。
- 结果按固定 source_type 顺序写回 `RetrievalBatch.results`；**不生成 evidence_id**（登记是 C2 的职责）。
- `overall_coverage`：有 failed/unavailable/timeout/invalid_request/disabled → `gap`；
  有 empty/truncated → `partial`；否则 `complete`。

### C4.2 六区块

固定顺序：`current_task` → `service_timeline` → `current_evidence` → `historical_experience` →
`expert_knowledge` → `investigation_state`（`schemas/context.py::BLOCK_ORDER`）。

`ContextBlock`：`name`、`priority`（PIN/HIGH/MEDIUM/LOW）、`trust_level`（`observed_fact`/`validated_fact`/
`historical_experience`/`expert_advice`/`model_state`）、`required`、`items[]`、`source_refs[]`、
`max_items?`、`token_budget?`、`render_variants?`、`dropped_item_count?`、`degradation_reason?`。
`PromptContextBundle`：`run_id`、`scope`、`blocks[]`、`rendered_prompt_sections`、`estimated_input_tokens`、
`reserved_output_tokens`、`budget_limit`、`truncation_summary`、`source_refs`。

`build_context_report()` 输出按区块分组的 Markdown（`## current_task（…）` 等中文标题），
`current_task` 与 `current_evidence` 标记 required；裁剪沿用 `assemble()` 的 PIN→LOW 降级顺序，
`Fit.texts` 记录每个块最终文本，用于精确生成区块与来源。叶子块名保持旧名（`system`/`event`/`evidence:*`…），
`section_of()` 负责映射，避免破坏既有 prompt 断言。

---

## C5 — Prompt 分层与主图集成、Runbook 契约（已落地）

文件：`prompts/system.md`、`prompts/lead.md`、`runtime/tools.py`、`integrations/runbook*.py`、
`a2a_adapter/{server,ragflow}.py`、`config.py`。

### C5.1 Prompt 分层

- `system.md`：角色/安全/证据优先级/六区块说明/时间规则/Runbook 与历史附件 trust 级别。
- `lead.md`：工具循环、结束条件、`consult_runbook_agent` 的契约语义。
- `planner.md`/`replan.md`/`report.md`/`subagent.md` 保持单职责；skills 仍是调查提示，不承载协议。
- 六区块由代码装配，prompt 文件不拼动态 JSON；所有不可信来源在渲染时带 Untrusted/trust 标记。

### C5.2 RunbookQueryV1（A2A 请求）

`schema_version="1"`、`request_id`、`run_id`、`service_id`、`incident_id?`、`service_name?`、
`symptom_summary`、`question`、`known_facts[]`（`RunbookFactHint{fact_id, evidence_id, summary, value}`，
来自 Fact Catalog 前 8 条，不带原始日志）、`window_start`、`window_end`（= InvestigationWindow）、
`max_citations`、`max_output_chars`、`deadline_at?`。

### C5.3 RunbookArtifactV1（A2A 响应）

`schema_version="1"`、`status`（`completed`/`failed`/`canceled`，以 A2A Task 终态为准）、`summary`、
`recommendations[]{id,text,risk,preconditions[]}`、`citations[]{document_id,chunk_id,title,excerpt}`、
`warnings[]`、`citations_available`。

兼容规则（只有一份协议，旧字段仅别名）：
- 请求/响应的 JSON 都放在 A2A `Part.text`，`media_type="application/json"`；Card 与 Part MIME 一致。
- 旧版 `schema="runbook-research.v1"` 的 `content` 视为 `summary`；citation 的 `doc_id`/`id` 归一化为
  `document_id`/`chunk_id`；输出同时带 `schema`/`content` 旧别名，便于滚动升级。
- 缺 `document_id` 或 `chunk_id` 的 citation 丢弃并告警，`citations_available=false`，建议文本仍保留。
- `status=failed`/`canceled` 或带 `error` 的 artifact 永不登记为成功证据。
- 父级本地 `evidence_id` 与 `document_id`/`chunk_id` 不可互替；verifier 只校验引用形态，不证明专家陈述为真。

### C5.4 RAGFlow v0.27.3 映射

- completion：`POST {base_url}/api/v1/agents/chat/completions`，
  body `{"agent_id", "query": <由 RunbookQueryV1 映射的可读 prompt>, "stream": false}`。
- 三配置 `ragflow_base_url`/`ragflow_api_key`/`ragflow_agent_id` 缺一，在 **httpx 请求之前**报
  `RuntimeError("ragflow is not configured")`。
- `reference.chunks[].document_id|doc_id` + `chunk_id|id` → `CitationV1`；answer 文本 → `summary`；
  响应 `code` 非 0/None → `RuntimeError("ragflow completion failed")`。
- A2A 端点/鉴权配置化：`A2A_ADAPTER_URL`、`A2A_ADAPTER_TOKEN`（客户端 Bearer）、
  `A2A_ADAPTER_TIMEOUT_SECONDS`；服务端 `A2A_AUTH_TOKEN` 启用入站 Bearer（Agent Card 保持可发现）。
- 不可用隔离：传输/超时/失败只记 coverage gap（`runbook agent did not answer`），
  不合规 artifact 记 `runbook artifact is invalid`，均不打断当前调查。

---

## 验收映射（本批实现）

| 规格条目 | 证据 |
| --- | --- |
| 统一窗口（§7.7.3/7.7.14-4） | `tests/test_cognition_window.py`（16） |
| 六项校验 + 稳定 ID + 提取器（7.7.6/14-6,7） | `tests/test_evidence_manager.py`（14） |
| 旧 Episode 兼容 + 附件幂等（7.7.7/14-9） | `tests/test_memory_timeline.py`（6） |
| retrieve 状态/不互相取消/六区块（7.7.5/14-5,11） | `tests/test_context_manager.py`（9） |
| Runbook 契约 + Mock A2A E2E（7.7.4D/7.1/7.2/7.3） | `tests/test_runbook_contract.py`（20）+ 既有 `tests/test_runbook.py` |
| 全量回归 | `uv run --frozen --extra dev pytest -q` → 224 passed / 4 skipped |

未纳入本批（见 BASELINE 遗留）：tools/graph 全量改走 `ContextManager.retrieve`、chat 六区块复用、
预算预留记账、checkpoint 幂等标记、Go 原始行 → TimelineNode 实时投影。


## C6 批次 5 收敛（§7.7.10 / §7.7.12）

### 报告 fact 引用

- `LLMReport.fact_ids[]`：报告级事实引用；`LLMHypothesis.supporting_fact_ids[]` /
  `weakening_fact_ids[]`：假设级事实引用；`conclusion_basis_hypothesis_id`：结论依据的假设 id。
- verifier 只做确定性校验：每个 fact_id 必须在本 run 的 fact catalog 中可定位，否则
  `VerificationResult.invalid_fact_refs` 记录该 id（`passed=False`）；
  解析出的 `{evidence_id, path, value}` 写入 `ReportHypothesis.supporting_facts` /
  `weakening_facts`，模型自报的内容不直接采用。
- 向后兼容：旧报告缺省上述字段时仍按原 evidence 引用校验通过（`tests/test_report_facts.py`）。

### 预算账本（`runtime/budget.py`）

- `reserve(guard, settings, node, estimate)`：调用 provider 前记一次 attempt 与 token 估算；
  失败/超时/重试同样消耗。
- `commit`（`_reconcile`）：provider 返回真实 usage 时只补差额（`actual - estimate`），
  与估算二选一，不重复累加；无 usage 时保留估算并置 `tokens_estimated`。
- `refund`：仅当预留后请求未真正投递 provider 时退款；已投递的失败一律不退。
- `begin_call`/`end_call` 记录 `llm_inflight`；`resume_run` 在恢复时把悬空 attempt 标
  `indeterminate_attempts += 1`，不新增 attempt/token（`tests/test_budget_ledger.py`）。


## C7 — P6 真实 run 驱动的收敛（批次 5 之后）

### C7.1 Lead 必须使用已启用的指标/日志来源

- 触发条件（代码判定，不按工具调用次数）：优先看“本 run 是否有 HTTP 检查样本”——
  `state["raw_checks"]` 非空即按 HTTP 类故障处理（窗口内任何 status_code 非 0 的检查行）；
  没有检查样本时再用事件 5xx/错误文本兜底；完全没有任何 HTTP 检查数据才允许跳过。
  这样 monitor 事件形状不同也不会让兜底静默失效（`tools._http_runtime_fault`）。
- 提示词层：`prompts/lead.md` 与 `runtime/context.py` 的 `mcp_tools` 块明确“本 run 只要有
  HTTP 检查数据（任何检查样本）就要各查一次指标/日志”，`planner.md` 要求计划包含 metrics/logs 查询。
- 代码兜底：`finish_investigation` 时若某已启用来源调用数为 0，`_note_unqueried_sources`
  记 `mcp_activity[source]={"calls":0,"status":"empty"}` 并追加
  `PROMETHEUS_NOT_QUERIED`/`LOKI_NOT_QUERIED` gap（源未配置时不记）。
- 报告 coverage：`report.coverage.sources` 按来源给 `{enabled, calls, status}`，
  status ∈ `ok|empty|unavailable|truncated|disabled`；未查询的启用来源显示为 `empty`，
  不得静默省略。

### C7.2 Go 浮点数值归一化（remember 失败根因）

Go 侧下推的 JSON 数字可能被反序列化为 float（如 `statusCode 503.0`、`id 2.0`、`attempts 3.0`）。
规范：`integrations.alertflow.normalize_check` 统一用 `_int_or_none` 归一 id/latency_ms/status_code/attempts；
`analysis.stats.error_stats` 用 `_code_key`（`str(int(float(value)))`）生成状态码键；
`runtime.memory.build_card` 对状态码做 `int(float(code))` 防御解析。
这样同一检查行的 int/float 两种表示进入记忆时结果一致，且不改变既有 int 输入行为。

### C7.3 失败可观测性

`graph.remember` 捕获异常时使用 `log.exception` 打完整 traceback，`memory.failed` 事件附
`message`（异常文本前 500 字符），不再只记录异常类型名。

### C7.4 MCP 候选定位与来源状态语义

- **selector 传递**：`runtime.tools._mcp` 把实际发出的 MCP 参数里的 `query`（label_values 为
  `matches[0]`）写入 evidence `data["selector"]`；`candidates_from_evidence` 仅对
  `prometheus`/`loki` 写 `source_locator["selector"]`（非空时），旧证据回退 `payload["query"]`。
  登记层 `SELECTOR_MISSING` 因此对真实 MCP 候选可通过。
- **状态语义（不折叠）**：
  - `empty`：来源可达、查询成功但无匹配数据；`mcp_activity[source]={"calls":1,"status":"empty"}`，
    无 `error`；报告 coverage 记 `empty`，对应 `*_EMPTY` gap。
  - `unavailable`：MCP/传输失败、工具缺失、超限、上游 API 返回 error 等；记
    `mcp_activity[source]["error"]`（错误文本前 200 字），报告 coverage 带 `error` 字段，
    对应 `*_<KIND>` gap（如 `PROMETHEUS_TIMEOUT`、`PROMETHEUS_UPSTREAM`）。
  - 上游 error payload（`{"status":"error","error":...}`）一律 `MCPError("upstream")`，
    不得被解析成 empty。
- **验收**：`tests/test_mcp.py` 的 selector 登记、empty 不误标 unavailable、传输失败带 detail、
  upstream error 带文本 4 组测试。
