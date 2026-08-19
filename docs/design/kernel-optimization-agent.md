# Pika Kernel 自动调优系统设计

## 状态

v1 设计已于 2026-08-18 冻结，并由 ADR-0028 修订 Agent Backend 协议。尚未开始产品实现；`docs/design/prototype/` 仅为已确认的 UI 交互原型。

## 目标

Pika 是一个常驻 HTTP 服务。它协调 Codex、Cursor 等外部编码 Agent，从 PyTorch 参考实现和性能目标出发，持续生成、验证、衡量并筛选 Kernel 优化候选，直到达到性能上限或耗尽迭代预算。

## 已确认的产品边界

- v1 是单租户、自托管服务。
- 一个 Pika Server 对应一个 Git 仓库的一次完整优化过程，也就是恰好一个 Campaign。
- 用户对同一或不同仓库发起另一次完整优化时，必须启动另一个 Pika Server。
- Pika Server 从启动时的仓库 HEAD 建立 Campaign Best Branch；Iteration 不修改启动前的原分支。
- 服务启动时生成一个随机访问 Token；只有持有该 Token 的客户端才能访问 HTTP 网站。
- 用户界面至少包含对话界面与 Metrics 曲线界面。
- 调优任务有持久状态；服务或机器重启后必须能识别未完成工作并继续推进。
- 调优既可以基于已有 Git 仓库，也可以从空仓库开始。
- v1 假设 Agent 运行环境能够使用当前机器上的 GPU，不提供远程 GPU Worker。
- Pika 之后也不计划拥有独立 GPU Worker；如果 Agent 需要在其他 GPU 环境执行，应由 global skill 指导 Agent 完成，对 Pika 仍表现为 Agent 会话内部行为。

## 已确认的调优流程

1. 用户与 AI 共同定义计算边界、PyTorch 参考实现、拟融合范围、正确性要求、测试 Shapes 和性能指标。
2. 用户确认性能目标或候选尝试预算。
3. 系统循环执行有边界的候选尝试，直到满足停止条件。
4. 每个候选尝试使用独立 Git worktree，并在尝试期间积极提交代码。
5. 候选尝试只有在至少一个目标指标改善超过 1%，且其他指标没有下降时才被接受；否则拒绝。
6. 已接受尝试 squash merge 回最佳已知版本；无论接受或拒绝都持久记录 Patch、描述、总结和 Metrics 变化。
7. 新计划获得最近 N 次尝试的描述、总结和 Metrics 变化；完整历史通过 MCP 查询。
8. 参考项目以 Git submodule 形式出现在候选工作区的 `ref/` 中，但不得归并到产品代码主线。

## 已确认的 Campaign Spec 生命周期

- 优化开始前必须显式冻结带版本号的 Campaign Spec，状态依次为 `DraftingSpec → AwaitingConfirmation → BuildingBaseline → Optimizing`。
- Campaign Spec 至少包含 PyTorch 计算语义、输入/dtype/layout、正确性容差、Fusion 边界、Shapes、目标与保护 Metrics、停止条件和 Benchmark 协议。
- 只有用户明确确认后，系统才能离开 `AwaitingConfirmation` 并建立 Baseline。
- 优化开始后，任何改变计算语义、Shapes、Metric、Benchmark 或正确性要求的全局指导都创建新 Spec Revision，并重新生成 Baseline 与噪声估计。
- 不同 Spec Revision 的 Metrics 不得直接连成同一条可比较曲线。

## 已确认的 Reference 与 Harness 边界

- Boundary Agent 在独立 setup worktree 中创建或完善 PyTorch Reference、正确性测试和 Benchmark Harness。
- 用户确认 Campaign Spec 后，这些文件由 Agent Merge 到 Campaign Best Branch，之后才开始 Baseline。
- Campaign Spec 记录受保护文件的路径与内容哈希。Iteration Agent 可以读取和执行，但不得修改。
- 候选 Patch 修改任一受保护文件时直接拒绝；修正 Reference 或 Harness 必须创建新 Spec Revision。

## 已确认的可选 Plan 阶段

- Plan 是用户可配置的可选阶段，默认关闭。
- 启用时，Plan Agent 调用 `submit_plan(markdown, summary)`；Pika 原子写入 `Artifact Workspace/campaigns/<campaign-id>/attempts/<attempt-id>/plan.md`，并在 SQLite 中保存相对路径和摘要。
- 后续 Agent 通过 MCP Resource 或 Backend 支持的嵌入资源读取 `plan.md`。
- `plan.md` 不位于 Git worktree，不能进入候选的 squash patch，也不能归并到 Campaign Best Branch。
- 未启用 Plan 时，Iteration 开发 Agent 直接进入实现，不要求先生成结构化 Plan。

## 已确认的参考仓库注入

- Pika 架构参考与 Attempt Reference Catalog 完全无关，不能作为 Kernel Ref 注入。
- Reference Catalog 的初始集合与 Atrex Kernel Agent `reference-projects/` 一致，共 16 项：`cutlass`、`cutex`、`cuLA`、`flash-attention`、`flashinfer`、`FlyDSL`、`triton`、`DeepGEMM`、`LeetCUDA`、`FlashMLA`、`composable_kernel`、`cute-gemm`、`hpc-ops`、`aiter`、`quack`、`tilelang`。
- Alignment UI 允许用户逐项选择 Ref，默认 16 项全部选中。
- Campaign 的 Reference Catalog 为每个选中仓库记录 URL、版本、`ref/` 目录名和简介；这些信息会注入 Agent Prompt。
- setup 和 Iteration worktree 使用 Git submodule 把所有选中仓库临时注入 `ref/<name>`。
- Pika 注入的 `ref/**` 以及对应 `.gitmodules` 增量属于禁止交付区域；用户仓库原有 submodule 必须保持不变。
- 进入归并队列后，编码 Agent 必须移除 Pika 注入的 submodule、恢复基础版本 `.gitmodules`，再重新运行正确性和 Benchmark。
- 候选不得把参考仓库变成构建或运行依赖；移除参考仓库后不能通过验证的候选必须拒绝。

## 已确认的 Skill 注入

- Skill Registry 与 Reference Catalog 分离；Skill 不是 `ref/` submodule。
- Pika Skill Registry 初始增加 `https://github.com/mit-han-lab/ncu-report-skill`。
- `ncu-report-skill` 原样提供给 Agent，不添加 Pika 自定义硬件适用范围 Prompt；Agent 按 Skill 自身说明判断通用流程与 B200/sm_100 专属内容。
- Skill 文件、helpers、reference docs 和生成的 Skill 安装入口不得进入候选 squash patch 或 Campaign Best Branch。
- Pika Server 首次初始化 Campaign 时，获取全部选中 Ref 与 Skill 默认分支的最新 HEAD，并把解析出的完整 commit SHA 固定到该 Campaign。
- 服务重启、普通 Sync 和后续 Attempts 不刷新 Ref/Skill；新建另一个 Pika Server 时重新获取最新版本。

## 已确认的 Campaign Workspace 与 Git 布局

- Workspace 固定包含 `repo/`、`attempts/`、`artifacts/plans/`、`artifacts/patches/`、`artifacts/profiles/`、`artifacts/prompts/`、`artifacts/logs/`、`pika.sqlite3` 和 `config.json`。
- 用户显式提供本地仓库时，`workspace/repo` 是指向该仓库的软链接，并假设该仓库由当前 Pika Server 独占管理。
- Managed Repo 首次启动要求 working tree 与 index 干净。Pika 在 Git common directory 中创建带 Server UUID、PID、启动时间和 Workspace canonical path 的锁文件，并在进程生命周期内持有 OS advisory lock。
- 另一个 Pika Server 无法取得该 advisory lock 时必须拒绝启动；崩溃后的锁释放依赖 OS，而不是仅凭 PID 文件猜测。
- 恢复 Managed Repo 时校验软链接 canonical path、设备/inode 和 Git common directory，防止软链接被替换或指向其他仓库。
- Pika 自己获取仓库或从零开始时，`workspace/repo` 是独立的实际目录。
- 空 Workspace 初始化新 Campaign；包含合法 `config.json` 与 SQLite 的 Workspace 执行恢复；其他非空目录拒绝启动。
- Campaign Best Branch 使用 `pika/best`，Setup Branch 使用 `pika/setup/<spec-revision>`，Attempt Branch 使用 `pika/attempt/<attempt-id>`。
- Attempt Agent 可以在自己的分支中多次提交。归并 Agent 基于最新 `pika/best` 重放候选、移除临时 `ref/`、完成正式配对测量后创建 squash commit。
- Squash commit 必须包含 `Pika-Attempt-Id` 和 `Pika-Spec-Revision` trailer；Pika 在 MCP 完成前验证新 HEAD、父提交、受保护路径与 trailer。
- Attempt 结束后，Pika 先持久化 Patch、Summary、Metrics、Profiler 路径、Agent 日志和 commit 信息；事务成功后移除 worktree 并删除临时分支。
- `Interrupted`、`Incomplete` 与 `Blocked` Attempt 不自动清理；`retain_attempt_worktrees=true` 可以保留已结束 worktree，默认关闭。

## 已确认的并发与归并边界

- 一个调优任务允许多个候选尝试并行执行。
- 用户只配置 Iteration Agent 并发度；Pika 不暴露全局并发度或 Boundary、Plan、Integration 等其他角色的并发参数。
- Iteration 开发 Agent 可以自由运行 Benchmark；Pika 不提供独立 Benchmark Lease 或 Benchmark 并发限制。
- 并发候选尝试的归并有明确先后顺序；归并阶段只有一个并发。
- 排队期间最佳已知版本发生变化的候选，必须由编码 Agent 在最新版本上重放、解决冲突并重新运行正确性与 Metrics 测试。只有相对最新版本仍满足接受条件时才能归并。
- 已接受尝试由编码 Agent 自动 squash merge 到本次服务唯一的 Campaign Best Branch；源仓库启动时的原分支不被 Iteration 修改。
- Integration 串行取得 Lease 后，必须在任何 Git mutation 前完成归并前全量回归；失败候选直接拒绝，不会短暂进入 Campaign Best Branch。
- 初始 Baseline 对 Full Case Set 完成 30 Pair；Baseline Agent 自动选择最多十个 Case 形成首个 Sampling Revision。
- 日常 Attempt 只要求其启动 Sampling Revision 的正式 Metrics；Sampling Advanced 通知活动 Agent，但不强迫已运行 Attempt 返工。
- Integration 对全量 Case/Metric 做 5 Pair 筛查；中位数回退超过当前 Best noise tolerance 或样本无效的组合独立重跑 30 Pair。
- 任一 Case/Metric 经 30 Pair 确认回退都拒绝候选，包括 Iteration 阶段的 Informational 项。
- Integration Agent 从确认回退且尚未采样的 Case 中选择代表项，Pika 校验后自动追加到新 Sampling Revision。同一 Spec Revision 内采样成员只增不减。
- Pika 不为此建立 GPU Worker、远程执行协议或节点资源模型。

## 已确认的性能判定原则

- 接受条件必须容忍 Benchmark 测量噪声，而不是按聚合值要求严格零退化。
- 每个 Benchmark Case 由具体 shape、dtype、layout、输入分布和可选线上频率权重定义；一个 Case 必须对应一次确定性 Harness 调用。
- Iteration 阶段至少一个采样 Target Case 的目标 Metric 必须真实改善，采样 Guard Case 不得退化超过噪声容忍值。
- 线上频率权重只用于综合评分、候选排序和 UI，不能用高频 Case 的收益抵消保护 Case 的退化。
- 观察 Case 在 Iteration 阶段只记录和展示；归并前全量回归仍执行 universal no-regression gate。
- 正式性能判定必须在 warmup 后交错执行 `baseline → candidate` 的多轮配对测量，使用配对比值的中位数和 MAD 自动估算每个 Metric 的噪声容忍值。
- Baseline 和 Iteration 正式测量默认 warmup 10 次并执行 30 个 Pair，次序交替为 `baseline → candidate` 与 `candidate → baseline`。
- 归并前全量回归先执行 5 Pair，至少 4 Pair 有效；回退超过既有 noise tolerance 或样本无效时，独立重跑完整 30 Pair 并要求至少 24 Pair 有效。
- 使用改善比例的中位数作为结果，`noise_tolerance = max(0.5%, 3 × 1.4826 × MAD)`。
- 只有非有限值、进程失败或 GPU 错误会使 Pair 无效；普通统计离群点不裁剪。有效 Pair 少于 24 个时整组重跑，再次不足则拒绝该 Attempt。
- 至少一个目标指标的改善必须大于 `max(1%, noise_tolerance)`；保护指标在各自 `noise_tolerance` 内的波动不视为退化；正确性不容忍失败。
- Iteration Agent 可以自由运行临时 Benchmark，但提交接受判断时必须使用正式配对测量 Harness。

## 已确认的停止语义

- `max_attempts` 在创建候选尝试时计数；Accepted 和 Rejected 都计入，Plan、恢复会话和 Integration 不重复计数。
- 达到尝试预算或性能目标后停止创建新候选，但已经运行或进入归并队列的候选继续完成。
- 多个性能目标默认必须全部达到；Campaign 可以显式配置为任一目标达到即停止。
- 默认不因连续若干次无提升而提前停止；只有用户显式配置时才启用 Plateau 条件。
- 所有 Attempt 和 Integration 工作排空后，Campaign 才能进入 `Completed`。

## 已确认的 Sync 与 Best 更新

- Pika 和 Agent 默认不自动 Push；只有用户显式触发 Sync 才允许与远端双向同步。
- Sync 由独立 Sync Agent 执行。Sync 期间停止创建新 Attempt，但已经运行的 Iteration Agent 可以继续工作。
- Campaign 配置固定 `sync.remote` 与 `sync.branch`。当前分支有明确 upstream 时可作为默认值，否则第一次 Sync 前必须由用户指定；一次 Sync 只操作这一对 remote/branch。
- Sync Agent 从 `pika/best` 创建 `pika/sync/<sync-id>` 临时分支，fetch 后把配置的远端分支 merge 到临时分支；不能 rebase 或改写 Best 历史。
- Sync Agent 在临时分支解决冲突并执行完整正确性和 Best Metrics 测量。验证通过后先以普通 fast-forward push 更新远端，再 fast-forward 本地 `pika/best`。
- Push 失败时本地 Best 保持不变并记录 Failed Sync Trail。远端 Push 成功但本地推进前崩溃时，恢复流程依据事先持久化的 Sync Intent 与远端 SHA 完成本地推进。
- Sync 修改受保护 Harness 或 Campaign Spec 输入时进入 `AwaitingSpecConfirmation`；用户确认新 Spec Revision、重建 Baseline 和噪声估计后才能继续。用户拒绝时 Sync 失败且 Best 不变。
- Sync 完成后必须更新 Best Metrics、写入独立 Sync Trail，再恢复创建新 Attempt。
- Accepted Attempt 或成功 Sync 每次推进 `pika/best` 都产生 `BestAdvanced` 事件。
- `BestAdvanced` 通过 Pika MCP/Agent Mailbox 作为高优先级消息通知所有活跃 Iteration Agent，但不 interrupt 正在执行的 Backend Turn。
- Agent 在下一个 MCP 检查点更新基础版本；Pika 在正式 Benchmark、`complete_attempt` 和进入归并队列前检查 `base_sha`，陈旧时拒绝继续并要求 Agent 刷新。
- Iteration Agent 可以 rebase 自己的临时 Attempt Branch 到新 `pika/best`，解决冲突并重新测试。Attempt Branch 可改写历史，但 `pika/best` 永远不能 rebase，只能通过已验证的 squash merge 或 Sync merge 前进。

## 已确认的用户信息通道

同一个 “By the way” 输入入口支持三种显式模式：

1. 全局指导：注入当前调优任务之后的每个候选尝试。
2. 当次指导：只注入正在运行的候选尝试，用于即时纠偏。
3. 旁路对话：不注入任何调优上下文。

进入旁路对话时，系统需要向其上下文提供系统状态和当前候选尝试状态的摘要。只有用户明确要求注入时，前两种指导才会改变 Agent 上下文。

## 已确认的持久化边界

- 使用本地 SQLite WAL 保存调优任务、候选尝试、状态、最新 Metrics、消息、Agent 进程信息、Git SHA 和 Artifact 元数据。
- Git 是代码与提交历史的权威来源；SQLite 是编排状态和最新结构化 Metrics 的权威来源。
- Patch、Prompt、Agent 输出、日志和 Profiler 文件等文件型产物保存在本地 Artifact Workspace 中；SQLite 保存相对路径及必要元数据。
- Profiler 原始文件不得作为 Blob 写入 SQLite。
- 未来可以把 Artifact Workspace 扩展到 S3，但对象存储不在本次开发范围内。
- SQLite 使用当前状态表加 `domain_events` 的混合模型。主要表包括 `campaign`、`spec_revisions`、`benchmark_cases`、`metric_definitions`、`sampling_revisions`、`sampling_revision_cases`、`best_revisions`、`attempts`、`attempt_metrics`、`agent_sessions`、`agent_messages`、`guidance`、`artifacts`、`sync_runs`、`operation_intents` 和 `domain_events`。
- 每个 Attempt/Case/Metric 只保留最新结构化 Metric；Integration Full Regression 覆盖 Iteration 快照并补齐全量 Case。`domain_events` 保存状态转换和 UI 时间线，但不复制被覆盖的旧 Metric 快照。
- 状态变更、Operation Intent 和待广播 Domain Event 必须在同一 SQLite 事务中提交。事务成功后再通过 Phoenix PubSub 广播。

## 已确认的 Agent 日志边界

- 每个 Backend Session 写入独立 JSONL Artifact，保存 provider 原始消息、Backend Turn 和工具状态。
- SQLite 只记录日志相对路径、最后序号、开始/结束时间、Session 状态与摘要，不保存逐 token chunk、thinking 或 terminal output。
- Phoenix PubSub 实时广播标准化 Backend Event；页面刷新后按 JSONL 序号回放所需尾部，恢复 Prompt 也从尾部提取最近输出。

## 已确认的恢复边界

- Pika 的恢复正确性不依赖 Codex、Cursor 或其他 Agent 的会话 resume 能力。
- Agent 异常结束或服务重启后，运行中的候选尝试标记为 `Interrupted`，原 worktree、提交和 Artifact 保留。
- 系统通过新 Backend Session 注入任务定义、当前计划、工作区状态和最近输出，继续同一个候选尝试。
- 服务启动后自动恢复正常的 Planning、Iteration 和归并前全量回归工作，不等待用户点击继续。
- 启动恢复必须检查未完成的 Full Regression、merge 和实际 Git 状态；`Blocked` 不自动解除。
- 无法确认 Campaign Best Branch 安全、Campaign Workspace 被外部修改或 Artifact 校验失败时，停止危险推进并通知用户。

## 已确认的 Agent 语义协议

- Pika MCP 是所有 Agent 与调优领域状态交互的强制接口。
- Agent 通过 Pika MCP 读取 Campaign、历史、用户指导、Sampling/Best Advanced 与当前系统状态，并提交 Plan、Summary、Metrics、Full Regression、Commit、Merge 和最终状态。
- Agent 的文本输出和传输层事件只用于实时展示与诊断，不作为结构化状态的权威来源。
- 候选尝试只有成功调用 `complete_attempt` 后才算完成；Plan 只有成功调用对应的 MCP 提交操作后才算生成。
- Backend Turn 正常结束但缺少当前角色必须完成的 MCP 操作时，Pika 保持同一 Backend Session，并发送 follow-up Prompt 要求 Agent 继续完成和提交缺失工作。
- 缺失 MCP 提交不会仅凭 Agent 自然语言中的“完成”而自动补全。
- 缺失必需 MCP 提交没有提醒次数、Agent 更换次数或时间预算；Pika 持续驱动 Agent，直到 MCP 提交成功、用户取消或调优任务因其他停止条件结束。
- Backend Session 或进程失效时，自动恢复流程用新会话继续上述无限重试，不因此消耗新的优化 Iteration。
- Pika 不从 Agent 的自然语言最终回答中猜测 Metrics、Commit 或完成状态。
- 新 Plan/Iteration Prompt 默认注入最近 10 个终态 Attempt 的 Description、Summary、Outcome、Metric delta 和关键失败原因；N 可在 Server 启动配置中修改。
- 未读 BestAdvanced、Sampling Advanced 和用户指导不受 N 限制，必须全部注入；完整历史通过 `query_attempt_history` 查询。

## 已确认的 Pika MCP 传输与权限

- Phoenix 只在 loopback 暴露 Streamable HTTP `/mcp`。
- 每个 Backend Session 获得独立、短期、角色受限的 MCP Token；SQLite 只保存 Token 哈希，服务重启后的新 Session 使用新 Token。
- Agent Backend 在打开 Session 时配置 MCP URL 与 Token。Codex 通过 App Server 进程配置注入，Cursor 通过 ACP Session 配置注入；不能连接 HTTP MCP 的 Backend 不符合 conformance contract。
- MCP Token 在服务端绑定 Campaign、Backend Session、Role 与可选 Attempt ID；Agent 不能通过工具参数切换身份。
- Boundary、Plan、Iteration、Integration、Sync 与 Side Conversation 使用不同工具集合。跨 Attempt 读取只能通过显式历史查询工具，所有写操作必须携带 idempotency key。
- 必需完成调用为：Boundary Drafting 的 `submit_spec`/`submit_harness`、用户确认后的 `complete_setup_merge`/`submit_baseline`/`submit_iteration_sample`，Plan 的 `submit_plan`，Iteration 的 `record_metrics`/`submit_attempt_summary`/`complete_attempt`，Integration 的 `submit_full_regression`、必要时的 `submit_sampling_feedback` 和通过后的 `complete_merge`，Sync 的 `complete_sync`。Side Conversation 没有完成门禁。
- Metrics 可以重复提交，后一次覆盖当前快照；缺少必需调用时继续采用无限 follow-up 规则。

## 已确认的 Agent Backend 与通信协议

- Pika 领域层只依赖 `Pika.AgentBackend` Behaviour，不依赖 Codex App Server 或 ACP wire types。
- `Pika.AgentBackend` 固定提供 `start_link`、`open_session`、`start_turn`、`steer`、`interrupt`、`close_session` 和 `capabilities`。
- 标准化 Backend Event 包括 `session_started`、`turn_started`、`message_delta`、`plan_updated`、`tool_started`、`tool_updated`、`tool_completed`、`command_output`、`file_changed`、`usage_updated`、`turn_completed`、`backend_error` 和 `process_exited`。
- Codex 使用 `Pika.AgentBackend.CodexAppServer`：每个 Backend Session 启动独立 `codex app-server --listen stdio://`，执行 `initialize → initialized → thread/start → turn/start`，通过 `thread/start.developerInstructions` 注入 Agent Instructions，并把 `item/*`/`turn/*` 通知转换为标准事件。
- Cursor 使用 `Pika.AgentBackend.CursorACP`：每个 Backend Session 启动独立 `cursor-agent acp`，执行 ACP `initialize`、`session/new`、`session/prompt` 和 `session/cancel`；由于 ACP 没有 system-instruction 字段，adapter 在临时 Workspace 安装 Git-excluded、always-on 的 `.cursor/rules` 系统规则，并在 Session 关闭时清理。`session/close` 仅在 capability 广告时调用，否则终止该 Session 的独立子进程。
- Codex 当次指导使用原生 `turn/steer`，不 interrupt 当前 Turn；Cursor 由 Backend adapter 通过 cancel + follow-up Prompt 模拟 `steer`。
- Stop Now 调用统一 `AgentBackend.interrupt`；Codex 映射到 `turn/interrupt`，Cursor 映射到 `session/cancel`。
- 每个活跃 Backend Session 使用独立子进程、MCP Token 和配置，隔离崩溃与权限影响。
- 多 Agent 通信采用中心辐射模型。Agent 不直接连接其他 Agent，而是通过 Pika MCP 的 Agent Mailbox 查询 Agent、发送消息和读取消息。
- Agent Mailbox 消息必须先持久化到 SQLite，只能在同一调优任务内路由。目标 Agent 忙碌时，在后续 MCP 检查点或 Backend Turn 获取消息。
- v0 内置 Codex App Server 与 Cursor ACP；新增 Backend 必须实现 `Pika.AgentBackend` conformance contract。

## 已确认的 Agent Profile

- Boundary、Plan、Iteration、Integration、Sync 和旁路对话角色都可以选择命名 Agent Profile。
- 每个 Agent 实例都可以使用不同的 Agent Backend、模型和 reasoning effort；同一 Campaign 不要求统一模型。
- Iteration Agent 使用显式 Slot 配置；每个 Slot 指定自己的 Agent Backend、模型、reasoning effort、环境与权限覆盖。
- `iteration_agents` 数组长度就是用户配置的 Iteration 并发度。每个 Slot 完成一个 Attempt 后继续领取下一个，不使用额外的权重调度。
- `Agent Backend` 是原先口语中的 Agent `harness`；`Benchmark Harness` 只表示正确性和性能测量程序，两者不得混用。

## 已确认的后端语言

- Pika 服务后端使用 Elixir/OTP，而不是全 TypeScript 后端。
- 每个 Backend Session、Campaign 状态机、归并队列和 Sync 队列都应映射为受监督的独立进程。
- Agent 协议通过 `Pika.AgentBackend` 隔离；Codex adapter 直接实现 App Server JSON-RPC，Cursor adapter 可使用经过 conformance 的 Elixir ACP 库或受控 fork。

## 已确认的部署与配置

- v1 正式支持 Linux x86_64 GPU 主机。macOS 只支持 UI、目标对齐和开发，不承诺进入 GPU 优化阶段。
- Pika 以 Elixir Release 交付，不要求 Docker，也不内置 GPU 容器运行时。
- 标准入口为 `pika serve --workspace <path> [--repo <path>] --config <pika.yaml> --host 127.0.0.1 --port 8080`。
- 用户配置使用 YAML；解析后的完整有效配置保存为 Workspace 内 `config.json`，用于恢复与审计。
- Pika 始终以前台进程运行，不实现 daemonize；systemd、Supervisor、tmux 或其他外部进程管理器负责常驻和拉起。
- 启动时检查 Git、Agent Backend、Python、GPU/Driver 与 Campaign 配置，但不自动安装或升级外部依赖。
- Workspace、Managed Repo、监听地址和 Backend type/command/protocol config 启动后不可变。
- Plan、最大 Attempts、历史 N、Reference Catalog 和停止条件可以在 UI 修改。
- Iteration Slot 的模型与 reasoning effort 只影响下一次领取的 Attempt，不热切换正在运行的 Session。
- 改变 Shapes、Metrics、Harness 或正确性要求必须产生 Spec Revision。

## 已确认的 Web 与 Agent Backend 实现栈

- Web 服务使用 Phoenix 和 LiveView；对话、状态与配置界面不维护独立 React SPA。
- Phoenix PubSub 把标准化 Backend Event 和持久化状态变更广播给 LiveView。
- Metrics 曲线使用 ECharts LiveView Hook；同时保留 JSON API 供脚本和未来客户端使用。
- `Pika.AgentBackend` 是内部 Behaviour；Phase 0 对 Codex App Server 和 Cursor ACP 分别运行协议测试，并对两者运行统一事件/MCP/steer/interrupt conformance suite。
- Codex adapter 在 conformance 时运行 `codex app-server generate-json-schema` 保存实际 CLI 版本 schema 证据；不引入 Codex SDK sidecar 或 `codex exec --json`。
- Pika 不引入 Node sidecar，也不使用 `acpx` 作为运行时。

## 已确认的 Backend 用户消息行为

- Pika 在打开 Backend Session 时只注入 Agent Instructions，不自动创建首个 Turn。Campaign 首轮必须由用户消息 Kick-off，且 Backend 收到的首条用户输入保持用户原文与附件元数据，不前置 Pika 模板。
- 用户点击“确认并建立 Baseline”属于显式用户动作，可驱动 setup merge，并在新的 Baseline Session 中作为已授权 Kick-off 重放；Pika 不能用系统生成的 Baseline Prompt 冒充用户动作。
- 当次指导先持久化，再调用当前 Backend Session 的 `steer`。Codex 原生追加到 in-flight Turn；Cursor adapter cancel 当前 Turn 后在同一 Session 发送 follow-up Prompt。
- `steer` 失败统一返回 `steer_failed`；恢复流程关闭对应子进程并创建新 Backend Session 继续同一候选尝试。
- 全局指导只影响之后创建或继续的 Iteration，不取消当前 Agent。
- 旁路对话使用独立 Backend Session，不影响 Iteration Agent 的 Backend Turn。

## 已确认的权限边界

- Pika v1 把本机 Agent 视为受信任进程，Backend 权限请求默认自动批准，运行方式默认为 YOLO。
- Pika 不承诺限制 Agent 对主机、网络、Git 或 GPU 的访问，也不作为恶意代码安全沙箱。
- Agent 凭证从服务启动环境或 Agent Profile 引用的环境变量继承，不得写入 SQLite、Prompt、Backend UI 事件或应用日志。
- 受保护 Harness、临时 `ref/` 和接受规则仍在候选 Diff 与验证阶段强制检查。

## 已确认的归并租约

- Integration Agent 在任何 Git 归并前调用 `acquire_integration_lease(expected_best_sha)`。
- SQLite 事务同时检查当前 Best SHA、现有租约和 Backend Session 身份；成功后租约绑定 Backend Session 与受 Supervisor 监控的 Elixir 进程，不使用固定超时。
- Agent 完成 Git 操作后调用 `complete_merge`。Pika 验证实际 HEAD、父提交、Diff、受保护 Harness、Metrics 和 commit trailers，再提交数据库状态并释放租约。
- Agent 进程崩溃时不能直接释放租约并调度下一个 Merge。恢复流程必须先核对 Full Regression Receipt、Operation Intent、未完成 merge 和实际 Git 状态，再启动恢复 Agent。

## 已确认的 HTTP Token 行为

- 每次服务启动生成 256-bit 随机 Token，只输出到启动终端，不持久化到 SQLite。
- API、SSE 和 WebSocket 使用 Bearer Token；浏览器首次通过带 Token 的 URL 访问时，换取 `HttpOnly`、`SameSite=Strict` Cookie，并立即重定向到不含 Token 的 URL。
- Token 不写入 localStorage 或应用访问日志；服务重启后旧 Token 与旧 Cookie 失效。
- 同一 Token 可以被多个浏览器窗口使用，用户动作按 SQLite 中的单调序号排序。

## 已确认的人工控制

- `Pause`：停止创建新候选，已运行 Agent 和归并前全量回归继续完成。
- `Stop Now`：通过 `AgentBackend.interrupt` 终止活动 Backend Turn，停止归并和自动恢复，但保留 SQLite、Git worktree 与 Artifact。
- `Resume`：从持久状态恢复调优。
- 删除 Campaign、worktree 或 Artifact 是独立显式操作，不能由 Pause 或 Stop 隐式触发。

## 已确认的对话界面

- 目标对齐使用独立 Alignment Conversation，专门承载 Boundary Agent 的多轮访谈、输入 Artifact、Campaign Spec diff 和用户确认。
- 每个运行中的 Attempt 提供独立 Attempt Conversation，展示对应 Backend Session 的文本、Plan、Tool Call、Diff、Terminal 和状态事件。
- BTW Conversation 只能从某个正在运行的 Attempt Conversation 中 fork；它天然绑定该 Attempt，不提供无来源的“当前 Attempt”选择器。
- BTW 使用同一 Composer 的三个显式模式：仅对话、注入该 Attempt、注入后续 Attempts。默认仅对话，Pika 不根据自然语言静默升级注入级别。
- 注入该 Attempt 时调用统一 Backend `steer`；注入后续 Attempts 不 interrupt 当前 Agent。
- BTW Context 包含系统状态和父 Attempt 当前工作摘要。

## 已确认的 Metrics 界面

- Metrics 使用真实时间轴展示每次尝试的不同 Metrics 折线，而不是 Best-only 阶梯图或 Accepted/Rejected 散点图。
- 每个点使用该 Attempt 当前最新 Metric；Integration 筛查或完整回归结果覆盖对应点并补齐未采样 Case。
- 用户可以按 Benchmark Case、Metric 和 Spec Revision 筛选；不同 Spec Revision 分段，不形成连续可比曲线。
- 点击时间点可以查看 Attempt Summary、Outcome、Patch、Profiler 和 Agent 日志。

## 已确认的线上输入获取

- 线上 Shape 与输入分布在 Alignment Conversation 中由 Boundary Agent 和用户共同确认，不要求预先固定一种导入格式。
- Boundary Agent 可以生成面向实际环境的测试/采集脚本，也可以读取用户提供的 pickle dump 或 JSONL 文件。
- 采集或导入结果进入 Campaign Spec 草稿，只有用户明确确认后才成为 Benchmark Cases。
- Alignment、setup merge 与 Baseline Agent Instructions 分别是 Config 可覆盖的独立 EEx 资源；资源缺失或无法编译时必须在打开 Backend Session 前失败。
- Agent Instructions 只作为 provider 的系统级上下文注入，不能作为首条用户 Prompt 或自动 Kick-off。Campaign 的首个 Backend Turn 必须保留用户首条消息原文；用户确认 Spec 的显式动作负责推进 setup merge 与 Baseline。
- Alignment Instructions 建议但不强制 latency、memory、TFLOPS、bandwidth 或 throughput 等常见 Metrics；Metric ID、名称和单位保持自由。
- 用户消息与附件作为同一个领域动作提交；附件只向 Agent 注入相对路径、MIME、大小与哈希，不自动把文件内容展开进 Prompt。

## 已确认的 Profiler 策略

- BuildingBaseline 必须生成一次 Profiler Artifact。
- Iteration Agent 可以自主决定是否运行 NCU、Nsight Systems 或其他 Profiler；Profiler 不是候选接受门禁，正确性与正式 Metrics 才是。
- Agent 通过 Pika MCP 注册 Profiler 目录、工具、命令、目标 SHA 与 Summary。
- 归并前全量回归默认不重复 Profiler。
- Profiler 原始文件保留在 Attempt 的 Artifact Workspace；清理 worktree 不删除 Profiler Artifact。

## 已确认的开源复用边界

- Codex App Server adapter 直接实现官方 stdio JSON-RPC；Cursor ACP adapter 可使用通过 conformance spike 的 Elixir ACP 库。二者都隔离在 `Pika.AgentBackend` 后。
- KernelAgent、Atrex Kernel Agent 和 Humanize/flowverse 不作为 Pika 状态机或持久化运行时依赖。
- Pika 可以复用它们的 Benchmark、Profiler 解析、Prompt/Skill 和 conformance 思路；代码级复用必须单独检查 License，并封装在 adapter 内。
- 参考 Kernel 仓库继续通过临时 `ref/` submodule 提供给 Agent，不成为构建或运行依赖。

## 已确认的 Campaign 状态

- 主路径为 `DraftingSpec → AwaitingConfirmation → BuildingBaseline → Optimizing → Draining → Completed`。
- `Paused`、`Blocked`、`Stopped` 与 `AwaitingSpecConfirmation` 是可持久化恢复状态。
- Pause 不取消在途工作；Stop Now 需要确认并 interrupt Backend Turn；Resume 可以恢复 Paused/Stopped，但不能自动解除 Blocked。
- Sync 前确认 remote、branch 和待 Push commit；删除 Workspace 默认只提供 CLI，不放在 Web UI。

## 可交互 UI 原型

- 2026-08-18 用户已确认当前信息架构与交互，无待修改项；最终 Phoenix/LiveView UI 以此为设计基线。
- 原型源码位于 [`docs/design/prototype/`](./prototype/)，用于讨论信息架构与交互，不是最终 Phoenix/LiveView 实现。
- 原型包含目标对齐对话、Attempt 对话及 Attempt-scoped BTW fork、Metrics 时间折线三个可切换视图。
- 原型使用模拟的 H20 Flash Attention Decode 数据，所有 commit、Metrics、Agent 输出和 Artifact 仅用于设计评审。
- 用户确认交互后，最终 Phoenix/LiveView UI 应复用领域结构和行为，而不是复用该 React 原型的运行时架构。

## 实现级配套文档

- [架构与监督树](./architecture.md)
- [Campaign、Attempt、Sync 与恢复状态机](./state-machine.md)
- [SQLite schema](./database-schema.md)
- [Pika MCP API](./mcp-api.md)
- [实现阶段与验收计划](./implementation-plan.md)
- [Reference 与 Skill Registry](./reference-registry.md)

## 待决问题

无架构待决问题；v1 设计已冻结。

## Pika 架构参考（不注入 `ref/`）

- Meta KernelAgent：融合子图拆分、并行 Kernel Worker、严格正确性验证、NCU/Roofline 驱动优化与 Gradio UI。
- Atrex Kernel Agent：干净的 Agent CLI 会话、磁盘结构化迭代记忆、以 Git 最佳版本驱动的外循环和 profile-driven planning。
- Humanize/flowverse：异构 Agent 轮换、以仓库作为跨会话记忆、流程 tracing 与中断恢复模型。

这些项目只用于设计和实现 Pika，不属于 Campaign Reference Catalog，也不会作为 Kernel Ref 注入 Attempt。只有通过 License 和 adapter 边界审查的模块才进入直接依赖。
