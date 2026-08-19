# Pika v1 MCP API

## 1. 传输与身份

Phoenix 在 loopback 暴露 Streamable HTTP `/mcp`。每个 Backend Session 获得独立短期 Bearer Token；服务端记录 Token 哈希，并把明文 Token 绑定到内存中的 Session identity：

```json
{
  "campaign_id": "...",
  "backend_session_id": "...",
  "role": "iteration",
  "attempt_id": "...",
  "sync_run_id": null
}
```

工具参数不接受 `campaign_id` 或任意 Session/Attempt 身份切换。跨 Attempt 读取只能经过明确历史工具。所有写工具必须包含调用方生成的 `idempotency_key`；响应由 `idempotency_records` 去重。

Backend-specific 注入方式：

- Codex App Server：每个独立进程通过 config override 设置 `mcp_servers.pika.url`、`mcp_servers.pika.bearer_token_env_var` 和 `mcp_servers.pika.required=true`，明文 Token 只放在该进程环境变量中。
- Cursor ACP：在 `session/new` 时传入 Pika Streamable HTTP MCP 配置与该 Session Token。

两种 Backend 看到相同的 Role-scoped 工具集合，Pika MCP 不暴露 provider-specific 工具。

统一错误码：

- `invalid_state`：当前领域状态不允许操作。
- `stale_best`：调用方 `base_sha` 不是当前 Best。
- `forbidden_role`：Role 没有该工具。
- `identity_mismatch`：参数资源不属于 Token identity。
- `idempotency_conflict`：同 key 使用不同请求。
- `protected_path_changed`：候选修改受保护文件。
- `missing_required_data`：完成门禁缺字段/Artifact/Metric。
- `blocked`：Campaign 已阻止危险推进。

## 2. 所有工作 Role 可用的读取工具

### `get_context`

返回当前 Campaign 状态、Spec Revision、Sampling Revision、Best SHA、调用方 Role/Attempt、停止条件、选中 Ref/Skill、本角色必需完成操作以及未读 BestAdvanced/SamplingAdvanced/Guidance。

### `query_attempt_history`

参数：`limit`、可选 `before_ordinal`、`outcome`、`tags`。返回 Description、Summary、Outcome、Metric delta、Base/Result SHA 与关键失败原因；不返回其他 Agent 的秘密环境或完整 SQLite 行。

### `get_attempt`

读取调用方 Attempt 完整上下文，或按历史查询权限读取终态 Attempt。

### `list_agents`

返回同一 Campaign 的 Backend Session ID、Role、Attempt、Backend、协议、状态和最后活动时间。

### `read_agent_messages`

按 Mailbox sequence 返回发给当前 Session 的消息。支持 `after_sequence` 和 `limit`。

### `ack_agent_messages`

确认已处理到指定 sequence，幂等。

### `send_agent_message`

参数：同 Campaign `target_session_id`、`body`、`priority`。消息先写 SQLite，再至少一次投递。

### `register_artifact`

参数：`kind`、Workspace 相对路径、SHA-256、大小、MIME、metadata。Pika 校验路径、文件存在性和哈希后登记。

## 3. Boundary Role

### `submit_spec`

提交 Campaign Spec draft 或 Revision：计算语义、输入契约、Fusion、Cases、Metrics、Harness、停止条件、选中 Ref。返回 Spec Revision 与缺失项；不等于用户确认。

### `submit_harness`

登记 Reference、正确性测试、Benchmark Harness 路径与 digest。Pika 验证 protected paths 后将 Spec 置为 `awaiting_confirmation`。

### `complete_setup_merge`

用户在 UI 确认 Campaign Spec 后，Boundary Agent 提交 `base_sha`、setup commit SHA 和 squash 后 `best_sha`。Pika 独立核验 `pika/best` 的父提交、Diff 和 protected digest；自然语言或 Git 命令退出码不能代替该工具。

### `submit_baseline`

Boundary Agent 在已核验的 Best SHA 上完成正确性、每个 Case/Metric 的 30 个交替自配对、以及至少一个 Target Case 的 Profiler 后，提交 raw Pair JSONL、正确性报告、Profiler manifest 和 Summary 的 Artifact 引用。Pika 读取原始文件并重新计算 Baseline 中位数、Pair delta、MAD、有效 Pair 数和 `max(0.5%, 3×1.4826×MAD)`；不接受 Agent 预计算值作为权威结果。有效 Pair 少于 24 时只允许整组重跑一次。

### `submit_iteration_sample`

只在全量 Baseline 已接受后可用。参数包含最多十个初始 Case IDs、逐项选择理由、预计 Iteration/Full 测量秒数、节省比例和 Summary。Pika 校验它是 Full Case Set 的非空子集、至少包含一个 Target Case，并创建首个 Sampling Revision。该调用完成前 Campaign 停留在 `SelectingIterationSample`，不能进入 Optimizing。

Boundary 在 DraftingSpec 的完成门禁要求 `submit_spec` 与 `submit_harness` 都成功，且 UI 已出现可确认 Spec diff。用户确认不是 MCP Agent 工具。确认后依次要求 `complete_setup_merge`、`submit_baseline` 和 `submit_iteration_sample`；缺少调用时继续使用同 Session 无限 follow-up，Backend 失效则创建新 Session 重建上下文。

## 4. Plan Role

### `submit_plan`

参数：`markdown`、`summary`、`hypothesis`、`scope`、`expected_metric_effect`。Pika 原子写入 Artifact Workspace 的 `plan.md`，登记 Artifact 并完成 Plan Role。禁用 Plan 时工具不可用。

## 5. Iteration Role

### `record_metrics`

参数包括 `sampling_revision_id`、`base_sha`、`candidate_sha`、采样集中每个 Case/Metric 的 raw value、baseline value、improvement、MAD、noise tolerance、Pair counts、Harness Artifact。Pika 检查：

- `base_sha` 等于当前 Best；否则返回 `stale_best` 并附新 SHA。
- Candidate SHA 属于当前 Attempt Branch。
- `sampling_revision_id` 等于 Attempt 创建时固定版本，Metric keys 完整覆盖该采样集 × Metrics。
- Pair 数和公式满足 Spec。
- 正确性已经登记通过。

同一 Attempt/Case/Metric 重复提交覆盖最新快照。

### `submit_attempt_summary`

提交 Description、Summary、尝试方向、修改范围、正确性结果、Profiler 摘要、风险和推荐 Outcome。Pika 不直接相信推荐 Outcome，仍执行门禁。

### `complete_attempt`

参数：`sampling_revision_id`、`base_sha`、`candidate_sha`、worktree status、最新 commit。完成前要求采样版本 Metric、Summary、Patch 可生成、无 protected path 修改和 clean worktree。成功后 Attempt 进入 `ready_for_integration`。

Backend Turn 结束但缺少任一必需工具时，Pika 向同一 Backend Session 发送 follow-up；没有次数或时间预算。

## 6. Integration Role

### `acquire_integration_lease`

参数：`expected_best_sha`、`attempt_id`。事务检查 Best、FIFO 队首、Agent identity 与现有 Lease，返回 Lease ID 和当前 Best。没有 TTL。

### `submit_full_regression`

参数：Lease ID、`base_sha`、`candidate_sha`、全量正确性 Artifact、每个 Full Case/Metric 的 5 Pair Screening Artifact，以及异常组合的独立 30 Pair Artifact。Pika 重算结果：Screening 至少 4/5 有效；中位数回退超过当前 Best noise tolerance 或样本无效的组合必须出现在完整 Artifact；完整测量至少 24/30 有效。任一组合确认回退即拒绝候选，否则生成只能用于该 Lease/Base/Candidate 的 Full Regression Receipt。

### `submit_sampling_feedback`

只在 Full Regression 已拒绝候选且存在尚未采样的确认回退 Cases 时可用。参数包括确认回退 Case IDs、Integration Agent 选择的代表 Case IDs 与逐项理由。Pika 校验选择是回退集合的非空子集，原子追加 Sampling Revision、Sampling Advanced、Attempt rejection 和 Lease release；已全部采样时不创建空 Revision。

### `complete_merge`

参数：Lease ID、Full Regression Receipt、Operation Intent ID、pre/post SHA、squash SHA、正式全量 Metric 快照、Patch Artifact、trailers。Pika 从 Git 独立核验：

- 父提交等于 Lease Best。
- squash commit trailer 完整。
- protected paths、`ref/**` 和 Pika `.gitmodules` 增量不存在。
- Receipt 与 Lease/Base/Candidate 完全匹配，且在 Git mutation 前签发。

事务提交 Accepted/Rejected、Best Revision、Metrics、BestAdvanced 和 Lease 释放。

## 7. Sync Role

### `complete_sync`

参数：Sync Intent、remote/branch、remote_before/candidate/remote_after/best_after SHA、正确性、Best Metrics、Sync Trail Artifact。Pika 核对 remote 和本地 Git；若 protected Harness 改变，返回 `awaiting_spec_confirmation`，不能完成。

## 8. Side Conversation Role

只可使用读取、Mailbox 和 Artifact 工具，没有完成门禁，不能提交 Metric、Spec、Full Regression、Merge 或 Sync。BTW 的“注入父 Attempt/后续 Attempts”由用户 UI 动作创建 Guidance，不由 Side Agent 自行调用。

## 9. Tool 权限矩阵

| Tool family | Boundary | Plan | Iteration | Integration | Sync | Side |
|---|---:|---:|---:|---:|---:|---:|
| Context/history | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Mailbox | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Artifact | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Spec/Harness/Baseline/Sample | ✓ |  |  |  |  |  |
| Plan |  | ✓ |  |  |  |  |
| Metrics/Summary/Attempt complete |  |  | ✓ |  |  |  |
| Full Regression/Sampling Feedback/Merge |  |  |  | ✓ |  |  |
| Sync |  |  |  |  | ✓ |  |

## 10. MCP conformance tests

- 每个写工具重复相同 idempotency key 返回同响应，不产生重复 Domain Event。
- 同 key 不同 body 返回 `idempotency_conflict`。
- 错误 Role、Attempt 或 Campaign 身份返回拒绝且不泄露资源是否存在。
- 服务重启后旧 Token 失效，新 Backend Session 能读取未读消息和恢复上下文。
- `record_metrics`、`complete_attempt` 和 Integration 在 BestAdvanced 后拒绝陈旧 Base。
- MCP 工具异常不得导致 Backend Session 进程或 Phoenix Endpoint 崩溃。
