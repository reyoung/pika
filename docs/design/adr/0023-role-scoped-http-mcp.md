---
status: accepted
---

# Pika MCP 使用角色化本地 Streamable HTTP

## Amendment（ADR-0028）

角色化 HTTP MCP、短期 Token、哈希和幂等规则继续有效。MCP 不再假设统一 ACP Session：Codex 通过 App Server 进程配置注入，Cursor 通过 ACP Session 配置注入。

Phoenix 在 loopback 提供 Streamable HTTP `/mcp`，每个 Backend Session 使用独立短期 Token；服务端把 Token 绑定到 Campaign、Session、Role 和可选 Attempt，SQLite 只保存哈希。Pika MCP 是 Agent 读取历史、指导和状态并提交权威结果的唯一语义接口；写工具要求 idempotency key，Agent 不能自行切换身份。Codex 通过 App Server 进程配置注入，Cursor 通过 ACP Session 配置注入；不能连接 HTTP MCP 的 Backend 不符合 conformance contract。

## Consequences

- Codex/Cursor conformance spike 必须覆盖各自协议的 HTTP MCP 配置及实际工具调用。
- 服务重启后不复用明文 MCP Token，新 Backend Session 获得新 Token。
- stdout、Backend 文本和自然语言完成声明不能绕过角色工具与完成门禁。
