# Pika v1 设计文档

## 状态

v1 设计已于 2026-08-18 冻结，并由 ADR-0028 修订 Agent Backend 协议、ADR-0029 修订 Benchmark/Integration 语义。当前仓库包含 Phase 0 与 Stage0 Preview；Attempt/Integration 仍按 Phase 文档实现。

## 阅读顺序

1. [主设计](./kernel-optimization-agent.md)：产品边界与全部已确认规则。
2. [领域语言](./CONTEXT.md)：Campaign、Attempt、Best、Guidance、Sync 等规范术语。
3. [架构](./architecture.md)：Elixir/OTP 组件、监督树与关键数据流。
4. [状态机](./state-machine.md)：Campaign、Attempt、Integration Full Regression、Sync 和恢复状态。
5. [SQLite schema](./database-schema.md)：表、字段、约束、索引与事务边界。
6. [Pika MCP API](./mcp-api.md)：角色化工具、身份、幂等和完成门禁。
7. [实现与验收计划](./implementation-plan.md)：阶段、测试矩阵与 v1 完成定义。
8. [Reference 与 Skill Registry](./reference-registry.md)：Atrex 16 项 Ref、UI 选择和 Campaign 版本冻结。
9. [逐阶段实现清单](../v0/phases/README.md)：Phase 0–6 的可执行任务、测试和 Exit Gate。
10. [ADR](./adr/)：关键决策及被替代决策的历史。

## UI 原型

- [原型说明](./ui-prototype.md)
- [原型源码](./prototype/)

## 非目标

- 多租户、RBAC、计费与跨 Pika Server 调度。
- Pika 自有 GPU Worker 或远程执行协议。
- Cursor ACP v2、未实现 `Pika.AgentBackend` contract 的 Agent，以及 provider resume 正确性依赖。
- S3 Artifact、自动 Push、内置 daemon 与容器编排。
- 默认 Plan 阶段或默认 Plateau 停止。
