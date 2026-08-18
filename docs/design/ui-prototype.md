# Pika UI 可交互原型

> 状态：已确认。2026-08-18 用户认可目标对齐、Attempt/BTW 和 Metrics 三个视图的当前交互。

## 目的

该原型用于在实现 Phoenix/LiveView 产品前评审 Pika 的信息结构、对话层级和 Metrics 表达。它不是产品实现，也不包含真实 Agent、SQLite、Git 或 GPU 行为。

## 视图

1. **目标对齐**：独立 Alignment Conversation、Shape 采集 Artifact、Campaign Spec 草稿和显式确认入口。
2. **Attempts**：并发 Attempt 列表、单个 ACP Session 工作流、实时摘要，以及只能从当前 Attempt fork 的 BTW Conversation。
3. **Metrics**：以时间为横轴的多 Metric 折线、最新值切换、Attempt 状态和点选详情。

## BTW 行为

BTW Drawer 继承父 Attempt 当前摘要。Composer 默认只在 BTW 中对话；用户可以显式切换为注入父 Attempt 或注入后续 Attempts。原型刻意不提供跨 Attempt 多选。

## Shape 输入

Alignment Conversation 展示了由 Boundary Agent 生成采集脚本的路径。实际产品也允许用户提供 pickle dump 或 JSONL；输入格式由对齐过程决定，最终结果必须进入待确认的 Campaign Spec。

## 本地预览

原型是独立 Sites/Vinext 工程，目录为 `docs/design/prototype/`。开发服务启动后访问 `http://localhost:3000/`。

## 评审重点

- Alignment Conversation 与 Attempt Conversation 是否应该共享更多导航或上下文。
- Attempt 工作流、工具调用和 Artifact 的信息密度是否合适。
- BTW fork 与三种消息模式是否足够清楚。
- Metrics 是否应该默认显示原始单位，还是像当前原型一样显示相对 Baseline 改善比例。
