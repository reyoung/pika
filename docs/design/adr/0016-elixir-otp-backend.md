---
status: accepted
---

# Pika 后端使用 Elixir/OTP

## Amendment（ADR-0028）

Elixir/OTP 决策继续有效；内部协议边界由 `ACPClient` 修订为 `Pika.AgentBackend`，Codex 直接使用 App Server，只有 Cursor 使用 ACP。

Pika 后端使用 Elixir/OTP，把长期运行的 Campaign、Backend Session、归并队列和 Sync 映射为受监督进程，以利用进程隔离、消息传递和监督树处理多 Agent 并发与局部失败。Agent 协议位于 `Pika.AgentBackend` Behaviour 后；Codex 直接实现 App Server stdio JSON-RPC，Cursor 使用经过 conformance 的 Elixir ACP 库或受控 fork，而不为此改回 TypeScript 后端。

## Consequences

- HTTP 与 UI 服务优先选择 Elixir 生态，但前端渲染方案仍可独立决定。
- Pika 的 Kernel 调优领域状态机由自身实现，不引入 TypeScript `acpx` 作为第二套运行时权威状态。
- Codex App Server 与 Cursor ACP 协议细节必须集中在 Adapter/Behaviour 层，不能泄漏到 Campaign 领域逻辑。
