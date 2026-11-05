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

请求 DataPart 固定为 `schemaVersion`、`invocationId`、`skill`、`knowledgeSpaceIds`、`query`、`context`、`constraints.topK`、`constraints.requireCitations`。`query` 包含服务名和最多 4 条时间轴事实。`context.episodeSummary` 为空字符串，`knownFacts` 保留同样的事实。`topK` 缺省为 8，小于 1 或大于 32 时 Knowledge Agent 返回 400。`knowledgeSpaceIds` 为空时返回空 `results`，不扫描全库。检索只使用 `document.current_version_id` 指向的、状态为 `READY` 的版本。来源字段来自数据库；文档、版本、片段或标题缺失的命中不会补默认值。

Java 把本次调用的超时毫秒放进 `X-Knowledge-Timeout-Ms`。Knowledge Agent 的租约是该秒数加 15 秒，最短 45 秒，并用同样的截止时间限制检索。没有这个头时按 45 秒执行。请求 JSON 在计算幂等 hash 前按对象键递归排序。

`KNOWLEDGE_A2A_TOKEN` 未设置或不匹配时，Knowledge Agent 拒绝 `message/send` 和管理接口。Agent Card 的 GET 不要求这个 token。`x-caller-agent` 只标识调用方。当前 gRPC 没有 TLS，只用于本地开发。

Compose 里的 Knowledge Agent 固定到 rag `a41981c6265018e5a203e9340b3800686a31a37b`，image 和 Git context 使用同一个提交。容器内存上限是 768m，并设置 `KNOWLEDGE_AGENT_MODE=true`。升级时同时修改这两处。

`KNOWLEDGE_UPLOAD_MAX_BYTES` 默认 33554432，`KNOWLEDGE_INGEST_MAX_ATTEMPTS` 默认 5。Compose 把这两个变量传给 Knowledge Agent，未设置时使用上述默认值。上传按 1MiB 分块。摄取达到尝试上限后不再重试。BM25 刷新失败不把已经 READY 的 Job 改回 PENDING。

这些模式拆分、版本上传、任务 fencing、请求截止时间和镜像固定都没有在本轮运行。不能把它们看成已经通过测试。`retrieve_documents` 不带知识空间时返回空结果，A4 评测不再做全库检索。旧 `/ingest` 仍按 `doc_id` 写入，不接收版本号。这次没有运行测试、构建或 Compose。
