---
status: accepted
---

# Pika 后端使用 Elixir/OTP

Pika 后端使用 Elixir/OTP，把长期运行的 Campaign、ACP Agent 连接、归并队列和主线复验映射为受监督进程，以利用进程隔离、消息传递和监督树处理多 Agent 并发与局部失败。ACP Client 位于内部 Behaviour 后面；现有 Elixir ACP 库仍处于 0.x，因此实现前必须用 Codex 与 Cursor 完成 ACP v1 conformance spike，必要时补齐或 fork 依赖，而不为此改回 TypeScript 后端。

## Consequences

- HTTP 与 UI 服务优先选择 Elixir 生态，但前端渲染方案仍可独立决定。
- Pika 的 Kernel 调优领域状态机由自身实现，不引入 TypeScript `acpx` 作为第二套运行时权威状态。
- ACP 协议细节必须集中在 Adapter/Behaviour 层，不能泄漏到 Campaign 领域逻辑。
