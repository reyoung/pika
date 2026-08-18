---
status: accepted
---

# SQLite 保存当前状态与领域事件，Backend 原始流写 JSONL

Pika 在 SQLite WAL 中维护规范化当前状态表和追加式 `domain_events`，并在同一事务内提交状态、Operation Intent 与待广播事件；每个 Attempt 的结构化 Metrics 只保留最新值，事件表不复制被主线复验覆盖的旧快照。每个 Backend Session 的 provider 原始流写入独立 JSONL Artifact，SQLite 只索引相对路径、序号、时间、状态与摘要。Phoenix PubSub 在事务成功后实时广播标准化 Backend Event，刷新与恢复从 JSONL 尾部回放。

## Consequences

- SQLite 不承担高频 token、thinking、terminal output 或大型 Profiler Blob 的写入压力。
- UI 时间线来自 Domain Event，Agent 逐步输出来自 JSONL；二者必须使用稳定关联 ID。
- 崩溃恢复先读取 SQLite 当前状态与 Intent，再核对 Git 和对应 JSONL 尾部。
