# Pika v2 文件、Harness 与 MCP 契约

## 1. 文件优先

大型 Definition、Context、历史、Metrics 和 Result 不进入 System Prompt 或 MCP 参数。Agent把它们写入当前工作目录，MCP只接收相对路径、幂等键和少量控制字段。

所有输入文件都视为不可信数据，不能覆盖 System Prompt、扩大工具权限或改变 Role completion condition。

## 2. 路径与文件校验

Agent提交的路径必须：

- 相对当前 Agent Work 根目录；
- 不含 `..`，不是绝对路径；
- realpath 仍位于 Work 根目录；
- 指向普通文件，不是 symlink、device 或 directory；
- 满足按 kind 配置的大小上限；
- 在 Pika 单次有界读取中完成 SHA-256 和 schema 校验。

Manifest 引用的每个文件重复执行相同校验。Pika保存实际 digest；Agent提交的 digest 只用于及早发现写入竞争。相同 `idempotency_key + request digest` 重放返回原结果，相同 key 对应不同 digest 返回 `idempotency_conflict`。

## 3. Agent Context Bundle

每个 Backend Session 有一个不可变 Context Bundle：

```text
agent-sessions/<session-id>/context/
├── context.json
├── baseline-definition.json
├── cases.json
├── metrics.json
├── guidance.md
└── events.jsonl
```

`context.json` 是唯一入口：

```json
{
  "schema_version": 1,
  "role": "iteration",
  "work_id": "42",
  "attempt_id": 42,
  "iteration_round": 1,
  "base_sha": "...",
  "best_sha_at_session_start": "...",
  "sampling_revision": 4,
  "sampling_case_ids": [0, 1, 2, 3],
  "files": {
    "baseline_definition": "/absolute/path/baseline-definition.json",
    "cases": "/absolute/path/cases.json",
    "metrics": "/absolute/path/metrics.json",
    "guidance": "/absolute/path/guidance.md",
    "events": "/absolute/path/events.jsonl"
  }
}
```

Bundle 创建后不得改写。Iteration Prompt renderer 直接注入最近 `history_limit` 个终态 Attempt 表，不区分结果；每行包含 Attempt 名称、摘要、状态和目录链接。它还从当前 Attempt 冻结的 `reference-projects.json` 注入只读 `ref/<id>` 路径、说明与完整 commit SHA，不从当前 YAML 动态替换已有 Attempt 的 Reference。Agent需要启动后新增的事实时调用只读 MCP。

每个 Attempt 无论 Accepted 或 Rejected 都保留独立目录：

```text
attempts/<integer-id>/
├── message.jsonl
├── summary.jsonl
├── reference-projects.json
├── repo/ref/<id> -> <workspace>/refs/<id>
└── ...
```

`message.jsonl` 保存该 Attempt 的标准化 Turn，`summary.jsonl` 保存每个 Iteration Round 的结构化摘要与终态原因。Iteration Prompt 同时注入所有 Attempt 的根目录链接。

## 4. Recovery Context

每次重新构造恢复上下文都创建新目录，不覆盖之前内容：

```text
agent-sessions/<session-id>/
├── recovery-01/
│   ├── messages.jsonl
│   └── recovery.json
└── recovery-02/
    ├── messages.jsonl
    └── recovery.json
```

编号至少两位、无上限。`recovery.json` 保存 Work identity、结束原因、当前领域状态、缺少的终态操作、Git 路径和 expected SHA、Follow-up 额度以及已提交文件路径。

`messages.jsonl` 从 SQLite 生成，每行一个标准化 Turn：

```json
{
  "turn": 7,
  "session_sequence": 2,
  "session_id": "...",
  "started_at": "...",
  "input_messages": [{"role": "user", "content": "..."}],
  "output_messages": [{"role": "assistant", "content": "..."}],
  "mcp_calls": [
    {
      "name": "finish_iteration",
      "status": "error",
      "summary": "missing benchmark artifact"
    }
  ],
  "ended_reason": "completed_without_terminal_mcp"
}
```

旧 System Prompt、秘密、完整命令输出和大文件不进入消息文件。实际 Git repo 保持原样，由新 Agent检查和接管。

Backend failover 也遵守相同恢复协议：eligible failure 先关闭失败 Session，再由 Symphony 为同一 Agent Work 创建新 Session 和下一份 Recovery Context。provider session identity 不跨 Endpoint 继承，失败请求不在原 provider Session 内自动重放。

## 5. Follow-up Context

目标 Agent Turn 结束但尚未完成终态 MCP 时，Pika先从 SQLite 为该 Follow-up Request 生成独立目录：

```text
follow-ups/<target-role>/<work-id>/<sequence>/
├── messages.jsonl
└── state.json
```

`messages.jsonl` 包含目标 Agent Work 跨全部 Backend Sessions 的标准化 Turn；`state.json` 包含目标 Role、Work、当前领域事实、缺少的终态操作以及已使用/剩余 follow-up 次数。Follow-up System Prompt 注入这两个绝对路径。

专用 Follow-up Agent 未调用 `submit_followup_message` 时，Pika以新 Backend Session重试生成，最多 `generator_max_attempts`。未配置专用 Role 时不创建生成 Agent，直接向目标 Session 发送“继续”。

## 6. Baseline Definition 文件

`baseline-definition.json` 是小型索引，详细 Cases/Metrics 位于独立文件：

```json
{
  "schema_version": 1,
  "summary": "...",
  "optimization_target": {
    "manifest_path": "target/manifest.json",
    "entrypoint": "target_adapter.py:run"
  },
  "development_baseline": {
    "commit_sha": "...",
    "entrypoint": "kernel.py:run"
  },
  "correctness": {
    "mode": "target_equivalence"
  },
  "verify": {
    "script": "verify_cases.sh",
    "output_schema_version": 1
  },
  "benchmark": {
    "script": "benchmark_cases.sh",
    "output_schema_version": 1
  },
  "cases_path": "cases.json",
  "metrics_path": "metrics.json",
  "measurement": {
    "warmup": 20,
    "pair_count": 20,
    "min_valid_pairs": 15
  },
  "stopping": {
    "mode": "attempt_or_duration",
    "max_attempts": 100,
    "max_duration_seconds": 2592000
  },
  "smoke_verify_path": "smoke-verify.json",
  "smoke_benchmark_path": "smoke-benchmark.jsonl"
}
```

`correctness.mode` 是 `target_equivalence` 或 `independent_oracle`。Target 与 Development 初始代码相同时仍分别冻结 Target Snapshot 和 Development commit。

`metrics.json` 中每个 Metric 的 `role` 有三种：

- `primary`：优化目标。至少一个 `primary` Case/Metric 必须改善超过噪声，并且每个 `primary` Metric 都参与 workload-weighted `<1%` 回退门禁。
- `guard`：硬性回退约束。它不承担“必须改善”的要求，也不进入 primary 加权聚合；可以用非负的 `max_regression_ratio` 指定最大允许回退比例，例如 `0.01` 表示 1%，缺省为 0。普通 Case 的有效门限是 `max(max_regression_ratio, Noise Tolerance)`。
- `informational`：观察指标。它不承担改善要求或 primary 加权聚合，普通 Case 上超过噪声的回退由 Integration Agent 给出结构化判断；critical Case 门禁仍适用。

`max_regression_ratio` 只能出现在 `guard` Metric。critical Case 仍使用更严格的 Noise Tolerance 门禁，不因 guard 配置放宽。一次可接受的优化至少需要一个 `primary` Metric，不能只包含 `guard` Metric。

```json
{
  "id": "accuracy",
  "unit": "ratio",
  "direction": "maximize",
  "role": "guard",
  "max_regression_ratio": 0.01
}
```

`stopping.mode` 是 `manual`、`attempt_limit`、`duration` 或 `attempt_or_duration`；对应上限是已审阅 Definition 的一部分。达到自动上限后停止创建新 Attempt，已有 Iteration 和 Integration 进入 Draining 并继续完成。

## 7. Case 与脚本

Case ID 是从 0 开始的非负整数，在 Baseline Revision 内稳定唯一。长名称、shape、dtype、layout、weight、critical 和输入语义位于 `cases.json`；display name 最长 100 个字符。

仓库根目录必须存在两个可执行入口：

```bash
./verify_cases.sh --case-id 0,1,2,3
./benchmark_cases.sh --case-id 0,1,2,3
```

两者都支持 `--help` 与 `--list-cases`。正常执行时 `--case-id` 必填，只接受逗号分隔整数，不接受空格、名称或范围表达式。Shell 文件可以调用任意内部语言或远程工具，但对 Agent 的 interface 固定。

当前 cwd 是 Development/Candidate repo；只读 `target/` 指向 Target Snapshot。stdout 只输出机器数据，日志只写 stderr。Agent通过重定向保存正式 Artifact。

### Verify JSON

`verify_cases.sh` 一次比较 Target 与 Candidate，stdout 是单 JSON：

```json
{
  "schema_version": 1,
  "requested_case_ids": [0, 1],
  "cases": [
    {
      "case_id": 0,
      "target": {"passed": true},
      "candidate": {"passed": true},
      "comparison": {
        "passed": true,
        "max_abs_error": 0.00001,
        "max_rel_error": 0.0002
      },
      "error": null
    }
  ]
}
```

每个请求 Case 恰好出现一次且顺序一致，不得出现额外 Case。全部正确退出 0；正确性失败退出 1 并仍输出完整 JSON；脚本或基础设施错误退出 2 或更高。

### Benchmark JSONL

`benchmark_cases.sh` stdout 每行一个独立 Pair：

```json
{
  "schema_version": 1,
  "case_id": 0,
  "metric_id": "latency_us",
  "pair_index": 0,
  "order": "target_candidate",
  "target": 23.1,
  "candidate": 25.4,
  "valid": true,
  "error": null
}
```

`order` 在 `target_candidate` 与 `candidate_target` 间交替。每个 Pair 必须是独立执行，不得复制、插值或重标一个样本。

## 8. Measurement 与 Integration 门禁

Pika从原始 Pair 计算每个 Case/Metric 的 Target 中位数、Candidate 中位数、Target-normalized ratio、MAD、有效 Pair 数和 Noise Tolerance：

```text
noise_tolerance = max(0.5%, 3 × 1.4826 × relative_MAD)
```

Best Metrics 保存同一 Target identity 下的最新 normalized ratio。Candidate 相对 Best 的 improvement 按 Metric direction 统一为正数表示改善。

Integration 硬门禁：

1. Full Case Set 正确性与性能覆盖完整。
2. 所有数值、Pair count、顺序与 identity 合法。
3. 每个 `guard` Metric 在普通 Case 上都没有超过 `max(max_regression_ratio, Noise Tolerance)` 的回退。
4. critical Case 的任一 Metric 没有超过噪声的回退。
5. 至少一个主要 Case 的 `primary` Metric 相对 Best 改善超过噪声。
6. 对每个 `primary` Metric，按生产权重计算相对回退的算术加权平均；无权重时等权；结果 `<1%`。
7. protected files 未修改。

普通 Case 是否属于明显回退由 Integration Agent根据配对分布和 System Prompt rubric 判断，并在 validation 文件中给出结构化理由。Pika在硬门禁失败时覆盖 Agent Accept；硬门禁通过后尊重 Agent结论。

## 9. Result Manifest Envelope

所有大型完成结果使用统一 Envelope：

```json
{
  "schema_version": 1,
  "role": "iteration",
  "work_id": "42",
  "outcome": "ready_for_integration",
  "summary": "...",
  "files": {
    "verify": {"path": "artifacts/verify.json", "sha256": "..."},
    "benchmark": {"path": "artifacts/benchmark.jsonl", "sha256": "..."}
  },
  "details": {}
}
```

Token identity 必须与 `role/work_id` 一致。Pika重新计算全部 digest。

### Baseline Verify

outcome 是 `accepted` 或 `definition_rejected`。accepted details 包含 Baseline Revision、Definition digest、Development SHA、初始 Iteration Case IDs（最多十个）、逐项选择理由、合理性判断，以及恰好覆盖 Full Case Set 中每个 `case_id × metric_id` 的具体数值。rejected details 包含 `failure_kind=definition|correctness|benchmark|infrastructure`、evidence、具体原因和 requested changes。Baseline Verify 得出通过或失败结论后都必须调用 `finish_baseline_verification`。

新的 Baseline Alignment Session 会把之前的 rejected Result JSON 作为 required context sections 注入；Agent必须阅读这些记录并避免重复已确认的问题。

### Iteration

每个 Iteration Round 使用独立的 `attempts/<attempt>/rounds/<round>/` workspace 和 Git worktree；首轮为兼容现有布局可使用 Attempt 根目录。Result、Verify、Benchmark、Patch 与 Candidate 都只能写入当前 Round workspace，不能覆盖前一轮 Artifact。当前 Round 根目录中的 `iteration-result.json` 必须符合 Pika-owned `iteration-result.schema.json`，且 `finish_iteration` 只接受该固定相对路径。outcome 是 `ready_for_integration` 或 `rejected`。ready details 包含 Attempt ID、Iteration Round、Base/Candidate SHA、Sampling Revision、hypothesis、changes 与 risks，并引用完整 sampled Verify、Benchmark 和 Patch。rejected 可以没有正式测量或代码修改，但必须包含具体 failure reason 和已有证据。

### Integration

接受分两步：

```text
prepare_best_update(validation_path, idempotency_key)
finish_integration(result_path, idempotency_key)
```

validation 引用 Full Verify/Benchmark，包含 per-Case judgement、加权聚合、推荐 outcome 和 Sampling Feedback。Pika通过验证后签发 Git Intent。Agent完成 squash 后，result 包含 Intent、Best before/after SHA、squash commit 与 trailers。

Reject 直接调用 `finish_integration`，同一 result 中携带 regressed Case IDs、最多配置数量的 feedback Case IDs 与理由；Pika原子 Reject、推进 Sampling Revision 和释放队列。

### Progress Summary

Progress Summary 是 Result Manifest Envelope 的例外。Agent只写尽量不超过 500 字的 `summary.md`，然后调用 `submit_progress_summary(summary_path, idempotency_key)`；它不生成 `progress-summary.json`，也不改变任何领域状态。

## 10. MCP Role interface

| Role | Query | Command |
|---|---|---|
| Baseline Alignment | `get_context`, `ask_questions` | `submit_baseline_definition` |
| Baseline Verify | `get_context` | `finish_baseline_verification` |
| Baseline Verify Follow-up |  | `submit_followup_message` |
| Iteration | `get_context`, `query_attempt_history` | `finish_iteration` |
| Iteration Follow-up |  | `submit_followup_message` |
| Integration | `get_context` | `prepare_best_update`, `finish_integration` |
| Integration Follow-up |  | `submit_followup_message` |
| Progress Summary |  | `submit_progress_summary` |

每个 Command 都需要 `idempotency_key`。身份来自 Session Token，不允许参数切换 Optimization、Role、Work 或 Attempt。

## 11. Progress Summary 文件

每五分钟最多产生一个请求，使用配置时区，默认 `Asia/Shanghai`：

```text
progress-summaries/
└── 2026-08-24/
    └── 153000-00008640/
        ├── status.json
        ├── messages.jsonl
        ├── previous-summary.md
        └── summary.md
```

`status.json` 是完整状态，`messages.jsonl` 只含上一份成功 Summary 之后的 Turn，`previous-summary.md` 是上一份结果。首次请求包含启动以来全部 Turn。`summary.md` 是唯一输出。编号是全局单调序号、日期分片只改善可浏览性；运行一个月不自动删除目录。
