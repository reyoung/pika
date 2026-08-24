# Pika Kernel 自动调优 v2

Pika v2 管理一个代码仓库内的一次 Kernel 优化，从目标与开发基线对齐开始，到形成可交付的最佳已知版本结束。

## 优化对象

**Optimization**：
一个 Pika 进程管理的完整优化生命周期，绑定一个仓库、一个 Optimization Target 和一个 Development Baseline。
_Avoid_: Campaign、Job、Run

**Optimization Target**：
冻结的目标实现，代表待优化计算的预期行为和稳定性能参照；它可以与最初的 Development Baseline 使用相同代码，但身份始终独立。
_Avoid_: Development Baseline、Best Revision、Correctness Oracle

**Development Baseline**：
首次通过 Baseline Verification 的可优化实现，也是第一个 Best Revision；后续 Attempt 只演进这条实现线。
_Avoid_: Optimization Target、未验证 setup 代码

**Correctness Oracle**：
判断 Target 与 Candidate 是否满足正确性契约的真值规则；它可以要求两者等价，也可以独立判断两者。
_Avoid_: Benchmark、Optimization Target、误差日志

## Baseline

**Baseline Definition**：
经用户审阅的 Target、Development Baseline、Oracle、Harness、Full Case Set、Metrics 与测量协议的不可变契约。
_Avoid_: Baseline Snapshot、Agent Prompt、smoke 输出

**Baseline Revision**：
Baseline Verification 进入优化前打回 Definition 后形成的新版本；任一被审阅身份或文件变化都会产生新 Revision。
_Avoid_: Sampling Revision、Iteration Round

**Baseline Review**：
用户对 Baseline Definition、相关代码 digest 和单 Case smoke evidence 的显式批准或退回。
_Avoid_: Agent 自述通过、Baseline Verification

**Baseline Verification**：
独立 Agent依据已冻结 Definition 对 Full Case Set 执行正确性与性能测量，并判断它是否可作为优化起点。
_Avoid_: smoke test、Iteration、用户 Review

**Baseline Snapshot**：
Baseline Verification 接受后形成的全量逐 Case 数值、噪声和聚合结果，绑定 Target 与 Development Baseline 身份。
_Avoid_: Baseline Definition、Target Snapshot

**Target Snapshot**：
Baseline Review 冻结的 Optimization Target 身份；无论 Target 来源如何，下游 Agent 都把它视为不可修改的目标实现。
_Avoid_: Development Baseline、Reference branch

## Case 与测量

**Benchmark Case**：
一个稳定整数 ID 所代表的可重复输入，详细描述包含 shape、dtype、layout、权重和 critical 属性。
_Avoid_: Case name、一次运行、Metric

**Full Case Set**：
Baseline Definition 已确认的全部 Benchmark Cases；Baseline Verification 与 Integration 必须覆盖它。
_Avoid_: Iteration Sample、线上原始样本

**Iteration Sample**：
供 Iteration 快速验证的 Case 集；初始最多十个，之后只能通过 Integration 反馈单调增加，直至 Full Case Set。
_Avoid_: Full Case Set、随机样本

**Sampling Revision**：
某个 Attempt 创建时冻结的 Iteration Sample 版本；在途 Attempt 不随新反馈改变。
_Avoid_: Baseline Revision、Attempt ID

**Noise Tolerance**：
从独立配对测量的相对差异估算出的正常波动范围，用于区分改善、回退与测量噪声。
_Avoid_: 固定 1% Case 门槛、正确性容差

## 优化循环

**Attempt**：
一个整数 ID 标识的有边界优化尝试，拥有独立目录、Git 分支和一个或多个 Iteration Rounds，最终只能被接受或拒绝。
_Avoid_: UUID、Agent Session、Best Revision

**Iteration Round**：
Iteration Agent在某个 Base Best 上完成的一轮实现与采样验证；stale Attempt merge 新 Best 后形成下一 Round。
_Avoid_: Attempt、Backend Turn

**Accepted Attempt**：
通过 Full Case Set 正确性、硬性聚合门禁和 Integration Agent噪声判断，并推进 Best 的 Attempt。
_Avoid_: Iteration 成功、已提交分支

**Rejected Attempt**：
未推进 Best 的终态 Attempt；它只说明该次实现失败，不否定其优化方向。
_Avoid_: 无用方向、可删除历史

**Best Revision**：
Baseline Verification 或 Accepted Attempt 产生的最佳已知 Development 版本，作为新 Attempt 的起点。
_Avoid_: Optimization Target、远端 main

**Integration Queue**：
串行验证 ready Attempt 的 FIFO；队首 stale Attempt 返回 Iteration 刷新并保持队首位置。
_Avoid_: 并行 Merge、远端提交队列

**Iteration Guidance**：
用户提交、仅影响之后创建的 Iteration Agent 的版本化指导；已经运行的 Attempt 不热更新。
_Avoid_: Baseline Revision、即时 steer

## Agent 运行

**Agent Role**：
固定职责、工具权限、完成条件和 System Prompt 的静态工作契约；部分辅助 Role 可以不配置。
_Avoid_: Backend 配置、Agent Work

**Agent Work**：
一个 Agent Role 对某个已持久化领域对象承担的可恢复工作身份。
_Avoid_: Backend Session、通用任务记录

**Backend Session**：
由某个完整 Backend 配置启动的一次 provider 会话；恢复总是创建新 Session，并允许切换 Backend。
_Avoid_: Agent Work、provider resume

**Agent Symphony**：
根据领域投影创建、监督和恢复 Agent Work 的协调者；它只限制每个 Role 的并发，不决定领域资格或结果。
_Avoid_: 领域状态机、全局 Agent 容量池

**Follow-up Request**：
目标 Agent Turn 结束但没有完成终态 MCP 时，要求生成或直接发送下一条 User Turn 的持久化请求。
_Avoid_: Backend retry、用户消息

**Recovery Context**：
从数据库重建的历史消息与领域状态快照，供新 Backend Session 阅读并接管实际 Git 现场。
_Avoid_: provider resume、Git rollback

**Progress Summary Request**：
按计划冻结当前状态和增量消息、供可选 Progress Summary Agent生成 UI 摘要的持久化请求。
_Avoid_: Domain Event、日志压缩
