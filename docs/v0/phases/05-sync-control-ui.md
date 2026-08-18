# Phase 5 — Sync、人工控制与完整 UI

## 状态

Not started

## 依赖

[Phase 4](./04-integration-mainline.md) Exit Gate 全通过。

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

- [ ] `sync.remote`/`sync.branch` 从明确 upstream 默认，否则首次 Sync 前要求填写。
- [ ] UI 显示 remote、branch、remote SHA、Best SHA 和待 Push commit。
- [ ] 用户确认后才创建 Sync Run。
- [ ] Sync 期间设置 dispatch gate，禁止新 Attempt；已运行 Attempt 继续。

### 5.2 Sync 临时分支

- [ ] 从 Best 创建 `pika/sync/<sync-id>`。
- [ ] fetch 配置的 remote branch。
- [ ] Sync Agent merge 远端并解决冲突；禁止 rebase/force push。
- [ ] protected Harness/Spec 输入变化时进入 AwaitingSpecConfirmation。
- [ ] 用户确认新 Revision 后重建 Baseline，拒绝则 Sync failed、Best 不变。

### 5.3 Sync 验证与外部状态

- [ ] 完整正确性和 Best Metrics。
- [ ] Push 前持久化 Sync Intent。
- [ ] 普通 fast-forward push 临时 candidate 到配置远端分支。
- [ ] 远端成功后 fast-forward 本地 Best。
- [ ] 写 Sync Trail、Best Metrics 和 BestAdvanced。
- [ ] Push 失败保持本地 Best 不变。

### 5.4 Sync 崩溃恢复

- [ ] pushing 状态恢复时 fetch 远端。
- [ ] remote SHA == candidate SHA：补做本地 Best 和事务。
- [ ] remote SHA == before SHA：继续/重试 push。
- [ ] remote SHA 为第三值：Blocked，不猜测。
- [ ] 重复恢复不重复 Push 或 Best Revision。

### 5.5 Campaign 人工控制

- [ ] Pause：关闭新 Attempt dispatch，不取消在途 Agent/Integration/Validation。
- [ ] Stop Now：二次确认，ACP cancel 所有 Turn，停止归并和自动恢复，保留数据。
- [ ] Resume：恢复 Paused/Stopped 的 `resume_state`。
- [ ] Blocked 只能用户解决原因后显式恢复。
- [ ] 达到目标/max_attempts 进入 Draining；在途与 Validation/Revert 清空后 Completed。
- [ ] 删除 Workspace 仅 CLI，不放 Web UI。

### 5.6 Alignment UI

按已确认原型实现：

- [ ] 独立对话时间线。
- [ ] Artifact 卡片和 Shape 脚本/pickle/JSONL 输入。
- [ ] Campaign Spec 侧栏、Cases、Metrics、Ref 16 项默认全选。
- [ ] Spec diff 和明确确认按钮。

### 5.7 Attempt / BTW UI

- [ ] 并发 Attempt 列表、Slot、状态、Backend/模型。
- [ ] 单 Attempt ACP 文本、Plan、Tool Call、Diff、Terminal、BestAdvanced。
- [ ] 实时 Summary、Metrics、Artifacts。
- [ ] BTW 只能从当前 Attempt fork。
- [ ] 模式：仅对话、注入父 Attempt、注入后续 Attempts；默认仅对话。

### 5.8 Metrics UI

- [ ] 真实时间为 X 轴，每次尝试的最新 Metrics 为折线点。
- [ ] Metric/Case/Spec Revision 筛选；Revision 分段。
- [ ] hover 显示 Attempt、状态、时间、原始值、相对改善、Summary。
- [ ] 点选后显示 Patch、Profiler、Agent JSONL 和 Outcome。
- [ ] Mainline 校正覆盖对应点，不展示旧结构化快照。
- [ ] ECharts 采用稳定静态模块注册，避免运行时 dynamic import 缓存失败。

### 5.9 JSON API 与审计

- [ ] UI mutation 使用 idempotency key。
- [ ] HTTP Token 保护所有 JSON/LiveView 入口。
- [ ] 用户 Guidance、Spec confirmation、Pause/Stop/Resume/Sync 产生 Domain Event。
- [ ] API 不暴露 MCP Token、凭证或完整 Agent 环境。

## 测试

- [ ] Sync success、merge conflict、push reject、protected Harness change。
- [ ] remote push 后、本地 Best 前 kill -9。
- [ ] Pause 不取消在途工作；Stop 确实取消。
- [ ] Draining 不创建新 Attempt，Validation/Revert 清空后 Completed。
- [ ] LiveView reconnect 后按 Domain Event/JSONL seq 恢复。
- [ ] BTW 三种模式作用域。
- [ ] Metrics hover 每个点均包含 Summary。
- [ ] 浏览器没有未处理 Promise、console error 或错误 overlay。

## Exit Gate

- [ ] 用户可在 Web 完成 Alignment、观察 Attempts、BTW、Metrics、Pause/Stop/Resume/Sync。
- [ ] Sync 外部状态故障全部可恢复或 Blocked。
- [ ] UI 与 `docs/design/prototype/` 信息架构一致。
- [ ] 所有用户动作可审计且受 Token 保护。
- [ ] Campaign 可以稳定进入 Completed。

## 非目标

- 多租户、RBAC 和跨 Server Dashboard。
- S3 Artifact。
- 自动 Push、自动 Sync 或内置 daemon。
