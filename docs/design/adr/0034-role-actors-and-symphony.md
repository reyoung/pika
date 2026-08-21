---
status: accepted
---

# 用 Agent Role、Agent Actor 与 Symphony 统一 Agent 工作执行

Pika 将所有启动 Agent 的流程表示为静态 **Agent Role** 与持久化 **Agent Work** 的组合。Role
定义稳定身份、启动方式、Pika MCP tools、Agent Instructions 构造规则和基于 committed domain
facts 的完成投影；Role 不持有进程或 Session 状态。一次具体工作由临时 **Agent Actor** 执行，
Actor 至多拥有一个活动 Backend Session，并通过内部 Session Host 隐藏 profile 重载、Token、
Session 审计、Backend 事件、关闭和替换。

**Agent Symphony** 在启动恢复和 Domain Event 后向对应 domain lifecycle module 查询可运行的
Agent Work，并幂等创建 Actor。Symphony 只防止重复执行和施加技术容量限制；Attempt、Integration、
Sync 等领域流程仍决定 eligibility、顺序、Lease、Git、Metric 和 terminal outcome。系统不创建
通用 `actor_runs` 表作为新的持久化权威。

Actor 通过统一 Role Runtime 的 `prepare`、`invoke` 与 `progress` interface 工作。每个 Role module
实现共同的 Definition、system instructions、initial prompt 与 recovery prompt interface。Definition
中的 completion graph 只能读取 facts、组合 `all`/`any` 条件并投影建议 operation；它不能执行动作、
计时、重试或复制 domain state machine。统一 Pika MCP adapter 使用绑定 Actor、Role、Agent Work 与
Backend Session 的临时 Token，tool catalog 与授权来自 Role Definition。写操作的幂等身份为
`Role + Agent Work + operation + idempotency key`，而不是可替换的 Backend Session。

`await_user_kickoff | automatic` 是 Role 的静态启动方式，`fresh | recovering` 是 Session 的动态
模式。Campaign Kick-off 必须先持久化真实用户动作；系统指令、Session 创建和恢复都不能伪造用户
消息。活动 Session 固定其 Agent Profile、Instructions、tool catalog 与 Role contract revision；
新建或恢复 Session 重新读取最新有效的 `pika.yaml` 和 Workspace Role 模板，并保存最终 Instructions
摘要与快照。显式配置但无效的模板阻止对应 Actor 启动，缺少 override 才使用 built-in default。

恢复 Agent Work 时第一版总是创建新的 provider Session 与 Token，通过 committed facts、Artifact
和 recovery prompt 继续工作，不恢复 Codex thread 或 Cursor session。本决定修订 ADR-0028 中
“恢复优先使用 provider-native resume”的后果；领域正确性继续不依赖 provider resume。

## Consequences

- Alignment、Setup Merge、Baseline、Plan、Iteration、Integration、Sync 与 Progress Summary 都是
  独立 Role；Role 变化会创建新 Actor、Backend Session 与 Token。
- Progress Summary 先持久化 Progress Summary Request，再由 `submit_progress_summary` MCP operation
  提交结果；Pika 不再把 Backend 自然语言输出拼接成权威 Summary。
- Workspace 可以定制 Role 工作指导和 Agent Profile，但不能定义新 Role、扩大 MCP 权限或改变
  completion graph。Workspace Role 模板使用无代码执行的受限变量语法；Pika-owned 固定 Instructions
  资源和旧配置路径继续兼容，不把可执行 EEx 暴露为新的 Workspace 扩展面。
- Agent Session 身份、状态和 Instructions audit 保存在 `agent_sessions`；domain outcome 写入 Domain
  Events；provider 原始事件保留 JSONL；实时 Actor 状态使用结构化日志、Telemetry 与 PubSub。
- 数据库迁移只增加 nullable audit/work 字段与 work-scoped operation receipts；旧 Role ID、Prompt
  路径、`required_operations_json` 和旧 Workspace 保持可读，不自动改写用户模板。
- 迁移按 Progress Summary、Sync、Integration、Plan/Iteration、Alignment/Setup Merge/Baseline 顺序
  进行。同一 Agent Work 在任一时刻只能由 legacy Coordinator 或新 Actor 之一拥有。
- 上述迁移完成后，前五类 Role 强制归 Actor 所有，避免过期的 `legacy` override 让 Work 无执行者；
  Alignment、Setup Merge、Baseline 暂时保留 role-by-role legacy selector，因为同一版本仍带有对应兼容
  adapter。旧 `boundary` Session 只用于判断 recovery mode，不会被改写或当作新 Role Session 复用。
- 每个 Role 必须通过共同 conformance suite；恢复测试覆盖 Session 打开与 MCP commit 前后。普通测试
  使用同一 BEAM 内的 fake AgentBackend，不为每个 case 启动独立 provider 或 OS 进程。
