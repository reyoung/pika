# Phase 5 — Sync、人工控制与完整 UI

## 状态

Implementation complete (2026-08-19); browser console acceptance pending

## 依赖

[Phase 4](./04-integration-full-regression.md) Exit Gate 全通过。

## 目标

完成本地产品体验：手工双向 Sync、Campaign 停止/恢复、完整 Phoenix/LiveView UI，以及已确认的 Metrics Timeline 和 hover Summary。

## 本 Phase 交付

- Sync Agent、临时分支、远端 push 与 crash recovery。
- Pause、Stop Now、Resume、Blocked、Draining、Completed。
- Alignment Conversation 页面。
- Attempt Conversation、BTW fork 和 Agent 状态页面。
- Metrics Timeline、筛选、点选详情和 hover Summary。
- JSON API 与 UI 操作审计。

## 实施顺序

### 5.1 Sync 配置与确认

- [x] `sync.remote`/`sync.branch` 从明确 upstream 默认，否则首次 Sync 前要求填写。
- [x] UI 显示 remote、branch、remote SHA、Best SHA 和待 Push commit。
- [x] 用户确认后才创建 Sync Run。
- [x] Sync 期间设置 dispatch gate，禁止新 Attempt；已运行 Attempt 继续。

### 5.2 Sync 临时分支

- [x] 从 Best 创建 `pika/sync/<sync-id>`。
- [x] fetch 配置的 remote branch。
- [x] Sync Agent merge 远端并解决冲突；禁止 rebase/force push。
- [x] protected Harness/Spec 输入变化时进入 AwaitingSpecConfirmation。
- [x] 用户确认新 Revision 后重建 Baseline，拒绝则 Sync failed、Best 不变。

### 5.3 Sync 验证与外部状态

- [x] 完整正确性和 Best Metrics。
- [x] Push 前持久化 Sync Intent。
- [x] 普通 fast-forward push 临时 candidate 到配置远端分支。
- [x] 远端成功后 fast-forward 本地 Best。
- [x] 写 Sync Trail、Best Metrics 和 BestAdvanced。
- [x] Push 失败保持本地 Best 不变。

### 5.4 Sync 崩溃恢复

- [x] pushing 状态恢复时 fetch 远端。
- [x] remote SHA == candidate SHA：补做本地 Best 和事务。
- [x] remote SHA == before SHA：继续/重试 push。
- [x] remote SHA 为第三值：Blocked，不猜测。
- [x] 重复恢复不重复 Push 或 Best Revision。

### 5.5 Campaign 人工控制

- [x] Pause：关闭新 Attempt dispatch，不取消在途 Agent/Integration。
- [x] Stop Now：二次确认，调用 `AgentBackend.interrupt` 终止所有活跃 Turn，停止归并和自动恢复，保留数据。
- [x] Resume：恢复 Paused/Stopped 的 `resume_state`。
- [x] Blocked 只能用户解决原因后显式恢复。
- [x] 达到目标/max_attempts 进入 Draining；在途 Attempt/Integration 清空后 Completed。
- [x] 删除 Workspace 仅 CLI，不放 Web UI。

### 5.6 Alignment UI

按已确认原型实现：

- [x] 独立对话时间线。
- [x] Artifact 卡片和 Shape 脚本/pickle/JSONL 输入。
- [x] Alignment 右侧使用目标边界、Metrics、Benchmark Cases、测量与采样规则、Reference Projects 五段折叠验收单；Ref 16 项默认全选。
- [x] Composer Enter 发送、Shift+Enter 换行、IME 安全，成功清空/失败保留，附件随消息提交。
- [x] Cases 显示 Full Case Set 数量、当前 Sampling Revision、Iteration Sample/Full Regression Only 标签与选择理由。
- [x] Spec diff 和明确确认按钮。

### 5.7 Attempt / BTW UI

- [x] 并发 Attempt 列表、Slot、状态、Backend/模型。
- [x] 单 Attempt 标准化 Backend Event：文本、Plan、Tool Call、Diff、Terminal、BestAdvanced。
- [x] 实时 Summary、Metrics、Artifacts。
- [x] BTW 只能从当前 Attempt fork。
- [x] 模式：仅对话、注入父 Attempt、注入后续 Attempts；默认仅对话。

### 5.8 Metrics UI

- [x] 真实时间为 X 轴，每次尝试的最新 Metrics 为折线点。
- [x] Metric/Case/Spec Revision 筛选；Revision 分段。
- [x] hover 显示 Attempt、状态、时间、原始值、相对改善、Summary。
- [x] 点选后显示 Patch、Profiler、Agent JSONL 和 Outcome。
- [x] Integration Screening/Full 结果覆盖对应点并补齐未采样 Case；展示 `iteration`/`integration_screen`/`integration_full` 来源。
- [x] ECharts 采用稳定静态模块注册，避免运行时 dynamic import 缓存失败。

### 5.9 JSON API 与审计

- [x] UI mutation 使用 idempotency key。
- [x] HTTP Token 保护所有 JSON/LiveView 入口。
- [x] 用户 Guidance、Spec confirmation、Pause/Stop/Resume/Sync 产生 Domain Event。
- [x] API 不暴露 MCP Token、凭证或完整 Agent 环境。

## 测试

- [x] Sync success、merge conflict、push reject、protected Harness change。
- [x] remote push 后、本地 Best 前故障注入。
- [x] Pause 不取消在途工作；Stop 确实取消。
- [x] Draining 不创建新 Attempt，Attempt/Integration 清空后 Completed。
- [x] LiveView reconnect 后按 Domain Event/JSONL seq 恢复。
- [x] BTW 三种模式作用域。
- [x] Metrics hover 每个点均包含 Summary。
- [ ] 浏览器没有未处理 Promise、console error 或错误 overlay。

## Exit Gate

- [x] 用户可在 Web 完成 Alignment、观察 Attempts、BTW、Metrics、Pause/Stop/Resume/Sync。
- [x] Sync 外部状态故障全部可恢复或 Blocked。
- [x] UI 与 `docs/design/prototype/` 信息架构一致。
- [x] 所有用户动作可审计且受 Token 保护。
- [x] Campaign 可以稳定进入 Completed。

## 验收证据

- `mix test test/pika/sync_control_test.exs`：`10 passed`；覆盖 Attempt/Integration/Sync Stop 和远端第三 SHA Blocked。
- `mix test test/pika_web/control_live_test.exs test/pika_web/alignment_live_test.exs`：`10 passed`。
- 全量回归 `123 passed, 1 excluded`；`mix assets.build` 通过。
- 浏览器 acceptance 未勾选：本次会话没有可绑定的内置浏览器实例，无法核实 console 与 overlay；不得用其他浏览器表面伪造该项。

## 非目标

- 多租户、RBAC 和跨 Server Dashboard。
- S3 Artifact。
- 自动 Push、自动 Sync 或内置 daemon。
