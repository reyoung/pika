---
status: accepted
---

# 正式性能判定使用用户指定的配对测量与 MAD 噪声带

每个 Benchmark Case 在 warmup 后执行用户在 Alignment 中明确指定数量的交替次序 baseline/candidate Pair，以改善比例中位数作为结果，并用 `max(0.5%, 3 × 1.4826 × MAD)` 计算噪声容忍。`pair_count` 与 `min_valid_pairs` 都是必填的用户决策，不存在全局默认数量。只有非有限值、进程失败或 GPU 错误会使 Pair 无效，普通离群点由中位数和 MAD 吸收而不裁剪；有效 Pair 少于用户指定门槛时整组重跑一次，再次不足则拒绝 Attempt。

## Consequences

- Agent 的快速单次 Benchmark 只能指导开发，不能提交正式接受结果。
- Campaign Spec 必须记录用户确认的 `pair_count` 与 `min_valid_pairs`；改变任一值会产生 Spec Revision。
- 测量环境持续不稳定时，Attempt 会被拒绝而不是无限重复统计采样。
