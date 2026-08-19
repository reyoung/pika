---
status: accepted
---

# 按 Benchmark Case 做 Pareto 门禁并使用配对测量

每个 `(Benchmark Case, Metric)` 独立参与候选判定：至少一个目标 Case 的指标改善超过 `max(1%, noise_tolerance)`，所有保护 Case 的指标不得退化超过各自噪声容忍，观察 Case 不参与门禁。线上频率权重只用于评分、排序和 UI，不能抵消保护 Case 的退化。正式判定在 warmup 后交错运行当前最佳版本与候选版本，使用配对比值的中位数和 MAD 自动估算噪声，从而降低温度、频率和并发环境漂移造成的假提升。

## Consequences

- Agent 的自由 Benchmark 可以指导开发，但不能代替正式配对测量。
- Harness 必须能在同一环境中可重复地运行两个 Git 版本。
- 新增或改变 Benchmark Case 会产生 Spec Revision，而不是悄悄改变既有曲线。

## Amendment — ADR-0029

日常 Attempt 的 Pareto 门禁只要求其固定 Sampling Revision；归并前全量回归对 Full Case Set 使用更严格的 universal no-regression gate。Sampling Revision 只改变已有 Case 的正式测量调度，不新增或修改 Benchmark Case，因此不产生 Spec Revision。
