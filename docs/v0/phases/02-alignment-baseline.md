# Phase 2 — Alignment、Campaign Spec 与 Baseline

## 状态

Not started

## 依赖

[Phase 1](./01-workspace-persistence.md) Exit Gate 全通过。

## 目标

让用户通过独立 Alignment Conversation 与 Boundary Agent 定义计算边界、Reference、Harness、Cases、Metrics 和停止条件；用户显式确认后建立可信 Baseline、噪声带和首个 Profiler Artifact。

## 本 Phase 交付

- Alignment ACP Session 与 Boundary Role MCP。
- Campaign Spec draft/diff/确认与 Spec Revision。
- Atrex 16 项 Reference UI，默认全选。
- `ncu-report-skill` Skill Registry。
- Ref/Skill 在 Campaign 初始化时取最新 HEAD 并固定 SHA。
- setup worktree、protected Harness digest、Baseline 和 NCU Artifact。
- UI 原型中的 Alignment 页面 Phoenix/LiveView 版本。

## 实施顺序

### 2.1 补齐 Spec Schema

- [ ] migrations：`spec_revisions`、`benchmark_cases`、`metric_definitions`、`best_revisions`、`best_metrics`。
- [ ] Campaign 状态：DraftingSpec、AwaitingConfirmation、BuildingBaseline、Optimizing、AwaitingSpecConfirmation。
- [ ] `submit_spec` 和 `submit_harness` MCP 实现。
- [ ] 用户确认是 UI 动作，不是 Agent MCP 工具。

### 2.2 Alignment Conversation

- [ ] 启动 Boundary Agent Profile 的 ACP Session。
- [ ] Prompt 注入 repo 状态、目标硬件、已知输入与必需 MCP 操作。
- [ ] 支持上传/登记测试脚本、pickle dump、JSONL 等 Artifact。
- [ ] Prompt Turn 缺少 `submit_spec`/`submit_harness` 时无限 follow-up。
- [ ] UI 展示对话、Artifact、Spec draft 和结构化缺失项。

### 2.3 Reference Registry

严格按 [reference-registry.md](../../design/reference-registry.md)：

- [ ] UI 展示 16 项，默认全部选中，允许取消。
- [ ] 初始化时解析每个选中 repo 默认分支最新 HEAD。
- [ ] 保存 URL、default branch 和完整 SHA。
- [ ] 任何默认选中项目解析失败都阻止初始化，不静默跳过。
- [ ] 重启和 Spec Revision 不刷新 SHA。

### 2.4 Skill Registry

- [ ] 获取 `ncu-report-skill` 默认分支最新 HEAD 并固定 SHA。
- [ ] Backend-specific discovery 路径指向同一份内容。
- [ ] 不添加 Pika 硬件覆盖 Prompt。
- [ ] Skill 目录位于禁止交付区域。

### 2.5 Setup Worktree 与 Protected Harness

- [ ] 从 `pika/best` 创建 `pika/setup/<revision>`。
- [ ] Boundary Agent 生成/修改 PyTorch Reference、正确性测试、Benchmark Harness。
- [ ] 用户确认 Spec 后由 Agent merge 到 Best。
- [ ] 记录 protected paths 和内容 digest。
- [ ] protected path 的任何后续候选修改都可被稳定检测。

### 2.6 Baseline 与噪声

- [ ] 验证所有 Benchmark Cases 正确性。
- [ ] 每个 Case warmup 10，进行 30 个交替 Pair 的自配对/重复测量。
- [ ] 使用中位数和 `max(0.5%, 3×1.4826×MAD)` 建立 noise tolerance。
- [ ] 有效 Pair 少于 24 时整组重跑一次；再次失败不进入 Optimizing。
- [ ] 写入 Baseline Best Revision 和 Best Metrics。

### 2.7 Baseline Profiler

- [ ] 必须生成一次 Profiler Artifact。
- [ ] 记录工具、命令、目标 SHA、Case、目录与 Summary。
- [ ] 原始文件在 `artifacts/profiles/`，SQLite 只存相对路径和哈希。
- [ ] 验证 `ncu-report-skill` 能读取/解析该目录。

## 测试

- [ ] 未确认 Spec 不能进入 BuildingBaseline。
- [ ] 用户拒绝/修改后回到 DraftingSpec。
- [ ] Ref 默认全选、取消选择和失败报告。
- [ ] Campaign 内 Ref/Skill SHA 在重启后不变。
- [ ] protected digest 对增加、删除、重命名、内容修改都敏感。
- [ ] Metric direction、目标/保护/观察 Case 门禁。
- [ ] Baseline 采样不足时的单次重跑。
- [ ] 不同 Spec Revision 的 Metrics 不连成同一可比曲线。

## Exit Gate

- [ ] 用户可完成一次真实的 Alignment → Confirm → Baseline 流程。
- [ ] Spec、Harness、Ref/Skill SHA 和 Baseline Metrics 全部持久化。
- [ ] Baseline Profiler Artifact 可由 Skill 工具解析。
- [ ] 修改 protected Harness 的测试候选被拒绝。
- [ ] 服务重启后从正确 Campaign 状态继续，不重新拉 Ref/Skill。

## 非目标

- 并发 Iteration Attempt。
- Integration Best 合并与 Mainline Validation。
- Sync 和完整 Metrics Timeline。
