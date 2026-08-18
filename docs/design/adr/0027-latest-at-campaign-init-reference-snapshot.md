---
status: accepted
---

# Ref 与 Skill 在 Campaign 初始化时取最新并固定

Pika 的内置 Reference Catalog 采用 Atrex Kernel Agent `reference-projects/` 的 16 项清单，Alignment UI 默认全选但允许用户取消；Skill Registry 初始加入上游 `ncu-report-skill`，不添加 Pika 硬件覆盖 Prompt。Pika Server 首次初始化 Campaign 时获取所有选中 Ref 与 Skill 默认分支的最新 HEAD，保存完整 commit SHA，并在该 Campaign 生命周期内固定；恢复、普通 Sync 和后续 Attempt 都不刷新，新 Pika Server 才重新解析最新版本。

## Consequences

- 同一 Campaign 的并发 Attempt 看到完全相同的 Ref 和 Skill 内容。
- “最新”以 Campaign 首次初始化时刻为准，不代表每个 Attempt 开始时重新拉取。
- Ref/Skill 更新需要启动新的完整优化过程，不能通过普通 Sync 改变既有 Campaign 的 Agent Context。
