# Phase 2 — Alignment、Campaign Spec 与 Baseline

## 状态

Completed — 2026-08-19

## 依赖

[Phase 1](./01-workspace-persistence.md) Exit Gate 全通过。

## 目标

让用户通过独立 Alignment Conversation 与 Boundary Agent 定义计算边界、Reference、Harness、Cases、Metrics 和停止条件；用户显式确认后建立可信 Baseline、噪声带和首个 Profiler Artifact。

## 本 Phase 交付

- Alignment Backend Session 与 Boundary Role MCP。
- Campaign Spec draft/diff/确认与 Spec Revision。
- Atrex 16 项 Reference UI，默认全选。
- `ncu-report-skill` Skill Registry。
- Ref/Skill 在 Campaign 初始化时取最新 HEAD 并固定 SHA。
- setup worktree、protected Harness digest、Baseline 和 NCU Artifact。
- UI 原型中的 Alignment 页面 Phoenix/LiveView 版本。

## 实施顺序

### 2.1 补齐 Spec Schema

- [x] migrations：`spec_revisions`、`benchmark_cases`、`metric_definitions`、`best_revisions`、`best_metrics`。
- [x] migrations：`sampling_revisions`、`sampling_revision_cases`；初始 Sampling Revision 最多十个 Case。
- [x] Campaign 状态：DraftingSpec、AwaitingConfirmation、BuildingBaseline、Optimizing、AwaitingSpecConfirmation。
- [x] `submit_spec` 和 `submit_harness` MCP 实现。
- [x] 用户确认是 UI 动作，不是 Agent MCP 工具。

### 2.2 Alignment Conversation

- [x] 通过 `Pika.AgentBackend` 启动 Boundary Backend Session。
- [x] Alignment/setup merge/Baseline Prompt 从 Config 指向的独立 EEx 资源加载；启动 Turn 前校验资源。
- [x] Alignment Prompt 注入 repo 状态、目标硬件、已知输入与必需 MCP 操作，并提示用户选择合适而非固定的性能 Metrics。
- [x] Composer 把文字与测试脚本、pickle dump、JSONL 等附件作为同一消息原子登记；允许纯附件。
- [x] Backend Turn 缺少 `submit_spec`/`submit_harness` 时无限 follow-up。
- [x] UI 展示对话、Artifact、Spec draft 和结构化缺失项。

### 2.3 Reference Registry

严格按 [reference-registry.md](../../design/reference-registry.md)：

- [x] UI 展示 16 项，默认全部选中，允许取消。
- [x] 初始化时解析每个选中 repo 默认分支最新 HEAD。
- [x] 保存 URL、default branch 和完整 SHA。
- [x] 任何默认选中项目解析失败都阻止初始化，不静默跳过。
- [x] 重启和 Spec Revision 不刷新 SHA。

### 2.4 Skill Registry

- [x] 获取 `ncu-report-skill` 默认分支最新 HEAD 并固定 SHA。
- [x] Backend-specific discovery 路径指向同一份内容。
- [x] 不添加 Pika 硬件覆盖 Prompt。
- [x] Skill 目录位于禁止交付区域。

### 2.5 Setup Worktree 与 Protected Harness

- [x] 从 `pika/best` 创建 `pika/setup/<revision>`。
- [x] Boundary Agent 生成/修改 PyTorch Reference、正确性测试、Benchmark Harness。
- [x] 用户确认 Spec 后由 Agent merge 到 Best。
- [x] 记录 protected paths 和内容 digest。
- [x] protected path 的任何后续候选修改都可被稳定检测。

### 2.6 Baseline 与噪声

- [x] 验证所有 Benchmark Cases 正确性。
- [x] 每个 Case 按 Campaign Spec 中用户确认的 warmup 与正式 Pair 数进行交替自配对/重复测量。
- [x] Full Case Set 全部建立 Baseline 后，Baseline Agent 自动提交初始 Iteration Sample Set、逐项理由与成本摘要。
- [x] `submit_iteration_sample` 完成前停留在 `SelectingIterationSample`，不进入 Optimizing。
- [x] 使用中位数和 `max(0.5%, 3×1.4826×MAD)` 建立 noise tolerance。
- [x] 有效 Pair 少于用户在 Campaign Spec 中指定的 `min_valid_pairs` 时整组重跑一次；再次失败不进入 Optimizing。
- [x] 写入 Baseline Best Revision 和 Best Metrics。

### 2.7 Baseline Profiler

- [x] 必须生成一次 Profiler Artifact。
- [x] 记录工具、命令、目标 SHA、Case、目录与 Summary。
- [x] 原始文件在 `artifacts/profiles/`，SQLite 只存相对路径和哈希。
- [x] 验证 `ncu-report-skill` 能读取/解析该目录。

## 测试

- [x] 未确认 Spec 不能进入 BuildingBaseline。
- [x] 用户拒绝/修改后回到 DraftingSpec。
- [x] Ref 默认全选、取消选择和失败报告。
- [x] Campaign 内 Ref/Skill SHA 在重启后不变。
- [x] protected digest 对增加、删除、重命名、内容修改都敏感。
- [x] Metric direction、目标/保护/观察 Case 门禁。
- [x] Baseline 采样不足时的单次重跑。
- [x] 不同 Spec Revision 的 Metrics 不连成同一可比曲线。

## Exit Gate

- [x] 用户可完成一次真实的 Alignment → Confirm → Baseline 流程。
- [x] Spec、Harness、Ref/Skill SHA 和 Baseline Metrics 全部持久化。
- [x] Baseline Profiler Artifact 可由 Skill 工具解析。
- [x] 修改 protected Harness 的测试候选被拒绝。
- [x] 服务重启后从正确 Campaign 状态继续，不重新拉 Ref/Skill。

## 验收记录

实现、自动化覆盖、真实 Codex kick-off 语义和 H20 Profiler 解析证据见
[`artifacts/phase-2/acceptance-report.md`](../../../artifacts/phase-2/acceptance-report.md)。

## 非目标

- 并发 Iteration Attempt。
- Attempt Loop 与 Integration Full Regression。
- Sync 和完整 Metrics Timeline。
