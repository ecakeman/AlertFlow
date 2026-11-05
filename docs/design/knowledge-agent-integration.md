# Knowledge Agent 接入

Knowledge Agent 的代码在 `https://github.com/ecakeman/rag.git` 的 `long` 分支。AlertFlow 不复制 `rag/ai`。

调用链：

```text
Main Agent unary InvokeAgent
  → Java 按 invocation_id 幂等
  → A2A message/send
  → Knowledge Agent 检索图
  → knowledge-retrieval-result-v1
```

结果只有原文片段和来源字段。没有摘要、结论或进度事件。

检索图保留 rewrite、router、decompose、retrieve、rerank、grade、sufficiency。没有模型密钥时，router 和 grade 不访问 LLM，retrieve 和 rerank 仍然执行。synthesize 和 verify 不在这条图上。

旧 `document` / `doc_chunk` 数据不迁移。`knowledge` 数据库和 `knowledge_app` 角色是空的，由 Knowledge Agent 自己建表。

同一 caller 和 `invocationId` 重复请求返回已有结果。内容不同返回冲突。过期租约以及 `FAILED`、`TIMED_OUT`、`UNKNOWN` 可以重新领取。完成写入必须带上本次 `fencing_token`。

请求 DataPart 固定为 `schemaVersion`、`invocationId`、`skill`、`knowledgeSpaceIds`、`query`、`context`、`constraints.topK`、`constraints.requireCitations`。`query` 包含服务名、故障摘要和已知事实，`context` 保留同样的结构。`knowledgeSpaceIds` 为空时返回空 `results`，不扫描全库。检索只使用 `document.current_version_id` 指向的、状态为 `READY` 的版本。来源字段来自数据库；文档、版本、片段或标题缺失的命中不会补默认值。

`KNOWLEDGE_A2A_TOKEN` 未设置时，Knowledge Agent 拒绝 `message/send`。Agent Card 的 GET 不要求这个 token。`x-caller-agent` 只标识调用方。

这些空间过滤、版本切换、占位恢复和内存限制都没有在本轮运行。不能把它们看成已经通过测试。`retrieve_documents` 不带知识空间时也会返回空结果，A4 评测不再做全库检索。直接调用 `run_ingest` 时必须传入 `document_version_id`。
