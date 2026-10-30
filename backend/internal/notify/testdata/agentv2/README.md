# Agent v2 envelope 黄金文件

这些文件是 B1 抽取（`internal/notify` → `internal/agentmessage`）的回归证据。

- 由重构前 `a5ca5d2` 的 `notify.AgentSender.Encode` 在固定时钟
  `time.Unix(1_700_000_123, 0)` 下生成，随后由 `TestAgentMessageGoldenBytes`
  逐字节比较。
- 生成/更新命令（**仅限从旧实现重新捕获基线时**）：

  ```
  ALERTFLOW_UPDATE_GOLDEN=1 go test -run TestAgentMessageGoldenBytes ./internal/notify
  ```

- 不要为了让新实现通过而重新生成这些文件。如果测试失败，说明 envelope 的
  字节输出发生了变化，而 Agent 契约要求 HTTP 与 gRPC 两条路径的 payload
  语义保持不变。
- `trimmed-oversize-context.json` 覆盖 8 步裁剪中的前几步，用于锁定裁剪顺序
  与 `coverage.truncated` 标记。
