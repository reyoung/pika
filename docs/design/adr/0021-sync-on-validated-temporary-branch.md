---
status: accepted
---

# Sync 在临时分支验证并以 Intent 恢复

Sync 固定一对 remote/branch，在 `pika/sync/<sync-id>` 临时分支 merge 远端更新并由 Sync Agent 解决冲突；完整正确性和 Best Metrics 通过后，先普通 fast-forward push 远端，再 fast-forward 本地 `pika/best`。所有可能改变外部状态的步骤前先持久化 Sync Intent。Push 失败不推进本地 Best；远端成功而服务崩溃时，恢复流程核对远端 SHA 后完成本地推进。若远端改变受保护 Harness，Sync 暂停在 Spec Revision 确认流程。

## Consequences

- Sync 不使用 rebase、force push 或历史改写。
- Sync 失败不会把未验证代码留在 Best Branch，但 fetch 得到的远端引用可以保留。
- 普通 Attempt 不因 Sync 开始而取消，但所有新 Attempt 等待 Sync 结束。
