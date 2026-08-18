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
└── ACP Agent subprocesses
```

## 2. 组件关系

```mermaid
flowchart LR
    Browser[Browser] -->|Token Cookie| Web[Phoenix LiveView / JSON API]
    Web --> Coordinator[Campaign Coordinator]
    Web --> PubSub[Phoenix PubSub]
    Coordinator --> DB[(SQLite WAL)]
    Coordinator --> Repo[Repo Manager]
    Coordinator --> Slots[Iteration Slot Supervisor]
    Coordinator --> Integration[Integration Coordinator]
    Coordinator --> Validation[Mainline Validation Coordinator]
    Coordinator --> Sync[Sync Coordinator]
    Slots --> ACP[ACP Client Sessions]
    Integration --> ACP
    Validation --> ACP
    Sync --> ACP
    ACP -->|stdio ACP v1| Backends[Codex / Cursor ACP Servers]
    Backends -->|Streamable HTTP + scoped token| MCP[Pika MCP]
    MCP --> DB
    ACP --> Logs[Agent JSONL Artifacts]
    Repo --> Git[(Campaign Git Repo)]
    PubSub --> Browser
```

## 3. OTP 监督树

```text
Pika.Application
├── Pika.Repo                         # Ecto SQLite
├── Phoenix.PubSub
├── PikaWeb.Endpoint
├── Pika.WorkspaceLock                # Managed Repo advisory lock
├── Pika.CampaignSupervisor
│   ├── Pika.CampaignCoordinator      # Campaign 状态与 dispatch gate
│   ├── Pika.RepoManager              # Git 事实核对，不代替 Agent merge
│   ├── Pika.ArtifactStore
│   ├── Pika.AgentSessionSupervisor   # DynamicSupervisor
│   ├── Pika.IterationSlotSupervisor  # 固定显式 Slots
│   ├── Pika.IntegrationCoordinator   # FIFO + Integration Lease
│   ├── Pika.ValidationCoordinator    # 单并发、默认关闭
│   └── Pika.SyncCoordinator          # 人工触发
└── Pika.Telemetry
```

Agent Session 是独立受监督进程，内部通过 `Port` 启动一个 ACP v1 Server。Agent 崩溃只影响对应 Session；CampaignCoordinator 根据 SQLite 和 Git 事实启动恢复 Session。ACP 库封装在 `Pika.ACPClient` Behaviour 后，领域代码不得依赖具体 0.x 包的数据类型。

## 4. 核心职责

### CampaignCoordinator

- 执行 Campaign 状态机与停止条件。
- 为闲置 Iteration Slot 创建 Attempt，不超过 `max_attempts`。
- 处理 Pause、Stop、Resume、BestAdvanced 和 Spec Revision。
- 只在 SQLite 事务成功后广播 Domain Event。

### RepoManager

- 校验 Managed Repo 所有权、canonical path、clean 状态和实际 Git HEAD。
- 创建 setup/attempt/sync worktree 与分支。
- 在 Agent Git 操作后验证父提交、protected path、trailers 和实际 HEAD。
- 不替代编码 Agent解决冲突或执行 Merge。

### AgentSession

- 协商 ACP v1 capabilities，自动批准权限，创建 Session 并注入 Pika HTTP MCP。
- 将 ACP 原始事件顺序写入 JSONL，并向 PubSub 广播实时更新。
- Prompt Turn 结束但缺少必需 MCP 调用时，在同一 Session 无限 follow-up。
- Attempt Guidance 触发 `session/cancel` 后用同一 Session 发送新 Prompt。

### IntegrationCoordinator

- FIFO 串行处理准备归并的 Attempt。
- 原子签发绑定 Agent Session 与进程的 Integration Lease。
- 检查陈旧 Base，要求 Agent刷新并重新进行正式配对测量。
- 在 `complete_merge` 后联合核验 SQLite Intent、Git 与 Metrics。

### ValidationCoordinator

- 默认关闭；启用后逐个固定到对应 squash SHA 复验。
- 不阻塞正常 Integration 队列。
- 失败时发布 RevertRequired，并由 Mainline Agent 在最新 Best 上创建 revert commit。

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
| Patch、Plan、Profiler、Prompt、ACP 原始输出 | Artifact Workspace |
| 实时 UI 更新 | Phoenix PubSub，非持久权威 |

恢复顺序固定为：SQLite 当前状态与 Operation Intent → Git 事实 → Artifact 完整性与 JSONL 尾部 → 重新启动所需 Agent。任何不可解释的不一致进入 Blocked，禁止危险推进。

## 6. Attempt 正常路径

```mermaid
sequenceDiagram
    participant C as CampaignCoordinator
    participant A as Iteration Agent
    participant M as Pika MCP
    participant I as Integration Agent
    participant G as Git

    C->>A: ACP session/new + Attempt Prompt
    A->>M: query_attempt_history / get_context
    A->>G: 修改、测试、提交
    A->>M: record_metrics + submit_attempt_summary
    A->>M: complete_attempt
    C->>I: 启动 Integration Session
    I->>M: acquire_integration_lease(expected_best_sha)
    I->>G: 刷新 Base、移除 ref、正式配对测量、squash merge
    I->>M: complete_merge
    M->>C: 事务提交 Accepted/Rejected + Domain Event
    C-->>A: BestAdvanced（至少一次）
```

## 7. 安全边界

HTTP Token 只保护单租户网站入口，不隔离本机 Agent。Agent 默认 YOLO 并被视为受信任进程；凭证来自进程环境，不能进入 Prompt、SQLite 或应用日志。Pika 通过 protected paths、正式 Harness、Git 核验和角色化 MCP 保护优化结果，但不承诺防御恶意 Agent 对主机的破坏。
