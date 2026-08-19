---
status: accepted
---

# 优先恢复 Provider Session，并用持久化上下文保证自动恢复

Pika 不把正确性依赖在任何 Agent 厂商的会话 resume 上，但会优先使用可用的原生恢复能力：Codex 使用 `thread/resume`，ACP Agent 在广告 `loadSession` 时使用 `session/load`。服务启动后自动检查 SQLite、Git 和 Artifact Workspace，把失去运行进程的工作标记为 `Interrupted`；原生恢复失败时创建新 Agent 会话，注入持久化上下文并继续原候选尝试。普通工作无需用户确认即可恢复，但 `Blocked`、无法判定的 Git 状态和损坏 Artifact 不自动越过。

## Consequences

- Prompt 和持久化 Artifact 必须足以让无历史对话的新 Agent 接续工作。
- Provider Session ID 必须持久化；adapter 的 resume/load 失败必须安全降级到新会话与 `get_context`。
- 自动恢复必须是幂等的，不能重复启动 Agent、重复 Full Regression 或重复 Merge。
