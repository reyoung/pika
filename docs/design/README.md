# Pika v1 设计文档

## 状态

v1 设计已于 2026-08-18 冻结。当前仓库没有产品实现；`prototype/` 是已确认的 UI 交互原型。

## 阅读顺序

1. [主设计](./kernel-optimization-agent.md)：产品边界与全部已确认规则。
2. [领域语言](./CONTEXT.md)：Campaign、Attempt、Best、Guidance、Sync 等规范术语。
3. [架构](./architecture.md)：Elixir/OTP 组件、监督树与关键数据流。
4. [状态机](./state-machine.md)：Campaign、Attempt、Integration、Validation、Sync 和恢复状态。
5. [SQLite schema](./database-schema.md)：表、字段、约束、索引与事务边界。
6. [Pika MCP API](./mcp-api.md)：角色化工具、身份、幂等和完成门禁。
7. [实现与验收计划](./implementation-plan.md)：阶段、测试矩阵与 v1 完成定义。
8. [Reference 与 Skill Registry](./reference-registry.md)：Atrex 16 项 Ref、UI 选择和 Campaign 版本冻结。
9. [ADR](./adr/)：关键决策及被替代决策的历史。

## UI 原型

- [原型说明](./ui-prototype.md)
- [原型源码](./prototype/)

## 非目标

- 多租户、RBAC、计费与跨 Pika Server 调度。
- Pika 自有 GPU Worker 或远程执行协议。
- ACP v2、非 ACP Agent fallback 和 Agent 会话 resume 依赖。
- S3 Artifact、自动 Push、内置 daemon 与容器编排。
- 默认 Plan 阶段、默认主线复验或默认 Plateau 停止。
