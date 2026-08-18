---
status: accepted
---

# ACP v1 承载会话，Pika MCP 承载领域语义

Pika v1 作为中心 ACP Client，为每个活跃 Agent 会话启动独立 ACP v1 Server 子进程；不支持 ACP 的 Agent 不进入 v1，也不提供 stdout scraping fallback。ACP 负责会话生命周期、Prompt Turn、取消、权限与流式 UI 事件，Pika MCP 则是读取和提交调优领域状态的强制接口。Agent 之间不直接连接，而由 Pika MCP 的持久化 Agent Mailbox 在同一调优任务内中心路由消息。

## Consequences

- Cursor 可使用原生 ACP Server，Codex 使用 `@agentclientprotocol/codex-acp`；其他 Agent 只需提供可配置的 ACP v1 Server 启动命令。
- 当次指导通过 `session/cancel` 结束当前 Turn，再在同一 Session 发送新 Prompt；取消超时才终止进程并进入恢复流程。
- Agent 正常结束 Prompt 或进程不等于候选完成；只有 Pika MCP 的 `complete_attempt` 能提交权威完成状态。
- ACP v2 和不支持 ACP 的 Agent 适配不属于 v1。
