# Pika v1 架构

## 1. 部署单元

一个 Pika Server 恰好管理一个仓库的一次完整 Optimization Campaign。它拥有一个 Campaign Workspace、一个 SQLite 数据库、一个 Campaign Best Branch、一组 Iteration Slots、一个 HTTP Token 和一个 Phoenix Endpoint。再次优化同一仓库也会启动另一个 Pika Server。

```text
Pika Server
├── Campaign Workspace
│   ├── repo/                  # Managed Repo 软链接或 Pika 自建 repo
│   ├── attempts/              # 活跃 Attempt worktree
│   ├── artifacts/             # patch/profile/prompt/log/plan
│   ├── pika.sqlite3
│   └── config.json
├── Phoenix Endpoint           # LiveView、JSON API、HTTP MCP
├── Campaign supervision tree
└── Agent Backend subprocesses
```

## 2. 组件关系

```mermaid
flowchart LR
    Browser[Browser] -->|Token Cookie| Web[Phoenix LiveView / JSON API]
    Web --> Domain[Campaign / Attempt / Integration / Sync domain lifecycles]
    Web --> PubSub[Phoenix PubSub]
    Domain --> DB[(SQLite WAL)]
    Domain --> Repo[Git / Workspace managers]
    Domain -->|Domain Events + runnable Agent Work| Symphony[Agent Symphony]
    Symphony --> Actors[Agent Actors]
    Actors --> Roles[Static Agent Roles]
    Actors --> Hosts[Session Hosts]
    Hosts --> Sessions[AgentBackend Sessions]
    Sessions -->|native JSON-RPC stdio| Codex[Codex App Server]
    Sessions -->|ACP v1 stdio| Cursor[Cursor ACP Server]
    Codex -->|Streamable HTTP + scoped token| MCP[Pika MCP]
    Cursor -->|Streamable HTTP + scoped token| MCP
    MCP --> Directory[Actor Directory]
    Directory --> Actors
    Roles -->|Domain Adapter| Domain
    Sessions --> Logs[Backend JSONL Artifacts]
    Repo --> Git[(Campaign Git Repo)]
    PubSub --> Browser
```

## 3. OTP 监督树

```text
Pika.Application
├── Pika.WorkspaceLock                # Managed Repo advisory lock
├── Pika.Repo                         # Ecto SQLite
├── Phoenix.PubSub
├── Pika.AgentBackendSessionSupervisor # DynamicSupervisor
├── Pika.Agent.Directory               # 活动 Work / Actor / Session Token
├── Pika.Agent.ActorSupervisor         # DynamicSupervisor
├── Pika.CampaignSupervisor            # Alignment Campaign 等动态领域进程
├── Pika.Runtime
├── Pika.Agent.Symphony                # Agent Work reconciliation
├── Pika.CampaignBootstrap
├── Pika.AttemptCoordinator            # Attempt 创建与用户控制
├── Pika.IntegrationCoordinator        # FIFO + Integration Lease
├── Pika.SyncCoordinator               # 人工触发 Sync lifecycle
├── Pika.ProgressSummaryCoordinator    # 可选定时 Request 创建器
└── PikaWeb.Endpoint
```

每个 Agent Actor 只执行一个 `Agent Role + Agent Work`，并通过内部 Session Host 持有至多一个 Backend Session。Session 对应独立受监督 Backend 进程，后者通过 `Port` 启动 provider-specific 子进程。Actor 或 Backend 崩溃只影响该项 Work；Symphony 根据领域持久状态创建新的 Actor 和新的 provider Session 恢复，不复用旧 Codex thread 或 Cursor ACP session。协议封装在 `Pika.AgentBackend` Behaviour 后，Role 与领域代码不得依赖 provider wire types。

## 4. 核心职责

### 领域 Lifecycle Modules

- 决定 Campaign、Spec Revision、Attempt、Integration、Sync 与 Progress Summary Request 的可运行资格和终态。
- 执行 FIFO、Lease、Git、Metric、停止条件与 Pause/Stop/Resume 等领域规则。
- 将可执行实体投影为稳定的 Agent Work；不直接决定 Backend Session 生命周期。
- 只在 SQLite 事务成功后广播 Domain Event。

### Agent Role Runtime

- 内置静态 Role 定义职责、`automatic | await_user_kickoff` 启动方式、MCP tool catalog、Profile key、Instructions 构造与 completion graph。
- 所有 Role 实现相同的 `definition`、`build_system_instructions`、`initial_prompt` 和 `recovery_prompt` interface。
- Role 只从 Domain Adapter 读取 committed facts；completion graph 只投影终态和下一组必需操作，不复制领域状态机。
- Workspace 可用 `prompts/roles/<role>.md` 添加受限变量模板，但不能新增 Role、扩大工具权限或覆盖固定安全约束。

### Agent Symphony 与 Actor

- Symphony 在启动、Domain Event 和周期核对时读取 runnable Agent Work，并保证每项 Work 至多一个活动 Actor。
- Symphony 自身重启时从 Directory 接管仍存活的 Actor；进程已消失时从 SQLite Work identity 创建恢复 Actor。
- Pause 保留在途 Actor 与 Session；Stop 和失去领域资格会停止 Actor。技术容量限制不改变领域 eligibility。
- Actor 冻结本 Session 的 Role contract、Profile、Instructions 与 tool catalog，并把 provider events 写入审计日志。
- 恢复总是重新读取最新 `pika.yaml` 和 Workspace Role 模板，创建新 Session 与新 Token；同一 Session 内配置不热切换。
- 已完成迁移的 Role 固定由 Actor 执行；Alignment/Setup Merge/Baseline 暂留内部 ownership selector，以便旧 `boundary` Workspace 逐角色滚动恢复。

### Session Host 与 Directory

- Session Host 是 Actor 内部的深模块，封装 Backend 启动、Token、Session audit、事件游标、Instructions Artifact、interrupt 与 close。
- Directory 是活动运行身份的内存权威，将随机 Bearer Token 绑定到 Actor、Role、Agent Work 与 Session；明文 Token 不进入 SQLite。
- 统一 Pika MCP 从 Directory 定位 Actor，并始终使用 Actor 启动时冻结的 tool catalog。
- 写操作按 `Campaign + Role + Work kind/id + operation + idempotency_key` 去重，因此替换 Session 后仍可安全重放。

### RepoManager

- 校验 Managed Repo 所有权、canonical path、clean 状态和实际 Git HEAD。
- 创建 setup/attempt/sync worktree 与分支。
- 在 Agent Git 操作后验证父提交、protected path、trailers 和实际 HEAD。
- 不替代编码 Agent解决冲突或执行 Merge。

### AgentBackend Session

- 通过 `Pika.AgentBackend` 打开全新的 provider Session、自动批准权限，并注入 Pika HTTP MCP、Skill roots 与 Role 构造的 Agent Instructions。
- 将 provider 原始消息顺序写入 JSONL，并把标准化 Backend Event 广播到 PubSub。
- Backend Turn 结束但缺少必需 MCP 调用时，根据 committed facts 发送 completion follow-up；Session 中断后由新 Actor 继续同一 Work。
- Attempt Guidance 调用统一 `steer`：Codex 映射到 `turn/steer`，Cursor 映射到 cancel + follow-up Prompt。
- Stop Now 调用统一 `interrupt`：Codex 映射到 `turn/interrupt`，Cursor 映射到 `session/cancel`。

`Pika.AgentBackend` 固定接口：

```elixir
start_link(profile, event_sink)
open_session(cwd, model, reasoning_effort, mcp, skill_roots)
start_turn(session, input)
steer(session, input)
interrupt(session)
close_session(session)
capabilities(session)
```

Codex adapter 通过 `initialize → initialized → thread/start → turn/start` 驱动 App Server，将 `item/*`、`turn/*` 通知转换为标准事件。Cursor adapter 驱动 ACP `initialize → session/new → session/prompt`，把 `session/update` 转换为同一事件集合。adapter 仍可实现 provider-native resume 能力，但 Agent Work 恢复策略不调用它。标准事件只表达会话、Turn、消息、Plan、工具、命令、文件、usage、完成与错误，不携带 provider wire struct。

Codex 每个 Session 通过进程级 config override 注入 Pika MCP URL、Bearer Token 环境变量和 `required=true`，并用 `skills/extraRoots/set`/`skills/list` 加载 Skill。Cursor 则通过 ACP Session 配置注入 MCP 与 Skill roots。

### IntegrationCoordinator

- FIFO 串行处理准备归并的 Attempt。
- 原子签发绑定 Backend Session 与进程的 Integration Lease。
- 检查陈旧 Base，要求 Agent刷新并重新进行正式配对测量。
- 在 Git mutation 前校验全量正确性、5 Pair Screening、异常组合的 Campaign Spec 正式 Pair 测量和 Full Regression Receipt。
- 回退候选拒绝并推进 Sampling Revision；通过候选才允许创建 Intent 和调用 `complete_merge`。
- 在 `complete_merge` 后联合核验 Receipt、SQLite Intent、Git 与全量 Metrics。

### SyncCoordinator

- 用户确认 remote、branch 和待 Push commit 后启动。
- 关闭新 Attempt dispatch，在临时 Sync Branch 上 merge、验证、push、推进 Best。
- 持久化 Sync Intent，支持“远端已成功、本地尚未推进”崩溃窗口恢复。

## 5. 数据权威

| 数据 | 权威来源 |
|---|---|
| 代码、分支、提交 | Git |
| Campaign/Attempt/Session 状态 | SQLite 当前状态表 |
| 最新结构化 Metrics | SQLite `attempt_metrics` / `best_metrics` |
| 状态时间线与待广播事实 | SQLite `domain_events` |
| Patch、Plan、Profiler、Prompt、Backend 原始输出 | Artifact Workspace |
| 活动 Actor、Work 与明文 MCP Token 绑定 | `Pika.Agent.Directory`，进程重启后重建 |
| 实时 UI 更新 | Phoenix PubSub，非持久权威 |

恢复顺序固定为：SQLite 当前状态与 Operation Intent → Git 事实 → Artifact 完整性与 JSONL 尾部 → 重新启动所需 Agent。任何不可解释的不一致进入 Blocked，禁止危险推进。

## 6. Attempt 正常路径

```mermaid
sequenceDiagram
    participant D as Attempt/Integration lifecycle
    participant S as Agent Symphony
    participant A as Iteration Actor
    participant M as Pika MCP
    participant I as Integration Actor
    participant G as Git

    D->>S: runnable Iteration Agent Work
    S->>A: start Actor + fresh Backend Session
    A->>M: query_attempt_history / get_context
    A->>G: 修改、测试、提交
    A->>M: record_metrics + submit_attempt_summary
    A->>M: complete_attempt
    M->>D: commit ready_for_integration + Domain Event
    D->>S: runnable FIFO Integration Work
    S->>I: start Actor + fresh Backend Session
    I->>M: acquire_integration_lease(expected_best_sha)
    I->>G: 刷新 Base、移除 ref、正式配对测量、squash merge
    I->>M: complete_merge
    M->>D: 事务提交 Accepted/Rejected + Domain Event
    D-->>S: BestAdvanced（至少一次）
```

## 7. 安全边界

HTTP Token 只保护单租户网站入口，不隔离本机 Agent。Agent 默认 YOLO 并被视为受信任进程；凭证来自进程环境，不能进入 Prompt、SQLite 或应用日志。Pika 通过 protected paths、正式 Harness、Git 核验和角色化 MCP 保护优化结果，但不承诺防御恶意 Agent 对主机的破坏。
