# Pika Kernel 自动调优

Pika 管理从计算边界定义到最终优化结果交付的完整 Kernel 自动调优过程。本文件只定义领域语言，不描述具体实现。

## Language

**调优任务（Optimization Campaign）**：
围绕一个已确认的计算边界、正确性契约、性能指标与停止条件开展的完整调优活动；它与一个 Pika Server 一一对应，系统重启不会改变其身份。
_Avoid_: 任务、Job、Run

**Campaign Workspace**：
一次完整优化从源仓库创建的独立工作空间，包含 Campaign Best Branch、状态数据库、Artifact 和候选 worktree；它不修改源仓库原分支。
_Avoid_: 主仓库、Attempt Worktree、多个 Campaign 的共享目录

**Managed Repo**：
用户显式交给当前 Pika Server 独占管理的本地 Git 仓库；Campaign Workspace 通过软链接把它暴露为 `repo/`。
_Avoid_: 只读源仓库、远端仓库、临时 worktree

**Campaign Spec**：
用户明确确认的调优边界，包括计算语义、输入契约、Fusion 范围、Shapes、正确性要求、Metrics、Benchmark 协议和停止条件。
_Avoid_: 用户 Prompt、README、Plan

**可确认 Campaign Spec（Confirmable Campaign Spec）**：
内容与审阅证据均已完整，并且 Alignment Agent 当前 Turn 已结束、没有待回答问题的稳定 Campaign Spec；只有该状态可以由用户确认并冻结。
_Avoid_: Agent 中间输出、仅显示 AwaitingConfirmation、待回答问题

**Spec Revision**：
Campaign Spec 在优化开始后的显式新版本；涉及语义、Shapes、Metrics、Benchmark 或正确性要求的变化都会产生新 Revision，并重新建立 Baseline。
_Avoid_: 全局指导、配置热更新、Plan 变更

**基线（Baseline）**：
调优任务进入迭代前，Development Implementation 在 Full Case Set 上相对固定 Optimization Target 的首组正式正确性与性能测量；对应代码版本是首个 Best Known Revision，而不是 Target。
_Avoid_: Optimization Target、Reference、单一“基线代码”

**正确性判定器（Correctness Oracle）**：
定义计算语义真值、用于独立判定 Optimization Target 与 Development Implementation 是否正确的实现或规则；它可以是仓库内受保护代码，也可以明确复用冻结 Target 的输出。
_Avoid_: Optimization Target、Baseline、性能 Reference

**优化目标（Optimization Target）**：
Campaign 固定的性能锚点实现；它来自被审阅的 Development 快照或某个固定 SHA 的 Reference Project，在该 Target 定义未显式修订时不随 Best 推进。
_Avoid_: Correctness Oracle、Baseline、当前 Best、Development Implementation

**Target Snapshot**：
Optimization Target 源码在 Campaign Workspace `targets/<revision>/repo` 中的不可变身份，绑定来源类型、Reference ID、commit/tree SHA、入口文件与内容 digest；worktree 只通过 Git 忽略的 `target/` 软链接读取它。
_Avoid_: Reference Checkout、Attempt Worktree、可修改源码副本

**开发实现（Development Implementation）**：
产品仓库内持续被 Attempt 优化的实现；Alignment 必须先给出可运行入口和初始提交，之后由 Campaign Best Branch 表示其最佳已知版本。
_Avoid_: Optimization Target、Correctness Oracle、Reference Project

**Implementation Review Evidence**：
证明同一 Benchmark Case 上 Optimization Target 与初始 Development 都通过 Correctness Oracle，并包含两者配对性能值的确认前 smoke-run 证据；它绑定 Spec、Harness、Target Snapshot、Development SHA 与本地 Artifact。
_Avoid_: Baseline、完整 Benchmark、Agent 自述、Reference Review Evidence

**受保护 Harness（Protected Harness）**：
已随 Campaign Spec 确认的仓库内 Correctness Oracle（若有）、正确性测试与 Benchmark Harness；候选尝试只能读取和执行，Development 入口和外部 Target Snapshot 不属于受保护产品代码。
_Avoid_: Optimization Target、Development Implementation、普通测试、Profiler 配置

**候选尝试（Optimization Attempt）**：
在独立工作空间中执行的一次有边界的性能改进实验，最终只能被接受或拒绝。
_Avoid_: Iteration、版本、分支

**Plan Artifact**：
可选 Plan 阶段生成、供后续 Agent 使用但永不归并到 Campaign Best Branch 的 `plan.md`。
_Avoid_: Campaign Spec、提交说明、最终 Summary

**Reference Catalog**：
Pika 可注入 Attempt `ref/` 的 Kernel 实现仓库清单；它包含内置项目，也包含用户为当前 Campaign 添加的 Git 仓库。用户在 Alignment 中选择条目，确认 Campaign Spec 时，所有已选条目解析并冻结到具体 commit SHA。
_Avoid_: Git submodule 状态、运行时依赖、包管理清单

**Reference Checkout**：
Campaign Workspace 拥有的、固定到 Reference Project commit SHA 的独立只读仓库副本；Agent 可从候选工作区读取它，但它不属于产品仓库内容或可交付 Patch。
_Avoid_: Git submodule、产品源码、运行时依赖、候选修改

**Campaign Reference Project**：
用户添加到当前 Reference Catalog 的 Git 仓库；其项目 ID 在 Campaign 内唯一，确认前可选择或删除，确认后以已解析的 commit SHA 进入冻结 Reference snapshot。
_Avoid_: 漂移的 branch、Reference Implementation、全局 Registry 配置

**Skill Registry**：
Pika 提供给 Backend Session 的外部 Skill 清单，与 `ref/` Kernel 仓库相互独立；Skill 不进入候选 Patch 或 Campaign Best Branch。
_Avoid_: Reference Catalog、Agent Backend、Pika MCP tools

**Backend Session**：
由 Pika 启动并通过某个 Agent Backend 控制的独立编码会话；Codex 对应独立 App Server thread，Cursor 对应独立 ACP session。
_Avoid_: GPU Worker、执行节点、统一 ACP Session

**Agent Profile**：
为一次 Agent 会话选择 Agent Backend、模型、reasoning effort、环境和权限行为的命名配置。
_Avoid_: Benchmark Harness、Campaign Spec、Backend Session

**Backend Permission Policy**：
Agent Profile 中由具体 Agent Backend 解释的一对执行策略：Approval Policy 决定 provider 如何审查或请求批准操作，Sandbox Policy 决定 provider 对文件系统和网络施加的技术访问边界；它们可随 Agent Profile 修改，不产生 Spec Revision。Codex 自身不可配置的硬性执行规则不属于 Sandbox Policy。
_Avoid_: Campaign Spec 权限、Protected Harness 门禁、主机安全边界、统一跨 provider 的权限等级

**Agent Backend**：
把 provider-specific 控制协议转换为 Pika 统一会话、Turn、steer、interrupt 与标准事件接口的适配器。
_Avoid_: Agent Harness、Benchmark Harness、模型、ACP-only Client

**Agent 邮箱（Agent Mailbox）**：
Pika 为同一调优任务内的 Agent 持久化并按目标路由消息的通信通道；发送方和接收方不直接建立连接。
_Avoid_: Backend Session、共享 Prompt、进程标准输入

**Pika MCP**：
所有 Agent 读取调优状态、历史与用户指导，以及提交计划、Metrics、Git 结果和完成状态的强制语义接口。
_Avoid_: Agent stdout、Backend 原始事件流、自然语言结果解析

**Iteration 开发 Agent（Iteration Agent）**：
在候选尝试的独立工作空间中规划、修改、提交、测试和报告结果，并在轮到该候选归并时操作 Git 的编码 Agent。
_Avoid_: Worker、Benchmark Agent

**已接受尝试（Accepted Attempt）**：
至少一个目标指标改善超过验收阈值，且所有保护指标均未退化的候选尝试。
_Avoid_: 成功运行、最快版本

**已拒绝尝试（Rejected Attempt）**：
未通过正确性验证、未达到改善阈值，或导致任一保护指标退化的候选尝试。
_Avoid_: 失败代码、无用尝试

**最佳已知版本（Best Known Revision）**：
调优任务中由所有已接受尝试依次归并形成、作为后续候选尝试起点的代码版本。
_Avoid_: main、HEAD、最新版本

**Campaign Best Branch**：
位于 Campaign Workspace 中、由已接受尝试串行推进并代表该次优化最佳已知版本的分支；源仓库原分支不随 Iteration 改变。
_Avoid_: Target Branch、main、用户原分支

**Best Advanced**：
Campaign Best Branch 因 Accepted Attempt 或 Sync 产生新版本的持久化事实；所有活跃 Iteration Agent 都必须获知并刷新自己的基础版本。
_Avoid_: Git hook、聊天通知、Metric 更新

**Sync**：
由用户显式触发、通过专用 Agent 同步 Campaign Best Branch 与远端并重新测量 Best Metrics 的维护流程；执行期间不创建新 Attempt。
_Avoid_: 自动 fetch、Iteration Merge、Pika 自动 Push

**Sync Trail**：
记录一次 Sync 的远端版本、Git 结果、Metric 更新、Agent Summary 和时间的持久化审计记录。
_Avoid_: Agent 日志、Git reflog、Attempt History

**Sync Intent**：
Sync 修改 Git 或远端前持久化的预期操作，包含本地起点、远端目标与候选 SHA，用于崩溃后判断操作是否已经发生并幂等恢复。
_Avoid_: Sync Trail、Agent Plan、Git reflog

**Domain Event**：
Pika 在状态事务中追加、用于恢复调度决策和构造 UI 时间线的领域事实；它不保存被覆盖的旧 Metric 快照。
_Avoid_: Backend 流式事件、Agent JSONL、应用日志

**归并队列（Integration Queue）**：
将并发候选尝试按确定顺序逐个验证并归并到最佳已知版本的队列；同一时刻最多处理一个候选尝试。
_Avoid_: Merge Agent、并行 Merge、提交队列

**归并租约（Integration Lease）**：
绑定一个 Integration Backend Session 与预期 Best SHA、授权其独占推进 Campaign Best Branch 的临时权利；进程失效不会在 Git 状态核对前直接释放。
_Avoid_: 固定超时锁、Git lock 文件、Agent 自报状态

**归并前全量回归（Pre-Merge Full Regression）**：
候选尝试进入 Campaign Best Branch 前，对全量 Case 集执行的串行正确性与性能门禁；只有全部 Case 均未确认回退的候选才允许归并。
_Avoid_: Iteration Benchmark、合入后复验、自由 Benchmark

**阻塞（Blocked）**：
系统无法安全恢复 Campaign Best Branch 时的调优任务状态；已有候选可以保存工作，但任何新归并都被禁止。
_Avoid_: Paused、Stopped、Failed

**中断（Interrupted）**：
Backend Session 意外结束但候选尝试的工作空间与持久状态仍可继续使用的状态；恢复不要求重新使用原 Backend Session。
_Avoid_: Failed、Blocked、Cancelled

**Metric 快照（Metric Snapshot）**：
某个版本当前最新的一组结构化性能测量；Integration Full Regression 产生更完整结果时替换 Iteration 快照，而不保留多套结构化观测。
_Avoid_: Metric Observation、测量历史、曲线点集合

**噪声容忍值（Noise Tolerance）**：
Baseline 重复测量后为每个 Metric 自动估算的正常波动范围，用于区分真实改善或退化与测量噪声。
_Avoid_: 1% 接受阈值、正确性容差、用户拍脑袋阈值

**Benchmark Case**：
由 shape、dtype、layout、输入分布和可选线上频率权重共同定义的一组可重复性能输入；每个 Case 上的 Metric 独立参与判定。
_Avoid_: Shape、测试样例、一次测量

**全量 Case 集（Full Case Set）**：
当前 Spec Revision 已确认的全部 Benchmark Cases；初始 Baseline 和归并前全量回归都覆盖这个集合。
_Avoid_: 迭代采样集、线上 Dump 原始记录、一次 Attempt 的自由测试列表

**迭代采样集（Iteration Sample Set）**：
从全量 Case 集中选择、供日常候选尝试正式测量的较小集合；初始集合由 Baseline Agent 选择，已确认回退的代表 Case 会在后续版本中加入。
_Avoid_: 全量 Case 集、Agent 临时 Benchmark、随机抽样结果

**采样版本（Sampling Revision）**：
同一 Spec Revision 内某一时刻生效的迭代采样集快照；初始版本最多十个 Case，回退反馈只能新增成员，直到新 Spec Revision 才能重置。
_Avoid_: Spec Revision、Attempt 序号、Metric Snapshot

**采样推进（Sampling Advanced）**：
归并前全量回归把已确认回退的代表 Case 加入迭代采样集后产生的持久化事实；活动 Agent 会获知它，但已运行 Attempt 不被强迫改用新版本。
_Avoid_: Best Advanced、用户 Guidance、Spec Revision

**目标 Case（Target Case）**：
至少需要有一个 Metric 取得真实改善的 Benchmark Case。
_Avoid_: 高频 Shape、性能目标

**保护 Case（Guard Case）**：
任何受保护 Metric 都不能退化超过噪声容忍值的 Benchmark Case。
_Avoid_: 回归测试、次要 Shape

**观察 Case（Informational Case）**：
日常 Iteration 阶段只记录和展示 Metrics、不用于证明收益的 Benchmark Case；归并前全量回归中仍必须不回退。
_Avoid_: Target Case、永远不参与门禁的 Case

**配对测量（Paired Measurement）**：
交错执行固定 Optimization Target 与某个 Development 候选，并基于同轮比值判断其相对 Target 的改善与噪声；候选对当前 Best 的比较使用同一 Case/Metric 的持久化 Development 值另行计算。
_Avoid_: Target/Best 混用、Baseline 单次测量、Agent 临时 Benchmark、非配对 Screening

**Artifact Workspace**：
保存 Patch、Prompt、Agent 输出、日志与 Profiler 文件等文件型产物的本地目录树；结构化状态只通过相对路径引用其中的文件。
_Avoid_: SQLite Blob、S3 Bucket、Git 仓库

**全局指导（Campaign Guidance）**：
用户明确要求注入此后每个候选尝试的信息，属于调优任务持续生效的约束或知识。
_Avoid_: By the way、系统提示词

**当次指导（Attempt Guidance）**：
用户明确要求只注入当前候选尝试的信息，在该尝试结束后失效。
_Avoid_: 即时消息、临时 Prompt

**旁路对话（Side Conversation）**：
不注入调优上下文、只用于用户与系统交流的信息。
_Avoid_: 普通 By the way、指导

**目标对齐对话（Alignment Conversation）**：
优化开始前由用户与 Boundary Agent 共同形成和确认 Campaign Spec 的独立对话。
_Avoid_: Attempt Conversation、BTW Conversation、Campaign Guidance

**Campaign Kick-off**：
用户通过首条消息或明确确认动作授权 Pika 开始或推进 Campaign 的领域动作；Pika 的系统指令、Session 创建和恢复都不构成 Kick-off。
_Avoid_: 自动 Prompt、Session 启动、系统消息

**Agent 系统指令（Agent Instructions）**：
Pika 注入 Backend Session、用于约束角色、权限与完成门禁的系统级上下文；它不属于用户消息，也不能触发或冒充 Campaign Kick-off。
_Avoid_: 用户 Prompt、Campaign Kick-off、自动用户消息

**Attempt 对话（Attempt Conversation）**：
用户查看某个正在运行的 Attempt 时看到的 Backend Session 工作过程，也是创建 BTW Conversation 的唯一入口。
_Avoid_: 目标对齐对话、Agent JSONL、汇总报告

**BTW Conversation**：
从一个指定 Attempt Conversation 当前上下文 fork 出来的用户对话；其消息默认不注入，只有用户显式选择当次或后续指导时才改变优化上下文。
_Avoid_: 全局聊天、无来源对话、自动指导

**Metrics Timeline**：
以真实时间为横轴、把每次尝试的各项最新 Metrics 连成折线的可视化；不同 Spec Revision 分段展示。
_Avoid_: Best-only 阶梯图、Attempt 散点图、旧 Metric 观测历史
