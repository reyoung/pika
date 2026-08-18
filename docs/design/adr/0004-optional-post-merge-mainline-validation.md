---
status: accepted
---

# Iteration 结果默认直接生效，主线复验异步可选

Iteration 开发 Agent 可以自由执行 Benchmark，其正确性与 Metrics 结果默认用于决定候选是否归并。用户可以选择在 Merge 后启动单并发主线 Agent 异步复验，但该能力默认关闭，而且不会阻塞后续归并；当复验 Metric 与 Iteration 报告相差超过 1% 时，系统直接以最新结果替换结构化 Metric。复验失败时，主线 Agent 必须在当前 Campaign Best Branch 上新增 revert commit，不能通过 rebase 或 reset 改写历史。这个选择优先保证默认调优吞吐量和 Agent 自主性，接受了并发 Benchmark 测量噪声、异步撤销冲突以及默认没有第二执行者复核的风险。

## Consequences

- Pika 不提供 Benchmark Lease，也不限制 Iteration 开发 Agent 的 GPU 测量并发。
- 主线复验是合入后的校验，不是默认的合入前门禁。
- 主线复验与归并队列可以同时推进，但主线复验本身最多一个并发。
- 结构化数据库只保留最新 Metric，不并列保存 Iteration 与主线两套观测。
- 复验失败通过可审计的 revert commit 撤销；后续 Agent 必须获知并处理该撤销。
