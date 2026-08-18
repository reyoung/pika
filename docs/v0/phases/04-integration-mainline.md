# Phase 4 — Integration、BestAdvanced 与 Mainline

## 状态

Not started

## 依赖

[Phase 3](./03-attempt-loop.md) Exit Gate 全通过。

## 目标

把 ready_for_integration Attempts 串行、安全、可恢复地推进到 Campaign Best Branch；处理陈旧 Base、Pareto 门禁、BestAdvanced、可选主线复验和迟到 Revert。

## 本 Phase 交付

- FIFO Integration Queue。
- 绑定 Session/进程且无 TTL 的 Integration Lease。
- merge/revert Operation Intent 与崩溃恢复。
- Agent 执行 squash merge，Pika 独立核验 Git。
- BestAdvanced 至少一次投递和 stale-base gate。
- 可选 Mainline Validation、Metric 覆盖和 RevertRequired。
- 终态 Attempt Artifact 后自动清理 worktree/branch。

## 实施顺序

### 4.1 Integration Queue

- [ ] `ready_for_integration` 按 Attempt ordinal FIFO 排队。
- [ ] 同时最多一个 Integration Agent。
- [ ] `acquire_integration_lease(expected_best_sha)` 原子检查队首、Best 和现有 Lease。
- [ ] Lease 绑定 Agent Session、Attempt、Elixir 进程和 Intent，不设 TTL。

### 4.2 陈旧 Base Refresh

- [ ] 如果 Attempt base_sha != current best_sha，返回 `stale_best`。
- [ ] Iteration/Integration Agent 可以 rebase 临时 Attempt Branch。
- [ ] 解决冲突后重跑正确性和正式配对测量。
- [ ] 刷新过程中 Best 再次变化时重复检查，不带旧 Metrics 归并。
- [ ] `pika/best` 本身绝不 rebase。

### 4.3 Integration Agent Git 流程

- [ ] 获取 Lease 后持久化 merge Intent。
- [ ] 移除 Pika 注入的 `ref/**` 与对应 `.gitmodules` 增量。
- [ ] 保留用户原有 submodule。
- [ ] 在无 Ref/Skill 的状态下再次验证正确性和 Metrics。
- [ ] Agent squash merge 到最新 `pika/best`。
- [ ] Commit trailers 包含 `Pika-Attempt-Id`、`Pika-Spec-Revision`。
- [ ] Agent 调用 `complete_merge`。

### 4.4 Pika 独立核验

- [ ] HEAD parent 等于 Lease expected Best。
- [ ] commit/branch/working tree 状态符合契约。
- [ ] Patch 不包含 protected paths、`ref/**`、Skill 或 Pika `.gitmodules` 增量。
- [ ] Pika 重算 Pareto 门禁：至少一个目标 > `max(1%, noise)`，保护指标不超噪声。
- [ ] Accepted/Rejected、Best Revision、Metrics、BestAdvanced、Lease 释放同事务。

### 4.5 BestAdvanced

- [ ] 事件含 old/new SHA、cause、Attempt、Metric delta。
- [ ] 写入所有活跃 Agent Mailbox，至少一次投递。
- [ ] 不 cancel 当前 ACP Turn。
- [ ] `record_metrics`、`complete_attempt`、获取 Integration Lease 前强制检查 Base。
- [ ] Agent 刷新/ack 操作幂等。

### 4.6 Mainline Validation

- [ ] 配置默认关闭；启用后单并发。
- [ ] 每个任务在独立 worktree 固定测试对应 squash SHA。
- [ ] 不阻塞普通 Integration。
- [ ] Metric 与 Iteration 相差 >1% 时覆盖 Attempt 最新快照。
- [ ] 校正后仍通过门禁：保留代码；失败：产生 RevertRequired。

### 4.7 Revert

- [ ] Mainline Agent 获取同一个 Integration Lease。
- [ ] 在最新 Best 上创建 `git revert <failed-sha>` commit，不 reset/rebase。
- [ ] 解决与后续提交的冲突并验证。
- [ ] 成功推进 Best、发 BestAdvanced；失败进入 Blocked。

### 4.8 清理

- [ ] 终态前持久化 Patch、Summary、Metrics、Profiler、JSONL、commit 信息。
- [ ] Artifact 事务成功后移除 worktree 和 Attempt Branch。
- [ ] Interrupted/Blocked/Incomplete 不清理。
- [ ] `retain_attempt_worktrees=true` 时保留。

## 故障注入矩阵

在以下点 kill -9：

- [ ] Lease 获取后、Git 前。
- [ ] Intent 写入后、Git 前。
- [ ] squash commit 后、`complete_merge` 前。
- [ ] `complete_merge` 请求中、SQLite commit 前。
- [ ] SQLite commit 后、Agent 收到响应前。
- [ ] Revert 冲突中。

每个场景都必须证明：不重复 Merge/Revert、不并发释放 Lease、无法解释时 Blocked。

## Exit Gate

- [ ] 两个同时完成的 Attempt 只有一个先推进 Best，另一个刷新。
- [ ] Accepted 与 Rejected 各至少一个，并有完整 Artifact。
- [ ] 所有故障注入均可幂等恢复。
- [ ] Mainline 迟到失败可在已有后续提交时安全 revert。
- [ ] BestAdvanced 触发其他 Agent stale gate。
- [ ] 终态清理不会删除 Profiler/Patch/JSONL。

## 非目标

- Remote Sync。
- 完整 Pause/Stop/Draining UI。
- 真实生产 GPU Campaign 最终验收。
