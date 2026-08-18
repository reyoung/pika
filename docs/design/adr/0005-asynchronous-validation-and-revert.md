---
status: accepted
---

# 主线复验固定版本并异步撤销

每个主线复验任务在独立 worktree 中固定测试对应候选的 squash commit SHA，因此即使 Campaign Best Branch 继续接受后续 Merge，复验结果仍能归属于原候选。复验失败后，系统向全部 Agent 发布 `RevertRequired`，主线 Agent取得唯一归并锁，在 Campaign Best Branch 最新 HEAD 上创建 revert commit、解决冲突并验证；正常复验不阻塞归并，但无法在预算内安全撤销时，调优任务进入 `Blocked` 并停止新 Merge。这个设计保留并行吞吐量，同时用可审计、不可改写历史的方式处理迟到失败。

## Consequences

- 复验工作区固定在被测 SHA，而撤销工作区必须基于 Campaign Best Branch 最新 HEAD。
- 已运行或排队的候选可能跨越一个迟到 revert，归并前必须刷新上下文并重新测试。
- `Blocked` 不丢弃已有工作，但在人工或后续恢复动作解决 Campaign Best Branch 前禁止继续推进最佳已知版本。
