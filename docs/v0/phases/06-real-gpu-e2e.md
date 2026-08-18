# Phase 6 — 真实 GPU End-to-End 验收

## 状态

Not started

## 依赖

[Phase 5](./05-sync-control-ui.md) Exit Gate 全通过。

## 目标

在真实 NVIDIA GPU 主机上完成一次可审计的完整 Campaign，证明 Pika 不只在 Fake Harness 下成立。该 Phase 是 v0/v1 发布门禁，不是可选演示。

## 环境要求

- Linux x86_64 NVIDIA GPU 主机。
- 可用 CUDA、Driver、Python、PyTorch、Git、NCU。
- Codex 与 Cursor ACP Backend 均已登录。
- 测试仓库和远端分支允许手工 Sync push。
- 选定一个规模适中、能在预算内产生 Accepted/Rejected 的 Kernel 问题。

## 建议测试任务

优先选择 H20/B200 上的 Decode Attention、GEMM epilogue 或小型 fused operator：

- PyTorch Reference 清晰。
- 至少 3 个生产代表 Shape。
- Latency 为目标 Metric，TFLOPS/Peak memory 为保护或展示 Metric。
- 能使用 CUTLASS/FlashInfer/Triton 等默认 Ref。
- 不依赖多节点通信，避免把远程 Worker 混入 v1 验收。

## 实施顺序

### 6.1 环境证据

- [ ] 保存 GPU 型号、Driver、CUDA、PyTorch、NCU、Git、Agent Backend 版本。
- [ ] 保存 `pika serve` 有效配置的脱敏快照。
- [ ] 验证 HTTP Token、Workspace lock 和 preflight。

### 6.2 Alignment 与 Baseline

- [ ] 通过 Alignment Conversation 生成/确认 Reference、Harness、Fusion 边界。
- [ ] 定义多个 Target/Guard/Informational Cases。
- [ ] UI 默认全选 16 个 Ref；记录最终选择和固定 SHA。
- [ ] 固定 `ncu-report-skill` SHA并验证 Agent 可读。
- [ ] 完成 Baseline 正确性、配对测量、噪声和 Profiler。

### 6.3 并发优化

- [ ] 至少两个 Iteration Slots，分别使用 Codex 和 Cursor。
- [ ] 至少运行 3 个 Attempts。
- [ ] 至少产生一个 Accepted 和一个 Rejected。
- [ ] 验证最近历史注入、Mailbox 和 BestAdvanced stale refresh。
- [ ] 检查 Ref/Skill 未进入 squash。

### 6.4 中途故障恢复

- [ ] 在一个 Agent 运行中 kill Agent 进程并恢复同一 Attempt。
- [ ] 在另一个 Attempt 运行中 kill Pika Server 并重启。
- [ ] 验证 Attempt 预算、worktree、JSONL、Metrics 和 Session identity 没有重复。
- [ ] 如启用 Mainline Validation，至少执行一次复验；可用受控测试验证 Revert。

### 6.5 手工 Sync

- [ ] 远端先产生一个可合并更新。
- [ ] UI 确认 remote、branch、待 Push commit。
- [ ] Sync Agent fetch/merge/validate/push/advance Best。
- [ ] 写 Sync Trail、更新 Best Metrics、广播 BestAdvanced。
- [ ] 验证其他运行 Attempt 在正式动作前刷新。

### 6.6 完成与清理

- [ ] 达到 max_attempts 或目标后进入 Draining。
- [ ] 排空 Integration、Validation、Revert。
- [ ] Campaign 进入 Completed。
- [ ] 终态 worktree/branch 按配置清理。
- [ ] Patch、Summary、Metrics、Profiler、JSONL 和 Best Branch 保留。

## 必须保存的证据

```text
artifacts/phase-6/
├── environment.md
├── effective-config.redacted.json
├── campaign-spec.json
├── reference-snapshot.json
├── baseline/
├── attempts/
├── recovery/
├── sync/
└── final-report.md
```

`final-report.md` 至少包含：

- Base/Final SHA。
- 每个 Attempt 的 Outcome 和 Metric delta。
- 正确性结果。
- 配对样本数、MAD、noise tolerance。
- Accepted/Rejected 的原因。
- Agent/Server 故障点和恢复证据。
- Sync remote before/after SHA。
- 资源清理结果。

## 验收标准

- [ ] 工作负载真实运行，不能只有任务提交记录。
- [ ] 所有命令保存 exit status 和可见 workload 输出。
- [ ] Baseline、Accepted、Rejected 和 Final Best Metrics 可追溯到 SHA。
- [ ] Agent 和 Pika 崩溃后均自动恢复。
- [ ] Codex/Cursor 两个 Backend 都实际修改或分析过代码。
- [ ] `ncu-report-skill` 生成或解析了真实 NCU Artifact。
- [ ] Sync 完整执行并有远端 SHA 证据。
- [ ] Pika UI 的 Metrics Timeline 与 SQLite/Artifact 一致。
- [ ] 无遗留活跃 worktree、Integration Lease 或未完成 Intent。

## Exit Gate

Phase 6 全部验收标准通过，且 [implementation-plan.md](../../design/implementation-plan.md) 的 v1 完成定义全部满足，才可以发布 v1。

## 非目标

- 多节点、跨机 GPU Worker 或 Kubernetes。
- 多租户负载与权限测试。
- 覆盖所有 Kernel 类型；一个完整真实 Campaign 足以作为 v1 门禁。
