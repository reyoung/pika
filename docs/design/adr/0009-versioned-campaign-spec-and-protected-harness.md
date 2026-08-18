---
status: accepted
---

# 显式冻结 Campaign Spec 并保护 Reference 与 Harness

调优必须从用户明确确认的版本化 Campaign Spec 开始；Reference、正确性测试和 Benchmark Harness 先由 Boundary Agent 在 setup worktree 中完成并归并到 Campaign Best Branch，随后按路径和内容哈希成为受保护 Harness。Iteration 只能读取和执行这些文件，任何候选修改都直接拒绝；优化中改变语义、Shapes、Metrics、Benchmark 或正确性要求必须创建 Spec Revision、重建 Baseline 与噪声估计，且不同 Revision 的 Metrics 不直接比较。

## Consequences

- 系统不能从仍在讨论的需求直接启动优化循环。
- 测试或 Benchmark 的“优化”不能伪装成 Kernel 性能提升。
- Spec Revision 会切断连续性能曲线，并产生新的 Baseline。
