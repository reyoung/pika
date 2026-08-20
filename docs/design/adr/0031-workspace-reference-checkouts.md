---
status: accepted
---

# Reference 使用 Workspace Checkout 而不是 Git submodule

每个已选 Reference Project 在 Campaign Workspace 的 `refs/<id>` 中维护一个固定 SHA 的独立 Git clone；setup 和 Attempt worktree 只通过被 Git 忽略的 `ref/<id>` 软链接读取它。Reference 因而不再修改产品仓库的 index 或 `.gitmodules`，同时 `ref/**` 仍被 setup 验证、候选 Patch 和最终 squash 明确排除。

## Consequences

- 多个 Attempt 共享同一份只读 Reference Checkout，不再重复 clone 或 add/remove submodule。
- 用户仓库原有 `.gitmodules` 不再因 Pika Reference 注入而受特殊限制。
- 正式验证前仍须移除 `ref/` 视图，以拒绝对 Reference 的构建或运行时依赖。
