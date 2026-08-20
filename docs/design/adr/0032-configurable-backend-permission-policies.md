---
status: accepted
---

# Agent Profile 显式配置 Backend Permission Policy

Pika 不再把所有 Backend Session 固定为自动批准和无 sandbox。每个 Agent Profile 都显式包含 provider-specific 的 `approval_policy` 与 `sandbox_policy`；`pika init` 根据已选择的 Agent Backend 只展示该 provider 支持的值，配置加载时也按 profile 的 Backend 独立校验。Alignment/Baseline Profile 与每个 Iteration Profile 可以使用不同策略，Integration 和 Sync 继承 Alignment/Baseline Profile。

Codex adapter 将 `never | on_request | untrusted` 映射为 App Server approval policy；后两者交给 provider 的 auto reviewer，任何仍回落到 Pika Client 的未决请求都会被拒绝。`danger_full_access | workspace_write | read_only` 映射为 thread 与 turn sandbox policy。Cursor adapter 将 `force | auto_review` 映射为 CLI 审批模式；auto review 未解决而继续发给 ACP Client 的权限请求会被拒绝。`disabled | enabled` 映射为 Cursor sandbox 开关。未声明字段的既有 Workspace 继续使用原行为：Codex 为 `never + danger_full_access`，Cursor 为 `force + disabled`。

Backend Permission Policy 属于可调整的 Agent Profile，不属于 Campaign Spec，也不触发 Spec Revision。恢复 Workspace 时可以修改它；adapter 在新建或恢复 Backend Session 时应用当前配置。它不能替代 Protected Harness、Git 门禁或主机隔离，也不能关闭 provider 自身不可配置的硬性执行规则。

本 ADR supersede ADR-0015 中“Pika v1 固定使用 YOLO”的部分，但不改变单租户 Pika 不是不可信代码安全边界的结论。

## Consequences

- 更严格的 sandbox 可能阻止 clone、依赖安装、编译、GPU 工具或 Workspace 外部路径访问；用户需为各角色选择满足工作负载的策略。
- provider 的策略枚举不强行抽象成统一等级；增加 Backend 时必须声明自己的合法值、默认值和协议映射。
- `danger_full_access` 只表示不启用 Codex sandbox，不保证所有 shell 写法都被接受；Codex exec policy 等 provider 内建规则仍可拒绝命令。
- 配置变更只影响之后打开或恢复的 Backend Session，不在运行中的 Turn 内热切换权限。
