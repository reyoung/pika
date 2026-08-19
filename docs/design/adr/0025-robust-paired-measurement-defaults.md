---
status: accepted
---

# 正式性能判定使用固定配对测量与 MAD 噪声带

每个 Benchmark Case 默认 warmup 10 次并执行 30 个交替次序的 baseline/candidate Pair，以改善比例中位数作为结果，并用 `max(0.5%, 3 × 1.4826 × MAD)` 计算噪声容忍。只有非有限值、进程失败或 GPU 错误会使 Pair 无效，普通离群点由中位数和 MAD 吸收而不裁剪；有效 Pair 少于 24 个时整组重跑一次，再次不足则拒绝 Attempt。这样牺牲部分测量吞吐量，换取并发 GPU 环境下更保守、可复现的接受判断。

## Consequences

- Agent 的快速单次 Benchmark 只能指导开发，不能提交正式接受结果。
- Campaign 可以覆盖 warmup、`pair_count` 与 `min_valid_pairs`，但改变正式测量协议会产生 Spec Revision；只覆盖 Pair 数时，有效门槛默认为向上取整的 80%。
- 测量环境持续不稳定时，Attempt 会被拒绝而不是无限重复统计采样。
