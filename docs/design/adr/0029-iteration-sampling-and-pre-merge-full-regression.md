---
status: accepted
---

# 动态迭代采样与强制归并前全量回归

Campaign Spec 确认全量 Case 集和正式 Pair 数（默认 30），初始 Baseline 对全部 Case/Metric 执行该正式测量协议。Baseline Agent 随后自动选择最多十个 Case 形成首个迭代采样集；日常 Attempt 只需对其启动时的 Sampling Revision 做正式测量。

Integration 串行取得 Lease 并刷新到最新 Best 后，必须在任何 Git mutation 前对全量 Case 集做正确性检查和 5 Pair 配对筛查。筛查回退超过当前 Best noise tolerance 或样本无效的组合按 Campaign Spec 的正式 Pair 数独立重跑；任一 Case/Metric 确认回退都会拒绝候选，包括 Iteration 阶段的 Informational 项。只有获得 Full Regression Receipt 的候选才允许 squash merge。

被拒绝后，Integration Agent 从确认回退且尚未采样的 Cases 中选择形状族、回退幅度和线上权重有代表性的成员；Pika 校验来源后自动生成新的 Sampling Revision。Sampling Revision 在同一 Spec Revision 内只增不减，回退反馈可以让集合超过初始十个上限。活动 Agent 收到 Sampling Advanced，但已运行 Attempt 继续使用启动快照。

这个设计以串行归并延迟换取更快的并行 Iteration 和不污染 Campaign Best Branch 的确定性。它取代合入后异步主线复验、Metric overwrite、Revert Required 与自动 Revert 的正常流程。

## Consequences

- Campaign Best Branch 只包含已完成全量验证的候选。
- 首次 Baseline 成本较高，但为全部 Case 提供可信值和噪声估计。
- 日常 Attempt 的正式测量成本由迭代采样集控制。
- 5 Pair 是筛查证据，不覆盖既有 noise tolerance；升级后的 Campaign Spec 正式测量才能确认回退。
- 被历史回退命中的 Case 会持续留在当前 Spec Revision 的迭代采样集中。
