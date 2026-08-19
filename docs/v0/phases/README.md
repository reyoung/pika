# Pika v0 Implementation Phases

这组文档把冻结的 v1 设计拆成可顺序实施的 v0 工程阶段。任何 Phase 未达到 Exit Gate 时，不应开始下一个 Phase。

## 顺序

| Phase | 文档 | 核心结果 | 状态 |
|---:|---|---|---|
| 0 | [Agent Backend 协议 Spike](./00-agent-backend-protocol-spike.md) | Codex App Server + Cursor ACP 的统一 AgentBackend | Done (`40e71e1`) |
| Preview | [Stage0 Alignment → Baseline Demo](./00b-stage0-alignment-baseline-demo.md) | 内存态目标对齐、Spec/Harness 确认、真实 GPU Baseline 接口 | Done，H20 E2E passed |
| 1 | [Workspace 与持久状态](./01-workspace-persistence.md) | 可启动、可恢复的单 Campaign Server | Not started |
| 2 | [Alignment、Spec 与 Baseline](./02-alignment-baseline.md) | 用户确认边界并建立可信 Baseline | Done，持久化 E2E、真实 Codex Alignment 与 H20 Profiler parse passed |
| 3 | [并发 Attempt Loop](./03-attempt-loop.md) | 多 Agent 并行优化与 MCP 完成协议 | Not started |
| 4 | [Integration Full Regression](./04-integration-full-regression.md) | 串行 Best、全量回归、Sampling Feedback | Not started |
| 5 | [Sync、控制与完整 UI](./05-sync-control-ui.md) | 可操作的完整本地产品 | Not started |
| 6 | [真实 GPU E2E](./06-real-gpu-e2e.md) | 真实 NVIDIA Campaign 验收 | Not started |

## 使用方式

1. 开始某个 Phase 前，先确认所有依赖 Phase 的 Exit Gate 已通过。
2. 在对应文档的 Checklist 中逐项实现，不把后续 Phase 功能提前塞入。
3. 将测试证据、关键命令和实际输出保存在 Phase 要求的位置。
4. Phase 完成后，把本表状态更新为 `Done`，并记录通过的 commit SHA。
5. 设计冲突时以 [`docs/design/`](../../design/README.md) 和 accepted ADR 为准；不要在代码中自行发明新语义。

## 全阶段规则

- 不用 mock 替代 Codex App Server、Cursor ACP、MCP、Git 或最终 GPU 关键路径；Fake 只用于确定性故障与并发测试。
- 每个外部状态变更先持久化 Operation Intent。
- Agent 自然语言、退出码和 Backend Turn 结束都不是领域完成信号；MCP 完成调用才是。
- 任何无法由 SQLite + Git + Artifact 唯一解释的恢复状态进入 Blocked。
- 不提交凭证、Token、Agent 登录状态、Profiler 大文件或临时 worktree。
- 不使用未固定版本的运行依赖或 `latest` 容器标签。

## 关联设计

- [主设计](../../design/kernel-optimization-agent.md)
- [架构](../../design/architecture.md)
- [状态机](../../design/state-machine.md)
- [SQLite Schema](../../design/database-schema.md)
- [MCP API](../../design/mcp-api.md)
- [Reference/Skill Registry](../../design/reference-registry.md)
- [实现总计划](../../design/implementation-plan.md)
