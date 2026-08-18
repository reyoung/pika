# Pika v1 实现与验收计划

> 本文是总览。可以依次执行的任务清单位于 [`docs/v0/phases/`](../v0/phases/README.md)。

## 1. 原则

- 先验证 Agent Backend 协议、Pika MCP 与崩溃边界，再实现完整优化循环。
- 每个阶段交付可运行、可恢复的纵向切片，不用 mock 掩盖关键外部协议。
- Kernel 性能不是 Pika 自身单元测试的稳定前提；调度与 Git 测试使用确定性 Fake Harness，最终另设真实 GPU E2E。
- 未通过阶段门禁不得把后续 UI 演示当作完成。

## 2. Phase 0：Agent Backend 协议 Spike

目标：证明 Elixir `Pika.AgentBackend` 可以分别通过 Codex 原生 App Server 和 Cursor ACP 驱动两个 Backend，并输出统一事件。

交付：

- `Pika.AgentBackend` Behaviour 与统一 Backend Event。
- `Pika.AgentBackend.CodexAppServer`：`codex app-server --listen stdio://`、thread/turn、steer、interrupt、skills 与 schema 证据。
- `Pika.AgentBackend.CursorACP`：ACP initialize/session/prompt/cancel；仅在 capability 广告时调用 session/close，否则使用独立进程 close fallback。
- 两种 Backend Session 注入本地 Streamable HTTP MCP 并实际调用同一个角色化工具。
- `ncu-report-skill` 固定 SHA 后对两个 Backend 可见的最小验证。

门禁：Codex/Cursor 各自协议测试与统一 conformance suite 全通过；任何缺失 capability 有明确 adapter 行为，领域层不出现 provider 分支。

## 3. Phase 1：Workspace、启动与持久状态

交付：

- Elixir Release、`pika serve` CLI、YAML 解析和 `config.json` 快照。
- Owned Repo / Managed Repo 软链接与 advisory lock。
- SQLite migrations、Artifact Store、Domain Event outbox。
- 256-bit 启动 Token、Cookie exchange、Phoenix/LiveView shell。
- 启动 preflight 和单例 Campaign 初始化/恢复。

门禁：kill -9 后重启，Workspace identity、数据库和 Git 状态无损；两个 Server 不能管理同一 Managed Repo；旧 HTTP/MCP Token 失效。

## 4. Phase 2：Alignment、Spec 与 Baseline

交付：

- Alignment Conversation Backend Session。
- Boundary Role MCP、Spec diff/确认 UI、Spec Revision。
- Reference Catalog 16 项默认全选、Campaign 初始化时解析最新 HEAD 并固定 SHA。
- Skill Registry 与 `ncu-report-skill` 固定 SHA。
- setup worktree、protected Harness digest、Baseline、Profiler Artifact 和噪声估算。

门禁：未确认 Spec 不能进入 Baseline；修改 protected path 的候选必拒绝；Ref/Skill 在重启和新 Attempt 间 SHA 不漂移。

## 5. Phase 3：并发 Attempt 与 MCP 完成协议

交付：

- 显式 Iteration Slots 和 per-slot Agent Profile。
- Attempt worktree、临时 Ref submodule、最近 10 次历史注入、Agent Mailbox。
- Plan 可选且默认关闭；`plan.md` 位于 Artifact Workspace。
- Iteration Role MCP、配对 Benchmark parser、Summary/Patch/Profiler 登记。
- 缺失 MCP 操作的无限 follow-up 与新 Session 恢复。

门禁：三个 Fake Agent 并发运行互不污染；Agent 正常结束但漏报 Metric 时不会误完成；进程多次崩溃仍继续同一 Attempt 且不多计预算。

## 6. Phase 4：Integration、BestAdvanced 与 Mainline

交付：

- FIFO Integration Queue、无 TTL Integration Lease、Operation Intent。
- 陈旧 Base refresh/rebase、正式 Pareto 门禁、Agent squash merge 与 Git 独立核验。
- BestAdvanced 至少一次投递和正式动作前 stale-base gate。
- 可选 Mainline Validation、Metric 覆盖、RevertRequired 与最新 Best revert。
- 终态 Attempt Artifact 后自动 worktree/branch 清理。

门禁：

- 两个 Attempt 同时完成时只有一个推进 Best，另一个必须刷新。
- 在 merge 前、commit 后、数据库事务前后逐点 kill -9，恢复不重复 Merge。
- Mainline 迟到失败时，即使后续 Commit 已存在，也由 Agent在最新 Best 上创建可验证 revert commit。

## 7. Phase 5：Sync、停止与完整 UI

交付：

- 人工 Sync 确认、临时分支、fetch/merge/validate/push/local advance。
- Sync Intent、Sync Trail、远端成功/本地未推进恢复。
- Pause、Stop Now、Resume、Blocked、Draining 与 Completed。
- 已确认的 Alignment、Attempt/BTW、Metrics Timeline LiveView。
- Metrics hover 显示原始 Metrics、相对改善、状态和 Attempt Summary。

门禁：Sync 期间无新 Attempt；Push 失败 Best 不变；远端成功后 kill -9 能幂等推进本地；Spec/Harness 变化进入 AwaitingSpecConfirmation。

## 8. Phase 6：真实 GPU E2E

至少完成一个 NVIDIA Campaign：

1. 用户在 Alignment 确认 PyTorch Reference、多个 Cases、Metrics 与 Ref 选择。
2. Baseline 正确性、Profiler 和噪声估算成功。
3. 至少两个不同 Agent Backend 并行运行 Attempts。
4. 至少一个 Accepted、一个 Rejected，并验证 Patch、Summary、Metrics 与 worktree 清理。
5. `ncu-report-skill` 在两个 Backend Session 可读，并生成登记的 Profiler Artifact。
6. 服务中途重启后自动恢复。
7. 手工 Sync 完成 pull、验证、push、Metrics 更新和 Sync Trail。

真实 GPU E2E 必须保留命令、退出状态、可见 workload 输出、Metrics 与清理证据；仅提交 Agent 或仅准备环境不算通过。

## 9. 测试矩阵

### 单元/性质测试

- Campaign/Attempt/Sync 状态转换和非法转换。
- Pareto 门禁、MAD 公式、Pair 有效数与方向统一。
- Path normalization、protected digest 和 Artifact SHA。
- Prompt 最近 N 次裁剪、Guidance 优先级和 Mailbox 至少一次投递。
- Token hashing、Role 工具矩阵和 idempotency conflict。

### 集成测试

- SQLite 事务 + PubSub outbox 顺序。
- Fake Agent Backend 的标准事件、权限、steer、interrupt、漏报和崩溃。
- Git worktree、submodule 注入/移除、squash、rebase、revert 和冲突恢复。
- Managed Repo advisory lock 与软链接替换检测。
- HTTP Token 对 HTML、JSON、LiveView、SSE/WebSocket、MCP 的完整保护。

### 故障注入

- Agent 在工具调用、Git mutation、MCP 完成前后崩溃。
- Pika 在 SQLite commit 前后、remote push 后、本地 Best 前崩溃。
- JSONL 尾部半行、Artifact 丢失/哈希不符、SQLite/Git SHA 不一致。
- Integration Agent 永不调用 MCP：系统持续运行，直到用户 Stop。

## 10. v1 完成定义

只有同时满足以下条件才算 v1 完成：

- 所有 accepted ADR 的核心契约有自动化测试。
- Codex App Server、Cursor ACP 与统一 HTTP MCP conformance 通过。
- SQLite migration、备份/恢复和三源权威核对通过。
- Git 并发、crash recovery、Mainline revert 和 Sync E2E 通过。
- UI 与已确认原型一致，Metrics hover 包含 Summary。
- 真实 GPU Campaign 按 Phase 6 全流程通过。
- 文档中所有“待决问题”清零，发布配置不使用未固定依赖或 `latest` 容器标签。

## 11. 不在 v1 实现

- 多租户、RBAC、计费和跨 Server 调度。
- Pika GPU Worker、远程节点注册、心跳或 GPU RPC。
- 未实现 `Pika.AgentBackend` contract 的 Agent、Cursor ACP v2、provider resume 正确性依赖。
- S3 Artifact、自动 Push、内置 daemon、Kubernetes/Temporal。
- 默认 Plan、默认 Mainline Validation 或默认 Plateau 停止。
