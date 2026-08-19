# Phase 3 — 并发 Attempt Loop

## 状态

Completed (2026-08-19)

## 依赖

[Phase 2](./02-alignment-baseline.md) Exit Gate 全通过。

## 目标

用显式 Agent Slots 并行运行多个独立 Attempt，让 Agent 在 worktree 中开发、自由 Benchmark、提交结构化 Metrics/Summary，并在崩溃或漏报时继续同一 Attempt。该 Phase 到 `ready_for_integration` 为止，不推进 Best。

## 本 Phase 交付

- `attempts`、`attempt_metrics`、`agent_sessions`、`agent_messages`、`guidance` schema。
- Attempt 固定 `sampling_revision_id`，正式 Metrics 只要求该 Revision 的 Case × Metric。
- 显式 Iteration Slots 与 per-slot Agent Profile。
- Attempt branch/worktree 与选中 Ref submodule 注入。
- 最近 N 次历史、Agent Mailbox、BestAdvanced/SamplingAdvanced/Guidance 上下文。
- 可选 Plan Agent 和 Artifact `plan.md`。
- Iteration MCP：Metrics、Summary、Artifact、complete_attempt。
- 无限 MCP completion follow-up 与新 Session 恢复。
- Attempt 对话/BTW 的基础 LiveView。

## 实施顺序

### 3.1 Attempt 状态与调度

- [x] 实现 queued、running、awaiting_report、ready_for_integration、refreshing、interrupted、cancelled 状态。
- [x] Attempt 创建时原子增加 `attempts_created`，立即消耗预算。
- [x] `iteration_agents` 数组每项对应固定 Slot；数组长度就是并发度。
- [x] Slot 完成/释放后领取下一个 Attempt。
- [x] Pause/Sync/Draining 关闭新 dispatch。

### 3.2 Worktree

- [x] `pika/attempt/<id>` 从当前 Best 创建。
- [x] 路径固定 `attempts/<id>`。
- [x] 注入 Campaign 已选的全部 Ref 到 `ref/<name>`。
- [x] Skill 通过 Backend discovery 暴露，不写候选 branch。
- [x] Agent 可在 Attempt Branch 内自由多次 commit。

### 3.3 Prompt Context

- [x] 注入 Campaign Spec、Best SHA、protected paths、停止条件和本角色必需 MCP 操作。
- [x] 默认注入最近 10 个终态 Attempt 的 Description、Summary、Outcome、Metric delta、关键失败原因。
- [x] 未读 BestAdvanced、SamplingAdvanced 和 Guidance 不受 N 限制。
- [x] 提供 `query_attempt_history`，不把完整历史硬塞 Prompt。

### 3.4 Plan 可选路径

- [x] 默认关闭。
- [x] 启用后先启动独立 Plan Backend Session。
- [x] `submit_plan(markdown, summary)` 原子写 `artifacts/plans/<attempt-id>/plan.md`。
- [x] 后续 Iteration Agent 通过 MCP Resource 或 Backend embedded resource 读取。
- [x] Plan Session 和恢复 Session 不消耗 Attempt 预算。

### 3.5 Agent Mailbox 与 BTW

- [x] `list_agents`、`send_agent_message`、`read_agent_messages`、`ack_agent_messages`。
- [x] 消息先 SQLite 持久化，按 sequence 至少一次投递。
- [x] BTW 只能从指定运行中 Attempt fork。
- [x] 默认仅对话；“注入当前 Attempt”创建 Attempt Guidance 并调用 `AgentBackend.steer`。Codex 原生追加，Cursor cancel→新 Prompt。
- [x] “注入后续 Attempts”创建 Campaign Guidance，不取消当前 Agent。

### 3.6 Iteration MCP

- [x] `record_metrics`：校验 base/candidate SHA、Pair 统计、Harness、正确性。
- [x] `record_metrics`/`complete_attempt` 校验 Sampling Revision 身份与完整覆盖。
- [x] Sampling Advanced 投递活动 Agent，但不强迫已运行 Attempt 补测；新 Attempt 使用最新 Revision。
- [x] `submit_attempt_summary`：Description、Summary、修改范围、风险、Profiler 摘要。
- [x] `register_artifact`：Patch/Profiler/Prompt/Log。
- [x] `complete_attempt`：要求 Metrics、Summary、clean worktree、无 protected 修改。
- [x] 成功只进入 `ready_for_integration`；不得自行标记 Accepted。

### 3.7 正式配对测量

- [x] Iteration Agent 可自由跑临时 Benchmark。
- [x] 正式提交必须运行 Campaign Harness，并满足用户在 Campaign Spec 中声明的交替 Pair 数。
- [x] 保存 raw value、baseline value、improvement、MAD、noise tolerance 和 valid count。
- [x] Pika 重算公式，不信任 Agent 计算结果。

### 3.8 Completion 与恢复

- [x] Backend Turn 结束但缺少必需 MCP 操作时进入 awaiting_report。
- [x] 同一 Backend Session 无限 follow-up，无次数/时间预算。
- [x] Session/进程失效进入 interrupted，启动新 Backend Session，注入 worktree、JSONL 尾部和缺失操作。
- [x] 恢复不增加 Attempt 序号或预算。
- [x] Stop Now 才能终止失控循环。

## 测试

- [x] 三个 Fake Agent Slot 并行修改三个 worktree，互不污染。
- [x] Agent 退出码 0 但漏掉 Metric/complete，不误完成。
- [x] 同 Session 多次 follow-up，最终完成一次。
- [x] 多次崩溃换新 Session，Attempt ID/预算不变。
- [x] idempotency key 重试不重复 Metrics/Artifact/消息。
- [x] Ref/Skill 不出现在候选 Patch。
- [x] Campaign Guidance 只影响后续 Attempts；Attempt Guidance 只影响父 Attempt。
- [x] 并行自由 Benchmark 不改变 Pika 调度状态。

## Exit Gate

- [x] 至少三个并发 Fake Attempts 达到 ready_for_integration。
- [x] 每个 Attempt 有独立 Branch、worktree、JSONL、Patch、Summary 和正式 Metrics。
- [x] 缺失 MCP 的无限 follow-up 与崩溃恢复通过故障注入。
- [x] History、Mailbox、BTW 三种 Guidance 的作用域验证通过。
- [x] 尚未有任何 Attempt 推进 `pika/best`。

## 验收证据

- `mix test test/pika/attempt_loop_test.exs test/pika/measurement_test.exs`
- 全量回归：`123 passed, 1 excluded`（外部真实 Backend conformance 默认排除）。

## 非目标

- Accepted/Rejected 最终判定和 squash merge。
- Integration Full Regression 与 Regression Feedback。
- Remote Sync 和完整 Metrics UI。
