# Phase 4 — Integration Full Regression

## 状态

Completed (2026-08-19)

## 依赖

[Phase 3](./03-attempt-loop.md) Exit Gate 全通过。

## 目标

把 `ready_for_integration` Attempts 串行、安全、可恢复地推进到 Campaign Best Branch；在任何 Git mutation 前对 Full Case Set 完成强制回归，并把确认回退的代表 Case 反馈到后续 Sampling Revision。

## 本 Phase 交付

- FIFO Integration Queue 与无 TTL Integration Lease。
- 陈旧 Base Refresh、Full Regression Receipt 和 merge Operation Intent。
- 全量正确性、5 Pair Screening、异常组合按 Campaign Spec 正式 Pair 数 escalation。
- Universal no-regression gate、Regression Feedback 和 Sampling Advanced。
- Agent-owned squash merge、Pika 独立 Git 核验、BestAdvanced。
- 终态 Attempt Artifact 与 worktree/branch 清理。

## 实施顺序

### 4.1 Integration Queue 与 Refresh

- [x] `ready_for_integration` 按 Attempt ordinal FIFO 排队，同一时刻最多一个 Integration Agent。
- [x] `acquire_integration_lease(expected_best_sha)` 原子检查队首、Best、Agent identity 和现有 Lease。
- [x] Base 陈旧时在 Attempt Branch 上 rebase/解决冲突，按 Attempt 固定 Sampling Revision 重跑正式 Metrics。
- [x] `pika/best` 本身绝不 rebase。

### 4.2 归并前全量回归

- [x] 在 Git mutation 前验证 Full Case Set 正确性。
- [x] 每个 Full Case/Metric 做 5 Pair Screening，至少 4 Pair 有效。
- [x] Screening 中位数回退超过当前 Best noise tolerance 或样本无效的组合，按 Campaign Spec 独立重跑完整正式 Pair 数。
- [x] 完整测量至少达到 Campaign Spec 的 `min_valid_pairs`；任一 Case/Metric 确认回退都拒绝，包括 Informational。
- [x] 通过后签发绑定 Lease、Base SHA、Candidate SHA、Harness digest 的 Full Regression Receipt。
- [x] Receipt 缺失、过期或身份不匹配时禁止创建 merge Intent。

### 4.3 Regression Feedback

- [x] 被拒绝候选不修改 Campaign Best Branch。
- [x] 若确认回退 Case 尚未采样，Integration Agent 按形状族、幅度和线上权重提交至少一个代表 Case。
- [x] Pika 校验选择是确认回退集合的子集，原子提交 Sampling Revision、Sampling Advanced、Attempt rejection 和 Lease release。
- [x] 同一 Spec Revision Sampling Revision 只增不减；初始十个上限不约束反馈版本。
- [x] 活动 Attempt 收到通知但继续使用启动快照；新 Attempt 使用最新 Revision。

### 4.4 Agent-owned Merge 与独立核验

- [x] Full Regression 通过后持久化 merge Intent。
- [x] 移除 Pika 注入的 `ref/**` 与对应 `.gitmodules` 增量，保留用户原有 submodule。
- [x] Agent squash merge 到最新 `pika/best`，commit trailers 包含 Attempt、Spec 和 Sampling Revision。
- [x] `complete_merge` 引用 Full Regression Receipt。
- [x] Pika 核验父提交、HEAD、Diff、protected paths、Receipt、trailers 和全量 Metric snapshot。
- [x] Accepted、Best Revision、best_metrics、BestAdvanced 和 Lease release 同事务。

### 4.5 BestAdvanced 与清理

- [x] BestAdvanced 含 old/new SHA、Attempt、Spec/Sampling Revision 和全量 Metric delta。
- [x] 所有活动 Agent Mailbox 至少一次收到 BestAdvanced，不 interrupt 当前 Turn。
- [x] 终态前持久化 Patch、Summary、Metrics、Profiler、Full Regression Artifact、JSONL 和 commit 信息。
- [x] 事务成功后移除 worktree/Attempt Branch；Interrupted/Blocked 不清理。

## 故障注入

- [x] Lease 获取后、Screening 中、正式 Pair escalation 中、Receipt 写入后分别故障注入。
- [x] Receipt 后 Git 前、squash 后 `complete_merge` 前、SQLite commit 调用前后分别故障注入。
- [x] 每个场景证明不重复 Full Regression/Merge、不错误释放 Lease；无法解释的状态进入 Blocked。

## Exit Gate

- [x] 两个并发完成的 Attempt 只有一个先取得 Lease，另一个刷新最新 Best。
- [x] 回退候选在 Git mutation 前被拒绝并能推进 Sampling Revision。
- [x] 通过候选拥有完整 Full Case Set Metrics 并安全推进 Best。
- [x] 当前有效设计中不存在 post-merge Mainline Validation、RevertRequired 或自动 Revert 队列。

## 验收证据

- `mix test test/pika/integration_full_regression_test.exs`：`10 passed`。
- `mix test test/pika/integration_full_regression_test.exs test/pika/measurement_test.exs`：`15 passed`。
- 故障矩阵覆盖 Lease、Screening、正式 Pair escalation、Receipt、Intent、squash、SQLite 事务前后，并断言恢复不会重复执行已完整落盘的 Screening/正式 Pair 测量。
