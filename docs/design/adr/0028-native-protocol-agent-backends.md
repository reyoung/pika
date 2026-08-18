---
status: accepted
---

# Backend 使用各自原生协议，领域层依赖统一 AgentBackend

Pika 不再把 ACP 作为所有 Agent 的统一 wire protocol。领域层只依赖 `Pika.AgentBackend` 的会话、Turn、steer、interrupt、close、capabilities 和标准事件 contract；Codex Backend 为每个 Session 启动独立 `codex app-server --listen stdio://`，直接使用 thread/turn JSON-RPC、`turn/steer` 和 `turn/interrupt`，Cursor Backend 为每个 Session 启动独立 `cursor-agent acp` 并使用 ACP v1。Pika MCP 继续作为所有 Backend 提交领域状态的唯一权威接口。本 ADR supersede ADR-0008，并修订 ADR-0016、ADR-0018 与 ADR-0023 的 ACP-only 部分。

Codex 协议与 MCP 配置依据 [OpenAI App Server](https://learn.chatgpt.com/docs/app-server) 和 [OpenAI Config Reference](https://learn.chatgpt.com/docs/config-file/config-reference)。

## Consequences

- Codex 不再依赖 `@agentclientprotocol/codex-acp`、Codex SDK sidecar 或 `codex exec --json`。
- Codex 当次指导使用原生 `turn/steer`，Cursor 通过 cancel + follow-up Prompt 模拟统一 `steer`。
- Cursor 的 `session/close` 是可选 capability；未广告时 adapter 必须终止对应独立子进程并返回统一 close 结果。
- 每个 Backend Session 保持独立子进程、MCP Token 和故障域；恢复创建新 Session，不依赖 provider resume。
- 新 Agent 只有实现 `Pika.AgentBackend` conformance contract 才能进入 v0。
