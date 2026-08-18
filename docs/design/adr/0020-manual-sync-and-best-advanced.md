---
status: accepted
---

# 远端同步必须人工触发并广播 BestAdvanced

Pika 与 Agent 默认不自动 Push。用户可以显式启动专用 Sync Agent，在 `pika/sync/<sync-id>` 临时分支 fetch 并 merge 配置的远端分支，完成正确性与 Metrics 验证后先 fast-forward push 远端、再 fast-forward 本地 `pika/best`；Sync 期间停止创建新 Attempt。Accepted Attempt、Sync 或 Revert 每次推进 Best 都产生持久化 `BestAdvanced`，不取消其他 Agent 的当前 Turn，但会在它们正式 Benchmark、完成或归并前强制刷新基础版本。

## Consequences

- 已运行 Iteration 可以继续编码，但新 Attempt 必须等待 Sync 结束；Attempt Branch 可以 rebase，`pika/best` 不得改写历史。
- Sync Intent 在 Git/远端操作前持久化；Push 失败保持本地 Best 不变，Push 后崩溃则按远端 SHA 幂等恢复。
- 远端更新改变受保护 Harness 时必须创建并由用户确认新 Spec Revision，不能继续沿用旧曲线。
- BestAdvanced 的投递至少一次，Agent 的分支刷新操作必须幂等。
