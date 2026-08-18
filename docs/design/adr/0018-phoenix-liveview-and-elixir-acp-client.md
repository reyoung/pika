---
status: accepted
---

# 使用 Phoenix LiveView 并隔离 Agent Backend

## Amendment（ADR-0028）

Phoenix、LiveView、PubSub、ECharts 和“不引入 Node sidecar”继续有效；`ACPClient` 统一传输层由 `Pika.AgentBackend` 取代，Codex 使用 App Server，Cursor 使用 ACP。

Pika 使用 Phoenix、LiveView 和 PubSub 承载对话、状态、配置及标准化 Backend Event，Metrics 通过 ECharts LiveView Hook 展示，同时保留 JSON API，不维护独立 React SPA。Agent 接入位于 Pika 自有 `Pika.AgentBackend` Behaviour 后；Codex 使用 App Server adapter，Cursor 使用 ACP adapter。Pika 不引入 Node sidecar，也不把 `acpx` 作为运行时状态层。

## Consequences

- Backend spike 必须分别覆盖 Codex thread/turn/steer/interrupt 与 Cursor ACP session/prompt/cancel/capability-aware close，并覆盖统一 MCP、标准事件和异常退出。
- Phoenix PubSub 只负责实时分发，SQLite 仍是持久状态权威来源。
- ECharts 是少量前端 JavaScript 的例外，不引入完整 SPA 状态管理。
