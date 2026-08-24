# Pika v2 配置

## 1. 原则

- 配置没有 Agent Profile 或 Profile registry。
- 每个 Agent位置都展开完整 Backend 配置；YAML anchor/alias 只属于 YAML 语法，Pika 接收解析后的普通值。
- `iteration.agents` 的元素数量就是 Iteration 并发度。
- 其他已配置 Role 的并发度固定为 1；不同 Role 可以并行。
- Follow-up 与 Progress Summary Role 可省略。
- Session 创建时冻结当前完整配置；更新只影响以后创建的 Session。

## 2. 完整示例

```yaml
version: 2

repo: /path/to/repo
workspace: /path/to/workspace

agents:
  baseline_alignment: &codex_writer
    backend: codex
    model: gpt-5.6
    reasoning_effort: high
    approval_policy: never
    sandbox: workspace-write

  baseline_verify:
    <<: *codex_writer
    max_followups: 8

  # 可省略；省略时直接向 Baseline Verify 发送“继续”。
  baseline_verify_followup: &codex_reader
    backend: codex
    model: gpt-5.6
    reasoning_effort: medium
    approval_policy: never
    sandbox: read-only
    generator_max_attempts: 3

  iteration:
    history_limit: 20
    max_followups: 5
    max_pending_attempts: 0
    agents:
      - <<: *codex_writer
      - backend: cursor
        model: auto
        approval_policy: force
        sandbox: disabled

  # 可省略；省略时直接向 Iteration 发送“继续”。
  iteration_followup:
    <<: *codex_reader
    generator_max_attempts: 3

  integration:
    <<: *codex_writer
    max_followups: 8
    regression_feedback_cases: 3

  # 可省略；省略时直接向 Integration 发送“继续”。
  integration_followup:
    <<: *codex_reader
    generator_max_attempts: 3

  # 可省略；省略时不创建定时 Summary Request。
  progress_summary:
    interval: 5m
    timezone: Asia/Shanghai
    backend: codex
    model: gpt-5.6
    reasoning_effort: low
    approval_policy: never
    sandbox: read-only
    max_followups: 3
```

## 3. 校验

缺少 `baseline_alignment`、`baseline_verify`、非空 `iteration.agents` 或 `integration` 时拒绝启动。可选 Role一旦出现就必须包含完整合法 Backend 配置。

`history_limit` 是非负整数；Iteration Context 取最近这些终态 Attempt，不区分 Accepted/Rejected。`max_pending_attempts=0` 表示无限制。`regression_feedback_cases` 是 Integration 每次最多加入 Iteration Sample 的回退 Case 数。`progress_summary.timezone` 是 IANA 时区名，默认 `Asia/Shanghai`。

Backend-specific 扩展必须放入 `protocol_config`，由对应 Adapter 解释；顶层未知字段直接拒绝，不能静默忽略。Role 和领域代码不得假设 Codex/Cursor 共有模型名、reasoning effort 或 sandbox 枚举。
