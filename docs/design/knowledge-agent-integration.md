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

同一 caller 和 `invocationId` 重复请求返回已有结果。内容不同返回冲突。
