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

返回当前 Campaign 状态、Spec Revision、Best SHA、调用方 Role/Attempt、停止条件、选中 Ref/Skill、本角色必需完成操作以及未读 BestAdvanced/Revert/Guidance。

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

Boundary 完成门禁要求两项都成功，且 UI 已出现可确认 Spec diff。用户确认不是 MCP Agent 工具。

## 4. Plan Role

### `submit_plan`

参数：`markdown`、`summary`、`hypothesis`、`scope`、`expected_metric_effect`。Pika 原子写入 Artifact Workspace 的 `plan.md`，登记 Artifact 并完成 Plan Role。禁用 Plan 时工具不可用。

## 5. Iteration Role

### `record_metrics`

参数包括 `base_sha`、`candidate_sha`、每个 Case/Metric 的 raw value、baseline value、improvement、MAD、noise tolerance、Pair counts、Harness Artifact。Pika 检查：

- `base_sha` 等于当前 Best；否则返回 `stale_best` 并附新 SHA。
- Candidate SHA 属于当前 Attempt Branch。
- Pair 数和公式满足 Spec。
- 正确性已经登记通过。

同一 Attempt/Case/Metric 重复提交覆盖最新快照。

### `submit_attempt_summary`

提交 Description、Summary、尝试方向、修改范围、正确性结果、Profiler 摘要、风险和推荐 Outcome。Pika 不直接相信推荐 Outcome，仍执行门禁。

### `complete_attempt`

参数：`base_sha`、`candidate_sha`、worktree status、最新 commit。完成前要求全部 Metric、Summary、Patch 可生成、无 protected path 修改和 clean worktree。成功后 Attempt 进入 `ready_for_integration`。

Backend Turn 结束但缺少任一必需工具时，Pika 向同一 Backend Session 发送 follow-up；没有次数或时间预算。

## 6. Integration Role

### `acquire_integration_lease`

参数：`expected_best_sha`、`attempt_id`。事务检查 Best、FIFO 队首、Agent identity 与现有 Lease，返回 Lease ID 和当前 Best。没有 TTL。

### `complete_merge`

参数：Lease ID、Operation Intent ID、pre/post SHA、squash SHA、正式 Metric 快照、Patch Artifact、trailers。Pika 从 Git 独立核验：

- 父提交等于 Lease Best。
- squash commit trailer 完整。
- protected paths、`ref/**` 和 Pika `.gitmodules` 增量不存在。
- 正确性与 Pareto 门禁通过。

事务提交 Accepted/Rejected、Best Revision、Metrics、BestAdvanced 和 Lease 释放。

## 7. Mainline Role

### `complete_validation`

参数：固定被测 squash SHA、最新 Metrics 与正确性。Metrics 覆盖 Attempt 当前快照。通过则结束；失败则生成 RevertRequired。

### `complete_revert`

需要 Integration Lease。参数：被撤销 SHA、最新 Best、revert SHA、验证结果。Pika 核验 revert commit 与 Git 历史，推进 Best 并发出 BestAdvanced。

## 8. Sync Role

### `complete_sync`

参数：Sync Intent、remote/branch、remote_before/candidate/remote_after/best_after SHA、正确性、Best Metrics、Sync Trail Artifact。Pika 核对 remote 和本地 Git；若 protected Harness 改变，返回 `awaiting_spec_confirmation`，不能完成。

## 9. Side Conversation Role

只可使用读取、Mailbox 和 Artifact 工具，没有完成门禁，不能提交 Metric、Spec、Merge、Validation 或 Sync。BTW 的“注入父 Attempt/后续 Attempts”由用户 UI 动作创建 Guidance，不由 Side Agent 自行调用。

## 10. Tool 权限矩阵

| Tool family | Boundary | Plan | Iteration | Integration | Mainline | Sync | Side |
|---|---:|---:|---:|---:|---:|---:|---:|
| Context/history | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Mailbox | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Artifact | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Spec/Harness | ✓ |  |  |  |  |  |  |
| Plan |  | ✓ |  |  |  |  |  |
| Metrics/Summary/Attempt complete |  |  | ✓ |  |  |  |  |
| Integration Lease/Merge |  |  |  | ✓ | Revert only | Sync commit only |  |
| Mainline validation |  |  |  |  | ✓ |  |  |
| Sync |  |  |  |  |  | ✓ |  |

## 11. MCP conformance tests

- 每个写工具重复相同 idempotency key 返回同响应，不产生重复 Domain Event。
- 同 key 不同 body 返回 `idempotency_conflict`。
- 错误 Role、Attempt 或 Campaign 身份返回拒绝且不泄露资源是否存在。
- 服务重启后旧 Token 失效，新 Backend Session 能读取未读消息和恢复上下文。
- `record_metrics`、`complete_attempt` 和 Integration 在 BestAdvanced 后拒绝陈旧 Base。
- MCP 工具异常不得导致 Backend Session 进程或 Phoenix Endpoint 崩溃。
