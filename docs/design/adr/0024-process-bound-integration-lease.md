---
status: accepted
---

# 归并租约绑定 Backend Session 与实际进程

Integration Agent 必须以预期 Best SHA 原子获取唯一归并租约，租约绑定 Backend Session 和受 Supervisor 监控的 Elixir 进程，不设置固定超时。Agent 调用 `complete_merge` 后，Pika 核验 Git HEAD、父提交、Diff、受保护文件、Metrics 与 trailers，才在事务中完成状态并释放租约。进程崩溃不代表 Git 操作没有发生，因此恢复必须先核对 Operation Intent、merge/revert 状态和实际仓库，再由恢复 Agent 接管。

## Consequences

- 长时间 Merge 不会因 TTL 过期与另一个 Agent 并发推进 Best。
- 卡死的 Integration Agent 只能由用户 Stop、进程故障或恢复逻辑接管，不能靠租约超时自动绕过。
- 租约状态、Best SHA 与 Git 事实不一致时 Campaign 必须停止危险推进。
