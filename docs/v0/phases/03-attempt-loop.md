# Phase 3 — 并发 Attempt Loop

## 状态

Not started

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

- [ ] 实现 queued、running、awaiting_report、ready_for_integration、refreshing、interrupted、cancelled 状态。
- [ ] Attempt 创建时原子增加 `attempts_created`，立即消耗预算。
- [ ] `iteration_agents` 数组每项对应固定 Slot；数组长度就是并发度。
- [ ] Slot 完成/释放后领取下一个 Attempt。
- [ ] Pause/Sync/Draining 关闭新 dispatch。

### 3.2 Worktree

- [ ] `pika/attempt/<id>` 从当前 Best 创建。
- [ ] 路径固定 `attempts/<id>`。
- [ ] 注入 Campaign 已选的全部 Ref 到 `ref/<name>`。
- [ ] Skill 通过 Backend discovery 暴露，不写候选 branch。
- [ ] Agent 可在 Attempt Branch 内自由多次 commit。

### 3.3 Prompt Context

- [ ] 注入 Campaign Spec、Best SHA、protected paths、停止条件和本角色必需 MCP 操作。
- [ ] 默认注入最近 10 个终态 Attempt 的 Description、Summary、Outcome、Metric delta、关键失败原因。
- [ ] 未读 BestAdvanced、SamplingAdvanced 和 Guidance 不受 N 限制。
- [ ] 提供 `query_attempt_history`，不把完整历史硬塞 Prompt。

### 3.4 Plan 可选路径

- [ ] 默认关闭。
- [ ] 启用后先启动独立 Plan Backend Session。
- [ ] `submit_plan(markdown, summary)` 原子写 `artifacts/plans/<attempt-id>/plan.md`。
- [ ] 后续 Iteration Agent 通过 MCP Resource 或 Backend embedded resource 读取。
- [ ] Plan Session 和恢复 Session 不消耗 Attempt 预算。

### 3.5 Agent Mailbox 与 BTW

- [ ] `list_agents`、`send_agent_message`、`read_agent_messages`、`ack_agent_messages`。
- [ ] 消息先 SQLite 持久化，按 sequence 至少一次投递。
- [ ] BTW 只能从指定运行中 Attempt fork。
- [ ] 默认仅对话；“注入当前 Attempt”创建 Attempt Guidance 并调用 `AgentBackend.steer`。Codex 原生追加，Cursor cancel→新 Prompt。
- [ ] “注入后续 Attempts”创建 Campaign Guidance，不取消当前 Agent。

### 3.6 Iteration MCP

- [ ] `record_metrics`：校验 base/candidate SHA、Pair 统计、Harness、正确性。
- [ ] `record_metrics`/`complete_attempt` 校验 Sampling Revision 身份与完整覆盖。
- [ ] Sampling Advanced 投递活动 Agent，但不强迫已运行 Attempt 补测；新 Attempt 使用最新 Revision。
- [ ] `submit_attempt_summary`：Description、Summary、修改范围、风险、Profiler 摘要。
- [ ] `register_artifact`：Patch/Profiler/Prompt/Log。
- [ ] `complete_attempt`：要求 Metrics、Summary、clean worktree、无 protected 修改。
- [ ] 成功只进入 `ready_for_integration`；不得自行标记 Accepted。

### 3.7 正式配对测量

- [ ] Iteration Agent 可自由跑临时 Benchmark。
- [ ] 正式提交必须运行 Campaign Harness 的交替 30 Pair。
- [ ] 保存 raw value、baseline value、improvement、MAD、noise tolerance 和 valid count。
- [ ] Pika 重算公式，不信任 Agent 计算结果。

### 3.8 Completion 与恢复

- [ ] Backend Turn 结束但缺少必需 MCP 操作时进入 awaiting_report。
- [ ] 同一 Backend Session 无限 follow-up，无次数/时间预算。
- [ ] Session/进程失效进入 interrupted，启动新 Backend Session，注入 worktree、JSONL 尾部和缺失操作。
- [ ] 恢复不增加 Attempt 序号或预算。
- [ ] Stop Now 才能终止失控循环。

## 测试

- [ ] 三个 Fake Agent Slot 并行修改三个 worktree，互不污染。
- [ ] Agent 退出码 0 但漏掉 Metric/complete，不误完成。
- [ ] 同 Session 多次 follow-up，最终完成一次。
- [ ] 多次崩溃换新 Session，Attempt ID/预算不变。
- [ ] idempotency key 重试不重复 Metrics/Artifact/消息。
- [ ] Ref/Skill 不出现在候选 Patch。
- [ ] Campaign Guidance 只影响后续 Attempts；Attempt Guidance 只影响父 Attempt。
- [ ] 并行自由 Benchmark 不改变 Pika 调度状态。

## Exit Gate

- [ ] 至少三个并发 Fake Attempts 达到 ready_for_integration。
- [ ] 每个 Attempt 有独立 Branch、worktree、JSONL、Patch、Summary 和正式 Metrics。
- [ ] 缺失 MCP 的无限 follow-up 与崩溃恢复通过故障注入。
- [ ] History、Mailbox、BTW 三种 Guidance 的作用域验证通过。
- [ ] 尚未有任何 Attempt 推进 `pika/best`。

## 非目标

- Accepted/Rejected 最终判定和 squash merge。
- Integration Full Regression 与 Regression Feedback。
- Remote Sync 和完整 Metrics UI。
