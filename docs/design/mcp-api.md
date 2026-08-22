# Pika v1 MCP API

## 1. 传输与身份

Phoenix 在 loopback 暴露 Streamable HTTP `/mcp`。每个 Backend Session 获得独立短期 Bearer Token；服务端记录 Token 哈希，并把明文 Token 绑定到内存中的 Session identity：

```json
{
  "campaign_id": "...",
  "backend_session_id": "...",
  "role": "iteration",
  "work_kind": "attempt",
  "work_id": "...",
  "attempt_id": "...",
  "sync_run_id": null
}
```

Directory 还把 Token 绑定到当前 Actor；工具参数不接受 `campaign_id` 或任意 Session/Work 身份切换。跨 Attempt 读取只能经过明确历史工具。工具目录取自 Actor 打开 Session 时冻结的 Role Definition。所有写工具必须包含调用方生成的 `idempotency_key`；响应按 `Role + Agent Work + operation + key` 由 `agent_operation_receipts` 去重，因此恢复 Session 不会丢失幂等身份。

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

## 2. Attempt 工作 Role 的共享工具

以下工具由 Plan 与 Iteration 共享；其他 Role 只获得各自 Definition 明确列出的工具，不存在隐式的全局工具集合。

### `get_context`

返回当前 Campaign 状态、Spec Revision、Sampling Revision、Best SHA、调用方 Role/Attempt、停止条件、选中 Ref/Skill、本角色必需完成操作以及未读 BestAdvanced/SamplingAdvanced/Guidance。

### `query_attempt_history`

参数：`limit`、可选 `before_ordinal`、`outcome`、`tags`。返回 Description、Summary、Outcome、逐 case Metrics、Base/Result SHA 与关键失败原因；不返回其他 Agent 的秘密环境或完整 SQLite 行。Prompt 中的 compact 摘要不改变或替代本工具，Agent 仍可用它查询超出 prompt N 限制的完整终态历史。

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

参数：`kind`、以 `artifacts/` 开头的 Workspace 相对路径、SHA-256、大小、MIME、metadata。Pika 使用有界内存流式校验路径、文件存在性和哈希后登记；Workspace 中其他目录不能登记为 Artifact。Baseline 使用下面的单 Manifest 流程，不需要逐个调用本工具。

## 3. Alignment、Setup Merge 与 Baseline Roles

三个 Role 共享同一个 Campaign/Spec Revision 领域流程，但使用不同 Actor、Backend Session、Token 和最小工具目录：

- Alignment：`get_context`、`ask_questions`、`register_artifact`、`submit_spec`、`submit_harness`、`submit_implementation_bundle`、`submit_implementation_review`。
- Setup Merge：`get_context`、`complete_setup_merge`。
- Baseline：`get_context`、`register_artifact`、`reopen_baseline_definition`、`submit_baseline`、`submit_iteration_sample`。

Alignment Session 只注入 Instructions 并等待真实用户首条消息；Setup Merge 与 Baseline 由已持久化的确认动作自动启动。Role 切换必然关闭旧 Actor 并创建新的 provider Session。

### `submit_spec`

提交 Campaign Spec v2 draft 或 Revision：计算语义、输入契约、Fusion、Correctness Oracle、固定 Optimization Target、可变 Development、Cases、Metrics、Harness、停止条件和选中 Reference Projects。返回 Spec Revision 与缺失项；不等于用户确认。旧 v1 `reference_path` 不能推断为任一实现角色。

### `submit_harness`

登记仓库内 Oracle（若 Spec 使用独立路径）、正确性测试、Benchmark Harness 路径与 digest。Pika 验证 protected paths；Development 入口不能受保护，外部 Target Snapshot 不属于产品仓库 protected paths。

### `submit_implementation_bundle`

绑定干净的 setup commit 作为初始 Development，并异步固化 Optimization Target。Target 可来自该 Development commit 的精确快照，或来自选中 Reference Project 的固定 SHA；Pika 返回 Target Snapshot ID，验证 Development/Oracle 入口，并在 Workspace `targets/` 建立不可变 checkout。该调用不传源码或测量数据。

### `submit_implementation_review`

登记当前 Target 与初始 Development 在同一个 Spec Benchmark Case 上的成功 smoke run。提交绑定 Spec revision、Harness digest、Target Snapshot ID 和 Development SHA，包含实际命令、执行环境、退出码 0、两者均通过 Oracle 的结果、至少一个 Target/Development 配对 Metric，以及 kind 为 `implementation_review_evidence` 的已登记本地输出 Artifact。这份证据用于用户同时审阅 Oracle、Target、Development 源码和首次运行结果，不是 Full Case Baseline；任何绑定身份或 Artifact 变化都会使其失效。

### `complete_setup_merge`

用户在 UI 确认 Campaign Spec 后，Setup Merge Actor 提交 `base_sha`、setup commit SHA 和 squash 后 `best_sha`。Pika 独立核验 `pika/best` 的父提交、Diff 和 protected digest；自然语言或 Git 命令退出码不能代替该工具。

### `reopen_baseline_definition`

Baseline Agent 在冻结的 Oracle、Target、Development、Harness、Case/Metric 契约或测量协议无法产生有效 Baseline 时，提交 `idempotency_key`、具体 `reason` 和 `requested_changes`。这是 Baseline Role 独有的逃生转换，不是完成门禁，也不能用于可原地重试的临时命令、依赖、GPU 或网络故障。Pika 会终止正在进行的 Baseline 校验和旧 Backend Session；setup 已合并时从当前 Best 创建下一条 `pika/setup/<revision>`，将技术交接发送给新的 Alignment Agent，并回到 `DraftingSpec`。修订后的 Spec、Harness、Implementation Bundle 和 Review Evidence 仍必须重新提交并由用户确认，Agent 不能借此绕过确认边界。未显式改变 Target 定义时继续引用原 Target Snapshot。

### `submit_baseline`

Baseline Agent 在已核验的 Target Snapshot 与 Development Best SHA 上完成正确性，并为每个 Case/Metric 按用户在 Campaign Spec 中指定的正式 Pair 数交替运行 Target/Development；然后把输出写入本地 Artifact Workspace，并创建一个小型 schema v2 JSON Manifest。Manifest 的必填项是 `target_snapshot_id`、`candidate_sha`、`summary`、`samples_artifact` 和 `correctness_artifact`。`profiler_artifact` 与完整的 `profiler_dependencies` 是可选且必须同时出现的诊断附件；它们不会决定 Baseline 是否有效。`submit_baseline` 只传 `idempotency_key` 与 `manifest_artifact`；原始数据不经过 MCP。

Pair JSONL 按 Spec 的 Case 顺序、Metric 顺序和递增 `pair_index` 分组写入。Pika 立即返回 `validating_baseline`，随后在 Campaign GenServer 之外以有界内存单次扫描原始文件：同一遍扫描完成 SHA-256、字节数、Pair 完整性以及 Baseline 中位数、Pair delta、MAD、有效 Pair 数和 `max(0.5%, 3×1.4826×MAD)` 的计算。其余 Manifest 文件由 Pika 在后台流式登记。校验期间 `get_context` 与 UI 快照保持可用并报告记录/分组进度。Agent 收到 accepted 响应后不应重复提交，等待 Pika 主动通知最终结果。不接受 Agent 预计算值作为权威结果。有效 Pair 少于用户指定的 `min_valid_pairs` 时只允许整组重跑一次。

### `submit_iteration_sample`

只在全量 Baseline 已接受后可用。参数包含最多十个初始 Case IDs、逐项选择理由、预计 Iteration/Full 测量秒数、节省比例和 Summary。Pika 校验它是 Full Case Set 的非空子集、至少包含一个 Target Case，并创建首个 Sampling Revision。该调用完成前 Campaign 停留在 `SelectingIterationSample`，不能进入 Optimizing。

Alignment 在 DraftingSpec 的完成门禁要求 `submit_spec`、`submit_harness`、`submit_implementation_bundle` 与 `submit_implementation_review` 都成功，且 UI 已出现可确认 Spec diff、Oracle/Target/Development 源码和配对运行证据。用户确认不是 MCP Agent 工具。确认后 Setup Merge 要求 `complete_setup_merge`，Baseline 再要求 `submit_baseline` 与 `submit_iteration_sample`；缺少调用时根据 committed facts follow-up，Backend 失效则用新 Actor/Session 重建同一 Work 上下文。

用户确认还必须绑定当前 Target digest、Development SHA 与 Implementation Review Evidence digest。UI 在有界源码预览中分别显示 Oracle、Target、Development 的路径、大小、内容和哈希，并要求用户显式确认已审阅；Pika 接受确认前重新核验 Target checkout、Development commit、Harness digest 与证据 Artifact。任一身份变化后旧审阅确认不能复用。

## 4. Plan Role

### `submit_plan`

参数：`markdown`、`summary`、`hypothesis`、`scope`、`expected_metric_effect`。Pika 原子写入 Artifact Workspace 的 `plan.md`，登记 Artifact 并完成 Plan Role。禁用 Plan 时工具不可用。

## 5. Iteration Role

### `record_metrics`

参数只引用本地 Target/Candidate JSONL 与 correctness Artifact，并包括 `sampling_revision_id`、`base_sha`、`candidate_sha`。Pika 重算采样集中每个 Case/Metric 的 Target value、Development value、`target_relative_improvement`、相对当前 Best 的 `best_relative_improvement`、MAD、noise tolerance 和 Pair counts。Pika 检查：

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

Backend Turn 结束但缺少任一必需工具时，Actor 根据最新 committed facts 发送 follow-up。单个 Session 使用有界 follow-up 防止坏会话永久占用；通常达到边界只会把 Work 标记为可恢复的中断，Symphony 用新的 Session 继续。Integration 是显式例外：单个 Integration Session 最多强制 follow-up 50 次，仍无领域进展时直接拒绝对应 Attempt，避免无响应 Agent 被无限恢复。

## 6. Integration Role

### `acquire_integration_lease`

参数：`expected_best_sha`、`attempt_id`。事务检查 Best、FIFO 队首、Agent identity 与现有 Lease，返回 Lease ID 和当前 Best。没有 TTL。

### `submit_full_regression`

参数：Lease ID、`base_sha`、`candidate_sha`、全量正确性 Artifact、每个 Full Case/Metric 的 5 Pair Screening Artifact，以及异常组合按 Campaign Spec 正式 Pair 数生成的独立 Artifact。Pika 重算结果：Screening 至少 4/5 有效；中位数回退超过当前 Best noise tolerance 或样本无效的组合必须出现在完整 Artifact；完整测量必须达到 Spec 的 `min_valid_pairs`。任一组合确认回退即拒绝候选，否则生成只能用于该 Lease/Base/Candidate 的 Full Regression Receipt。若 Full Regression 的正确性校验返回 `correctness_failed`（包括正确性失败或 Full Case 覆盖不完整），Pika 原子签发 rejected Receipt 并直接拒绝 Attempt，不要求 Agent 再调用 `reject_attempt`。

### `submit_sampling_feedback`

只在 Full Regression 已拒绝候选且存在尚未采样的确认回退 Cases 时可用。参数包括确认回退 Case IDs、Integration Agent 选择的代表 Case IDs 与逐项理由。Pika 校验选择是回退集合的非空子集，原子追加 Sampling Revision、Sampling Advanced、Attempt rejection 和 Lease release；已全部采样时不创建空 Revision。

### `complete_merge`

参数：Lease ID、Full Regression Receipt、Operation Intent ID、pre/post SHA、squash SHA、正式全量 Metric 快照、Patch Artifact、trailers。Pika 从 Git 独立核验：

- 父提交等于 Lease Best。
- squash commit trailer 完整。
- protected paths 和 `ref/**` 软链接不存在；用户自己的 `.gitmodules` 只按普通候选变更处理。
- Receipt 与 Lease/Base/Candidate 完全匹配，且在 Git mutation 前签发。

事务提交 Accepted/Rejected、Best Revision、Metrics、BestAdvanced 和 Lease 释放。

## 7. Sync Role

### `complete_sync`

参数：Sync Intent、remote/branch、remote_before/candidate/remote_after/best_after SHA、正确性、Best Metrics、Sync Trail Artifact。Pika 核对 remote 和本地 Git；若 protected Harness 改变，返回 `awaiting_spec_confirmation`，不能完成。

## 8. Progress Summary Role

这个 Role 只有 `get_progress_context` 与 `submit_progress_summary`。前者读取定时器到期时已经持久化的不可变快照；后者提交唯一权威 Summary 并完成 Request。它没有文件、Shell、Git、Metric、Mailbox 或其他写权限，Pika 不解析 Backend 的自然语言尾输出来生成 Summary。

## 9. Side Conversation Role

只可使用读取、Mailbox 和 Artifact 工具，没有完成门禁，不能提交 Metric、Spec、Full Regression、Merge 或 Sync。BTW 的“注入父 Attempt/后续 Attempts”由用户 UI 动作创建 Guidance，不由 Side Agent 自行调用。

## 10. Tool 权限矩阵

| Tool family | Alignment | Setup Merge | Baseline | Plan | Iteration | Integration | Sync | Progress Summary | Side |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| Role context | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Attempt history / Mailbox |  |  |  | ✓ | ✓ |  |  |  | ✓ |
| Artifact registration | ✓ |  | ✓ | ✓ | ✓ | ✓ | ✓ |  | ✓ |
| Spec/Harness/Implementation Review | ✓ |  |  |  |  |  |  |  |  |
| Setup merge |  | ✓ |  |  |  |  |  |  |  |
| Baseline/Sample/Reopen |  |  | ✓ |  |  |  |  |  |  |
| Plan |  |  |  | ✓ |  |  |  |  |  |
| Attempt Metrics/Summary/complete/reject |  |  |  |  | ✓ |  |  |  |  |
| Full Regression/Lease/Merge/reject |  |  |  |  |  | ✓ |  |  |  |
| Sync |  |  |  |  |  |  | ✓ |  |  |
| Submit captured progress summary |  |  |  |  |  |  |  | ✓ |  |

## 11. MCP conformance tests

- 每个写工具重复相同 idempotency key 返回同响应，不产生重复 Domain Event。
- 同 key 不同 body 返回 `idempotency_conflict`。
- 错误 Role、Attempt 或 Campaign 身份返回拒绝且不泄露资源是否存在。
- 服务重启后旧 Token 失效，新 Backend Session 用同一 Agent Work 读取 committed context，且 work-scoped command receipt 仍可重放。
- `record_metrics`、`complete_attempt` 和 Integration 在 BestAdvanced 后拒绝陈旧 Base。
- MCP 工具异常不得导致 Backend Session 进程或 Phoenix Endpoint 崩溃。
