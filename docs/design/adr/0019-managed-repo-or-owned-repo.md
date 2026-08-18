---
status: accepted
---

# Workspace 支持 Managed Repo 与 Pika 自建 Repo

用户显式提供本地仓库时，Pika 把它视为当前 Server 独占管理的 Managed Repo，并在 Campaign Workspace 中用 `repo/` 软链接指向它；Pika 自己获取仓库或从零开始时，`repo/` 是 Workspace 内的实际目录。两种模式共享相同的 `pika/best`、Attempt worktree、SQLite 和 Artifact 语义，但 Managed Repo 需要额外的跨进程所有权锁、软链接校验和外部修改检测。

## Consequences

- 同一个本地仓库不能同时交给两个 Pika Server 管理。
- Managed Repo 首次启动必须 clean，并通过位于 Git common directory、由进程持续持有的 OS advisory lock 建立独占权；锁文件中的 PID 只用于诊断，不决定所有权。
- Workspace 备份或移动时必须校验 Managed Repo 软链接仍指向原来的 canonical path。
- Pika 自建 Repo 可以随 Workspace 整体移动，Managed Repo 不能仅靠复制 Workspace 完成迁移。
