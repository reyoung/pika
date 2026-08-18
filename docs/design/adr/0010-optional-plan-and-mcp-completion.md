---
status: accepted
---

# Plan 默认关闭，角色完成必须由 MCP 确认

Plan 是默认关闭的可选阶段；启用后由 Agent 调用 `submit_plan`，Pika 把 `plan.md` 原子写入 Artifact Workspace，后续 Agent 才能使用。所有角色的完成都以其必需 MCP 操作为准，而不是 Backend Turn、进程退出码或自然语言声明；如果 Turn 结束但缺少提交，Pika 保留同一 Backend Session 并持续发送 follow-up Prompt，Agent 或进程失效时则用新会话恢复。该循环没有次数或时间预算，只能由成功提交、用户取消或调优任务的其他停止条件终止。

## Consequences

- `plan.md` 位于 Artifact Workspace，并通过 MCP Resource 或 Backend 嵌入资源暴露给后续 Agent。
- 禁用 Plan 时不会产生空计划步骤或额外 Agent 成本。
- Pika 需要为每种 Agent 角色声明必需 MCP 操作，并接受失控 Agent 无限运行和持续产生成本的风险。
