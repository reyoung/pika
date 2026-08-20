---
status: accepted
---

# Ref 与 Skill 在 Campaign 初始化时取最新并固定

Pika 的内置 Reference Catalog 采用 Atrex Kernel Agent `reference-projects/` 的 16 项清单，Alignment UI 默认全选但允许用户取消或为当前 Campaign 添加 Git Repository；Skill Registry 初始加入上游 `ncu-report-skill`，不添加 Pika 硬件覆盖 Prompt。确认 Campaign Spec 时，Pika 获取所有尚未冻结的已选 Ref 默认分支最新 HEAD，保存完整 commit SHA，并在该 Campaign 生命周期内固定；恢复、普通 Sync 和后续 Attempt 都不刷新已有项目。

## Consequences

- 同一 Campaign 的并发 Attempt 看到完全相同的 Ref 和 Skill 内容。
- “最新”以每个项目在 Campaign 中首次解析的时刻为准，不代表每个 Attempt 开始时重新拉取。
- Spec Revision 可以增加新的 Campaign Reference Project；已有项目沿用原 SHA，新项目解析一次后加入该 Revision 的冻结 snapshot。
- 已有 Ref/Skill 的版本更新需要启动新的完整优化过程，不能通过普通 Sync 改变既有 Campaign 的 Agent Context。
