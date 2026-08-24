# Pika v2 设计审阅稿

## 状态

本目录描述 Pika 下一版本的目标架构。产品与领域决策已经过逐轮确认；代码尚未按本设计完成重构，因此本文档不能作为 v2 已交付的证明。

v2 是破坏性版本：不兼容 v1 Workspace、SQLite schema、配置、MCP 或 CLI。实现切换后使用最终领域名，不保留 `Pika.V2.*` namespace，也不并行维护两套运行时。

## 核心变化

- 一个 Pika 进程只管理一个 Optimization 和一个 Git 仓库。
- 删除 Campaign、Sync、Plan 和 Setup Merge 概念；Pika 不负责最终远端合并或 Push。
- Agent Symphony 只协调 Agent Work，不拥有领域判断。
- 工作流固定为 Baseline Alignment → Baseline Verify → 并发 Iteration → 串行 Integration。
- Optimization Target 与 Development Baseline 是独立身份，但允许初始代码相同。
- 所有大型上下文和结果通过本地文件传递；Prompt 与 MCP 参数只携带路径和小型身份字段。
- Attempt 使用单调递增整数 ID；stale Attempt 回到 Iteration 并 merge 当前 Best。
- Follow-up、恢复和定时 Progress Summary 使用数据库中的标准化 Turn 重建 `messages.jsonl`。

## 阅读顺序

1. [领域语言](./CONTEXT.md)
2. [代码架构](./architecture.md)
3. [状态机](./state-machine.md)
4. [配置](./configuration.md)
5. [文件、Harness 与 MCP 契约](./protocols.md)
6. [SQLite 数据模型](./database-schema.md)
7. [System Prompt 审阅索引](./system-prompts/README.md)

## 非目标

- 多 Optimization、多仓库或跨进程统一调度。
- Pika 自有 GPU Worker、远程执行协议或容器调度。
- Pika 内置的远端 Merge、Push 或 Sync。
- 依赖 Codex/Cursor provider session resume 实现恢复。
- v1 数据在线迁移或兼容层。
