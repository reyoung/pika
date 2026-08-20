---
status: superseded by ADR-0031
---

# 参考仓库以临时 submodule 注入但不得成为依赖

Campaign 通过 Reference Catalog 固定参考仓库的 URL、commit、`ref/` 目录名和简介，并在 setup 与 Iteration worktree 中以临时 Git submodule 注入。Pika 注入的 `ref/**` 和 `.gitmodules` 增量不得进入 Campaign Best Branch；编码 Agent 在归并前必须移除它们、保留用户原有 submodule，并在无参考仓库的状态下重新运行正确性和 Benchmark。这样 Agent 能阅读精确版本的开源实现，同时不会把调优交付物绑定到参考仓库。

## Consequences

- Squash 不能直接包含候选分支的全部文件，必须显式排除 Pika 注入的 gitlink 与 `.gitmodules` 增量。
- 候选代码不能 import、链接或在运行时读取 `ref/`；违反者即使性能更好也必须拒绝。
- Reference Catalog 更新不需要修改源仓库原分支，但会影响之后 Agent 的可用上下文。
