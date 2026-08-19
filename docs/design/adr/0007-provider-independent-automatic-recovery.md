---
status: accepted
---

# 使用新 Agent 会话自动恢复中断工作

Pika 不依赖任何 Agent 厂商的会话 resume。服务启动后自动检查 SQLite、Git 和 Artifact Workspace，把失去运行进程的工作标记为 `Interrupted`，然后用新 Agent 会话注入持久化上下文并继续原候选尝试；普通工作无需用户确认即可恢复，但 `Blocked`、无法判定的 Git 状态和损坏 Artifact 不自动越过。这样可以统一支持 Codex、Cursor 等能力不同的 Agent，并避免把系统可靠性绑定到外部会话格式。

## Consequences

- Prompt 和持久化 Artifact 必须足以让无历史对话的新 Agent 接续工作。
- Agent adapter 可以提供 resume 作为优化，但不能成为正确性依赖。
- 自动恢复必须是幂等的，不能重复启动 Agent、重复 Full Regression 或重复 Merge。
