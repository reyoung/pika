---
status: accepted
---

# Pika MCP 使用角色化本地 Streamable HTTP

Phoenix 在 loopback 提供 Streamable HTTP `/mcp`，每个 ACP Agent Session 使用独立短期 Token；服务端把 Token 绑定到 Campaign、Session、Role 和可选 Attempt，SQLite 只保存哈希。Pika MCP 是 Agent 读取历史、指导和状态并提交权威结果的唯一语义接口；写工具要求 idempotency key，Agent 不能自行切换身份。不能连接 HTTP MCP 的 Agent Backend 不进入 v1，系统不提供 stdio proxy。

## Consequences

- Codex/Cursor conformance spike 必须覆盖 ACP 传入 HTTP MCP 配置及实际工具调用。
- 服务重启后不复用明文 MCP Token，新 ACP Session 获得新 Token。
- stdout、ACP 文本和自然语言完成声明不能绕过角色工具与完成门禁。
