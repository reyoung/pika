# Pika UI 可交互原型

> 状态：已确认，并于 2026-08-19 增加 Full Case Set、Iteration Sample Set 和归并前全量回归表达。

目标对齐视图已有 Phoenix/LiveView 的 Stage0 内存 Preview 实现；Attempt/BTW 与 Metrics Timeline 仍只有本目录中的交互原型。Stage0 Preview 不改变 Phase 1/2 的 SQLite 与恢复门禁。

## 目的

该原型用于在实现 Phoenix/LiveView 产品前评审 Pika 的信息结构、对话层级和 Metrics 表达。它不是产品实现，也不包含真实 Agent、SQLite、Git 或 GPU 行为。

## 视图

1. **目标对齐**：独立 Alignment Conversation 与五段验收单；右侧依次展示目标边界、Metrics、具体 Cases、测量/采样规则和 Reference Projects。
2. **Attempts**：并发 Attempt 列表、单个 Backend Session 工作流、标准化 Backend Event、实时摘要，以及只能从当前 Attempt fork 的 BTW Conversation。
3. **Metrics**：以时间为横轴的多 Metric 折线、最新值切换、Attempt 状态和点选详情。

## BTW 行为

BTW Drawer 继承父 Attempt 当前摘要。Composer 默认只在 BTW 中对话；用户可以显式切换为注入父 Attempt 或注入后续 Attempts。原型刻意不提供跨 Attempt 多选。

## Shape 输入

Alignment Conversation 展示了由 Boundary Agent 生成采集脚本的路径。实际产品也允许用户提供 pickle dump 或 JSONL；输入格式由对齐过程决定，最终结果必须进入待确认的 Campaign Spec。

## Benchmark 覆盖表达

- Cases 区分 `ITERATION SAMPLE` 与 `FULL REGRESSION ONLY`，并显示 Full/Sample 数量、Sampling Revision 和选择理由。
- 初始 Baseline 覆盖 Full Case Set 的全部 Case/Metric 30 Pair；Baseline Agent 自动选择最多十个初始 Iteration Cases。
- Attempt 视图固定显示其启动 Sampling Revision。Sampling Advanced 到达时提示活动 Agent，但不改变该 Attempt 的门禁快照。
- Metrics Timeline 的点标明 `iteration`、`integration_screen` 或 `integration_full`；Integration 结果覆盖同 Attempt 的旧快照并补齐未采样 Case。
- Informational 项在 Iteration 中只展示，但归并前全量回归仍受 universal no-regression gate。

## Composer

消息和附件使用同一表单；Enter 发送、Shift+Enter 换行、IME 组词不误发送。发送成功后清空，失败时保留输入。工具/命令/MCP 继续使用灰色折叠 Activity，Agent 正文使用安全 Markdown。

## 本地预览

原型是独立 Sites/Vinext 工程，目录为 `docs/design/prototype/`。开发服务启动后访问 `http://localhost:3000/`。

## 评审重点

- Alignment Conversation 与 Attempt Conversation 是否应该共享更多导航或上下文。
- Attempt 工作流、工具调用和 Artifact 的信息密度是否合适。
- BTW fork 与三种消息模式是否足够清楚。
- Metrics 是否应该默认显示原始单位，还是像当前原型一样显示相对 Baseline 改善比例。
