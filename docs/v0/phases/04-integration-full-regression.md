# Phase 4 — Integration Full Regression

## 状态

Not started

## 依赖

[Phase 3](./03-attempt-loop.md) Exit Gate 全通过。

## 目标

把 `ready_for_integration` Attempts 串行、安全、可恢复地推进到 Campaign Best Branch；在任何 Git mutation 前对 Full Case Set 完成强制回归，并把确认回退的代表 Case 反馈到后续 Sampling Revision。

## 本 Phase 交付

- FIFO Integration Queue 与无 TTL Integration Lease。
- 陈旧 Base Refresh、Full Regression Receipt 和 merge Operation Intent。
- 全量正确性、5 Pair Screening、异常组合 30 Pair escalation。
- Universal no-regression gate、Regression Feedback 和 Sampling Advanced。
- Agent-owned squash merge、Pika 独立 Git 核验、BestAdvanced。
- 终态 Attempt Artifact 与 worktree/branch 清理。

## 实施顺序

### 4.1 Integration Queue 与 Refresh

- [ ] `ready_for_integration` 按 Attempt ordinal FIFO 排队，同一时刻最多一个 Integration Agent。
- [ ] `acquire_integration_lease(expected_best_sha)` 原子检查队首、Best、Agent identity 和现有 Lease。
- [ ] Base 陈旧时在 Attempt Branch 上 rebase/解决冲突，按 Attempt 固定 Sampling Revision 重跑正式 Metrics。
- [ ] `pika/best` 本身绝不 rebase。

### 4.2 归并前全量回归

- [ ] 在 Git mutation 前验证 Full Case Set 正确性。
- [ ] 每个 Full Case/Metric 做 5 Pair Screening，至少 4 Pair 有效。
- [ ] Screening 中位数回退超过当前 Best noise tolerance 或样本无效的组合，独立重跑完整 30 Pair。
- [ ] 完整测量至少 24 Pair 有效；任一 Case/Metric 确认回退都拒绝，包括 Informational。
- [ ] 通过后签发绑定 Lease、Base SHA、Candidate SHA、Harness digest 的 Full Regression Receipt。
- [ ] Receipt 缺失、过期或身份不匹配时禁止创建 merge Intent。

### 4.3 Regression Feedback

- [ ] 被拒绝候选不修改 Campaign Best Branch。
- [ ] 若确认回退 Case 尚未采样，Integration Agent 按形状族、幅度和线上权重提交至少一个代表 Case。
- [ ] Pika 校验选择是确认回退集合的子集，原子提交 Sampling Revision、Sampling Advanced、Attempt rejection 和 Lease release。
- [ ] 同一 Spec Revision Sampling Revision 只增不减；初始十个上限不约束反馈版本。
- [ ] 活动 Attempt 收到通知但继续使用启动快照；新 Attempt 使用最新 Revision。

### 4.4 Agent-owned Merge 与独立核验

- [ ] Full Regression 通过后持久化 merge Intent。
- [ ] 移除 Pika 注入的 `ref/**` 与对应 `.gitmodules` 增量，保留用户原有 submodule。
- [ ] Agent squash merge 到最新 `pika/best`，commit trailers 包含 Attempt、Spec 和 Sampling Revision。
- [ ] `complete_merge` 引用 Full Regression Receipt。
- [ ] Pika 核验父提交、HEAD、Diff、protected paths、Receipt、trailers 和全量 Metric snapshot。
- [ ] Accepted、Best Revision、best_metrics、BestAdvanced 和 Lease release 同事务。

### 4.5 BestAdvanced 与清理

- [ ] BestAdvanced 含 old/new SHA、Attempt、Spec/Sampling Revision 和全量 Metric delta。
- [ ] 所有活动 Agent Mailbox 至少一次收到 BestAdvanced，不 interrupt 当前 Turn。
- [ ] 终态前持久化 Patch、Summary、Metrics、Profiler、Full Regression Artifact、JSONL 和 commit 信息。
- [ ] 事务成功后移除 worktree/Attempt Branch；Interrupted/Blocked 不清理。

## 故障注入

- [ ] Lease 获取后、Screening 中、30 Pair escalation 中、Receipt 写入后分别 kill -9。
- [ ] Receipt 后 Git 前、squash 后 `complete_merge` 前、SQLite commit 前后分别 kill -9。
- [ ] 每个场景证明不重复 Full Regression/Merge、不错误释放 Lease、无法解释时进入 Blocked。

## Exit Gate

- [ ] 两个并发完成的 Attempt 只有一个先取得 Lease，另一个刷新最新 Best。
- [ ] 回退候选在 Git mutation 前被拒绝并能推进 Sampling Revision。
- [ ] 通过候选拥有完整 Full Case Set Metrics 并安全推进 Best。
- [ ] 当前有效设计中不存在 post-merge Mainline Validation、RevertRequired 或自动 Revert 队列。
