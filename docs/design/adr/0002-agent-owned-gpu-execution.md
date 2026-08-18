---
status: accepted
---

# Pika 不建模 GPU Worker

Pika 只启动和编排外部编码 Agent，不拥有 GPU 执行节点。v1 假设 Agent 能直接使用当前机器上的 GPU；未来即使通过 global skill 指导 Agent 去其他 GPU 环境执行，远程执行仍属于 Agent 会话内部行为，而不是 Pika 的 Worker。因此 Pika 不设计 GPU Worker 注册、心跳、远程进程、代码同步或 GPU RPC 协议，避免与 Agent 已有的工具和技能体系重复。

## Consequences

- Pika 只观察 Agent 进程、结构化事件和最终产物，不能把远端 GPU 任务当作独立领域对象调度或恢复。
- GPU 环境准备、远端鉴权和远端任务清理由 Agent 及其 global skill 负责。
- Pika 仍可提供与具体 GPU Worker 无关的 Agent 并发限制、归并顺序和持久化状态机。
