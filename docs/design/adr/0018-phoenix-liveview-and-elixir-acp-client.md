---
status: accepted
---

# 使用 Phoenix LiveView 并隔离 Elixir ACP Client

Pika 使用 Phoenix、LiveView 和 PubSub 承载对话、状态、配置及实时 Agent 事件，Metrics 通过 ECharts LiveView Hook 展示，同时保留 JSON API，不维护独立 React SPA。ACP 接入位于 Pika 自有 `ACPClient` Behaviour 后面；先以现有 Elixir `agent_client_protocol` 完成 Codex/Cursor conformance spike，通过则固定版本使用，存在缺口则 fork 或补齐。Pika 不引入 Node sidecar，也不把 `acpx` 作为运行时状态层。

## Consequences

- ACP spike 必须覆盖 initialize、session/new、MCP forwarding、prompt/update、自动权限批准、cancel、close 和异常退出。
- Phoenix PubSub 只负责实时分发，SQLite 仍是持久状态权威来源。
- ECharts 是少量前端 JavaScript 的例外，不引入完整 SPA 状态管理。
