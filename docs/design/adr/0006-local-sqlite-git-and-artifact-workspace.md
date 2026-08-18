---
status: accepted
---

# 本地状态分为 SQLite、Git 和 Artifact Workspace

Pika 使用 SQLite WAL 保存编排状态与最新结构化 Metrics，使用 Git 保存代码和提交历史，并把 Patch、Prompt、Agent 输出、日志及 Profiler 原始文件保存在本地 Artifact Workspace；SQLite 只用相对路径和元数据引用文件型产物。这避免大型 Profiler 数据膨胀数据库，同时使单机部署、备份和恢复保持简单。未来可把 Artifact Workspace 扩展到 S3，但对象存储明确不属于本次开发范围。

## Consequences

- 恢复时必须联合核对 SQLite 状态、Git 历史和 Artifact 文件，不能只信任其中一个来源。
- Workspace 的移动和备份需要保持目录结构或执行显式迁移。
- 当前实现不设计 S3 客户端、上传队列、远端生命周期或缓存一致性。
