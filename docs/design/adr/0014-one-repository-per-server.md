---
status: accepted
---

# 一个 Pika Server 只管理一次完整优化

Pika Server 启动时绑定一个 Git 仓库的一次完整优化，也就是恰好一个 Campaign；对同一或不同仓库再做一次优化，都启动另一个 Pika Server。每个实例从源仓库当时的 HEAD 创建独立 Campaign Workspace，只暴露本次优化的 Iteration Agent 并发度，不包含多 Campaign 调度，也不让用户配置 Boundary、Plan 或主线复验等角色的并发度。

## Consequences

- Campaign Workspace、SQLite、Artifact Workspace、HTTP Token 和 Iteration Slots 形成一个服务实例的本地部署单元。
- 跨 Campaign 或跨仓库的资源竞争和统一队列不属于 Pika，由运行多个服务的外层环境负责。
- UI、数据库和恢复状态都不包含 Campaign 列表或 Active Campaign 选择器。
