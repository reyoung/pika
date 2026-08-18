---
status: accepted
---

# Accepted Attempt 只推进独立 Campaign Workspace

Pika Server 从启动时的仓库 HEAD 创建本次优化专属 Campaign Best Branch。用户显式提供本地仓库时，Campaign Workspace 通过 `repo/` 软链接把它作为 Managed Repo 独占管理；否则 `repo/` 是 Pika 自己 clone 或初始化的独立目录。并发 Attempt 分别使用独立 worktree，Accepted Attempt 由编码 Agent 串行 squash merge 到 Campaign Best Branch；启动前的原分支不被 Iteration 修改。另一次完整优化由另一个 Pika Server 和另一个 Workspace 承担，因此系统没有多 Campaign 调度问题。本 ADR 取代 ADR-0003。

## Consequences

- 最终代码交付就是 Campaign Workspace 中的 Best Branch、commit、Patch 和 Metrics，不存在服务内 Promote 阶段。
- 用户若要把结果带回其他分支，在本次 Pika 优化流程之外处理。
- Managed Repo 必须防止另一个 Pika Server 同时取得所有权；Pika 自建 repo 则天然由当前 Workspace 独占。
