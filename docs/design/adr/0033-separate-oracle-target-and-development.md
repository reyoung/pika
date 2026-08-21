---
status: accepted
---

# 分离 Correctness Oracle、Optimization Target 与 Development

Campaign Spec v2 将实现明确拆成三种不可互换的角色：Correctness Oracle 判定语义正确性，Optimization Target 是固定性能锚点，Development Implementation 是持续优化并形成 Best 的产品代码。Alignment 必须同时实现并审阅三者所需的源码，在同一 Case 上证明 Target 与初始 Development 都通过 Oracle 并给出配对性能；空仓库也不能把三者含混为一个 `reference_path`。

Optimization Target 可以来自已审阅 Development 提交的快照，也可以来自固定 SHA 的 Reference Project。Pika 将其固化在 Campaign Workspace 的 `targets/<revision>/repo`，只用 Git 忽略的 `target/` 软链接暴露给 setup、Attempt、Integration 与 Sync worktree；Target 不进入 setup squash、候选 Patch 或最终交付。只要 Target 的来源类型、Reference Project ID 与入口定义未显式改变，后续 Spec Revision 和 Best Advanced 都继续引用同一 Target Snapshot。

正式 JSONL 始终记录 `target`/`candidate`，并同时持久化 `target_relative_improvement` 与 `best_relative_improvement`：前者判断是否超过固定优化目标，后者执行相对当前 Best 的无回归门禁。大量样本保留在本地 Artifact 文件中，MCP 只传 Manifest 和身份。

Schema v1 的 `Reference` 无法安全推断属于哪一种角色，因此恢复时必须创建显式 v2 草稿并重新确认，不能自动迁移其语义。本 ADR supersede ADR-0012 中“正式测量交错运行当前最佳版本与候选版本”的比较对象，但保留按 Case/Metric 的阈值、MAD 与 Pareto/no-regression 原则。

## Consequences

- Baseline 是初始 Development 相对 Target 的测量，不再是一份兼任 Oracle、Target 和待优化代码的“基线实现”。
- Target 与 Best 拥有不同且持久的 SHA/ID；UI、MCP、SQLite 和恢复检查必须同时保留二者身份。
- Reference Project 只是 Target 源码的一种来源或供 Agent 阅读的材料，不自动获得 Oracle 或 Target 语义。
- 明确更换 Target 会使旧 Implementation Review Evidence 失效，并要求新的 Spec Revision、源码审阅、smoke run 与 Baseline。
