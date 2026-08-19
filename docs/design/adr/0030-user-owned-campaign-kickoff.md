# Campaign Kick-off 只由用户发起

Pika 将角色、权限、工作区和 MCP 完成门禁作为 Agent 系统指令注入 Backend Session，但不会把这些指令作为首条用户 Prompt 发送。Campaign 的首轮工作只由用户首条消息启动；用户确认 Campaign Spec 的显式动作可以继续驱动 setup merge 和 Baseline。Provider 若没有原生系统指令接口，adapter 必须使用其系统级规则机制或拒绝启动，不能退化为伪造用户消息。

这保留了用户意图的原始归属，也避免 Backend Session 的内部边界改变 Conversation 中“谁发起了工作”的语义；进程恢复只允许重放已有的用户 Kick-off，不产生新的 Kick-off。
