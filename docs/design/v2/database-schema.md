# Pika v2 SQLite 数据模型

## 1. 原则

- SQLite WAL 保存领域当前状态、标准化 Turn、最新 Metrics、Intent、Receipt 与 Domain Event。
- Git 保存代码与实际 mutation 现场；大型文件保存在 Artifact Workspace，数据库只保存相对路径、digest、大小与 kind。
- 一个数据库只有一个 Optimization，但保留稳定 `optimization_id` 作为外键、审计与幂等根身份。
- v2 使用全新 schema，不迁移或兼容 v1 Campaign/Sync 表。

## 2. 核心表

### `optimizations`

singleton root：`id`、repo/workspace identity、status、resume status、initial/base/current Best SHA、stop reason、created/updated timestamps。唯一约束保证只有一行。

### `baseline_revisions`

保存 revision、status、Definition Artifact、Definition digest、Development SHA、Target identity、Review feedback 与终态 reason。`(optimization_id, revision)` 唯一。

### `target_snapshots`

保存 Baseline Revision、独立 Snapshot 相对路径、normalized Target provenance、root Artifact/digest、entrypoint 和 immutable identity。一个 approved Revision 恰有一个 Snapshot；Attempt spawn 与 Integration 会重新核验其文件 digest。

### `baseline_reviews`

保存 revision、decision、绑定的 Definition/commit/dependency digest、用户 feedback 与时间。旧 digest 不得复用 Review。

### `baseline_question_batches`

保存 Alignment Agent 通过 `ask_questions` 提交的整批问题、所属 Revision/Session、用户整批答案和终态；同一 Revision 同时最多一个 pending Batch。

### `baseline_verifications`

保存 Result Manifest、outcome、Full Verify/Benchmark Artifact、Development SHA、全量统计状态和 rejection requested changes。

### `benchmark_cases` / `metric_definitions`

Case 使用 `(baseline_revision_id, integer_case_id)` 唯一键；保存 display metadata、weight、critical 与详细 JSON。Metric 保存 direction、unit、role 和聚合规则；`guard.max_regression_ratio` 保存在 `aggregation_json`，缺省空对象表示配置容差为 0。

### `sampling_revisions` / `sampling_revision_cases`

Sampling Revision 记录 sequence、cause 与创建时间。Case 只增不减；数据库约束或事务检查新集合是上一集合超集。Revision 0 最多十个，之后可以增长到 Full Case Set。

### `best_revisions` / `best_metrics`

Best Revision 0 来自 accepted Baseline Verification；之后每个 Accepted Attempt 创建一个递增 Revision。Metrics 只保存当前 Revision 的最新完整逐 Case值与噪声，不保存重复结构化观察；原始历史在 Artifact 中。

## 3. Attempt 与 Integration

### `attempts`

`id INTEGER PRIMARY KEY`，从 1 单调增长且不复用。保存 status、创建时 Base/Sampling/Guidance、当前 Iteration Round、Candidate SHA、Summary、outcome、failure reason 和 timestamps。

### `iteration_rounds`

保存 Attempt ID、round number、kind=`initial|stale_refresh|recovery`、Base SHA、Session identity、Result Manifest 与终态。`(attempt_id, round)` 唯一。

### `attempt_metrics`

保存 Attempt 最新 Sampling Revision Metrics；新 Iteration Round 覆盖该 Attempt 的权威候选值，但旧原始文件仍在对应 Round Artifact 中。

### `integration_runs`

一个 Attempt 至多一个 active Integration Run；保存 FIFO sequence、status、expected Best、validation Artifact、Agent judgement、Sampling Feedback 和 terminal outcome。

### `operation_intents`

Git mutation 前持久化 expected Best、Candidate、validation receipt、预期操作和 status。恢复时不能根据 PID 释放或猜测完成，必须核对实际 Git。

## 4. Agent 运行

### `agent_sessions`

保存 Role、Work kind/id、Backend 展开配置快照、System Prompt/Context digest、status、started/ended timestamps 和结束原因。provider session ID 只用于审计，不作为恢复身份。

### `conversation_turns`

保存 Work identity、session sequence、turn sequence、标准化 input/output messages、MCP calls、ended reason 与 partial/interrupted 标记。恢复、Follow-up 和 Progress Summary 文件只从此表构造。

### `followup_requests`

保存 target Role/Work/Session、required terminal operation、target follow-up sequence、generator attempt sequence、状态、生成消息和 delivered timestamp。唯一约束防止同一目标同时存在重复 active Request。

### `progress_summary_requests` / `progress_summaries`

Request 保存 snapshot cursor、previous summary、status 和目录；Result 只保存 `summary.md` 的相对路径、digest 与完成时间。同一时刻至多一个 active Request。

### `guidance_revisions`

保存版本化 Iteration Guidance。Attempt 创建时绑定 revision；已运行 Attempt 不改变。

## 5. 通用基础表

- `artifacts`：owner identity、kind、relative path、SHA-256、size、MIME、metadata。
- `operation_receipts`：`Role + Work + operation + idempotency_key` 唯一，保存 request digest 与 response。
- `domain_events`：事务内追加的领域事实和 UI 时间线，不保存 provider streaming events。

## 6. 关键事务

### Baseline Review Approve

同一事务核验 Review digest、推进 Revision 到 verifying、追加 Domain Event。Target Snapshot 文件在事务前完成 digest；数据库只引用已核验 Artifact。

### Baseline Verification Accept

同一事务提交 Verification、Baseline Snapshot、Best Revision 0、Best Metrics、Sampling Revision 0、Optimization `optimizing` 和 runnable Iteration projection。

### Attempt Create

同一事务分配整数 ID、捕获当前 Best/Sampling/Guidance、增加 created count 和追加事件。目录/Git 创建失败时用显式 preparation state 恢复，不复用 ID。

### Integration Reject

同一事务提交 rejection、可选 Sampling Revision、队列释放和事件。Sampling 新集合必须是旧集合超集。

### Integration Accept

`finish_integration` 核验 Intent 与 Git 后，同一事务提交 Accepted Attempt、Best Revision、Best Metrics、Integration terminal、Intent completed 和 BestAdvanced。

## 7. Pending 与索引

pending 查询覆盖 Attempt status `ready_for_integration|refreshing_iteration|integrating`。`max_pending_attempts=0` 时不施加 gate。

必须建立：Attempt status/id、Integration FIFO/status、Agent Session Work/status、Conversation Work/sequence、Follow-up target/status、Progress active status、Domain Event aggregate/sequence 与 Artifact owner/path 索引。
