# Pika v2 配置

## 1. 原则

- 配置没有 Agent Profile 或 Profile registry。
- 每个 Agent位置都展开一个主 Backend Endpoint 和可选的有序 `fallbacks`；YAML anchor/alias 只属于 YAML 语法，Pika 接收解析后的普通值。
- `iteration.agents` 的顶层元素数量就是 Iteration 并发度；fallback Endpoint 不增加并发度，也不能再嵌套 `fallbacks`。
- 其他已配置 Role 的并发度固定为 1；不同 Role 可以并行。
- Follow-up 与 Progress Summary Role 可省略。
- Session 创建时冻结当前完整配置；更新只影响以后创建的 Session。
- Codex 默认使用 `danger-full-access`，Cursor 默认关闭 sandbox；只有用户显式配置时才启用受限 sandbox。
- `cursor_headless` 使用本机已登录的 `cursor-agent -p`，不要求 API key。Headless 每个 turn 启动独立进程，在同一 Pika Session 内用 Cursor chat id 延续上下文；断流不会自动重发 prompt。
- Cursor ACP 已软退役。`pika init`、reconfiguration 和 fallback 向导只允许 Codex 与 Cursor Headless，并拒绝 `--backend cursor|cursor_acp`。旧 YAML 的 `cursor|cursor_acp` 仍按 ACP 加载、运行并记录 warning；重新输出时规范化为显式 `cursor_acp`，访问该槽位进行 reconfiguration 时必须迁移。
- 根级 `token` 是 Web/API 访问 token。新 Workspace 默认生成随机 256-bit token；也可在初始化时固定。`serve` 在启动时读取它，手工修改后需要重启。旧配置省略该字段时，每次 `serve` 启动仍临时生成随机 token。

## 2. 交互式配置

`pika init` 默认逐一配置每个 Agent 位置。每个必选 Role、每个 Iteration Agent，以及
启用的可选 Follow-up/Progress Summary Role 都独立选择 Backend、该 Backend 动态返回的
完整模型列表、`low|medium|high|xhigh|max|ultra` reasoning effort 和 fallback Endpoint 数量。`--yes` 用于脚本化的
非交互初始化。向导生成随机 token 作为默认值，也接受用户输入的固定 token；包含 token 的
Workspace `pika.yaml` 以 `0600` 权限写入。

Workspace 创建后，可在其目录运行 `pika reconfiguration`（或 `pika reconfigure`）。命令
可修改一个 Role 或全部 Agent；对于 Iteration，会逐个修改展开后的 Agent。写入前会重新
校验候选 `pika.yaml`。运行时为每个新 Session 重新读取该文件，已有 Session 的冻结配置不变。

## 3. 完整示例

```yaml
version: 2

repo: /path/to/repo
workspace: /path/to/workspace
token: replace-with-a-long-random-token

agents:
  baseline_alignment: &codex_writer
    backend: codex
    model: gpt-5.6
    reasoning_effort: high
    approval_policy: never
    sandbox: danger-full-access

  baseline_verify:
    <<: *codex_writer
    max_followups: 8

  # 可省略；省略时直接向 Baseline Verify 发送“继续”。
  baseline_verify_followup: &codex_reader
    backend: codex
    model: gpt-5.6
    reasoning_effort: medium
    approval_policy: never
    sandbox: danger-full-access
    generator_max_attempts: 3

  iteration:
    history_limit: 20
    max_followups: 5
    max_pending_attempts: 0
    agents:
      - <<: *codex_writer
      - backend: cursor_headless
        model: gpt-5.6-sol-high
        reasoning_effort: high
        approval_policy: force
        sandbox: disabled

  # 可省略；省略时直接向 Iteration 发送“继续”。
  iteration_followup:
    <<: *codex_reader
    generator_max_attempts: 3

  integration:
    <<: *codex_writer
    fallbacks:
      - backend: cursor_headless
        model: gpt-5.6-sol-high
        reasoning_effort: high
        approval_policy: force
        sandbox: disabled
      - backend: codex
        model: another-model
        reasoning_effort: medium
        approval_policy: never
        sandbox: danger-full-access
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
    sandbox: danger-full-access
    max_followups: 3
```

## 4. 校验

缺少 `baseline_alignment`、`baseline_verify`、非空 `iteration.agents` 或 `integration` 时拒绝启动。可选 Role一旦出现就必须包含完整合法 Backend 配置。

`history_limit` 是非负整数；Iteration Context 取最近这些终态 Attempt，不区分 Accepted/Rejected。`max_pending_attempts=0` 表示无限制。`regression_feedback_cases` 是 Integration 每次最多加入 Iteration Sample 的回退 Case 数。`progress_summary.timezone` 是 IANA 时区名，默认 `Asia/Shanghai`。

Backend-specific 扩展必须放入 `protocol_config`，由对应 Adapter 解释；顶层未知字段直接拒绝，不能静默忽略。Role 和领域代码不得假设 Codex/Cursor 共有模型名、reasoning effort 或 sandbox 枚举。

### Backend failover

Backend Adapter 把失败归一化为 `capacity_exhausted`、`authentication_failed`、`context_exhausted`、`session_budget_exhausted`、`transient`、`fatal` 或 `unknown`。只有账户容量耗尽和认证失效排除当前 Endpoint 并推进 chain；其他失败仍重启当前第一个可用 Endpoint。

排除记录按 `Role + Work kind/id + chain SHA-256 + endpoint index` 持久化。容量失败携带 `retry_at` 时到期自动恢复；没有 reset 时间的容量失败及认证失败等待 UI 的 **Retry backend chain** 或配置变化。chain 内容变化会产生新 digest 并从主 Endpoint 开始；旧记录保留审计。所有 Endpoint 都不可用时 Work 保持 runnable-but-blocked，Symphony 不反复创建 Actor。

Codex 只根据失败终态分类。`error.willRetry=true`、`account/rateLimits/updated` 和 `thread/tokenUsage/updated` 都不会单独推进 chain；rate-limit reset 只用于给之后真实的 `UsageLimitExceeded` 填入 `retry_at`。Cursor Headless 只把 `cursor-agent status` 明确的未登录结果分类为认证失败；运行期非零退出保持 `unknown`，不匹配 `stderr` 文案。成功 result 的可选 `usage` 只作为计量事件转发。

协议依据：[Codex app-server](https://developers.openai.com/codex/app-server/)；[Cursor CLI output format](https://docs.cursor.com/en/cli/reference/output-format)。

每次 failover 都结束当前 Pika/Backend Session，并为同一 Agent Work 创建全新 Session。新 Session 保留 Git workspace、Conversation Journal 与 Recovery Context，不恢复旧 provider session，也不把失败的 provider prompt 自动重发到原 Session。
