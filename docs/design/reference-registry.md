# Pika Reference 与 Skill Registry

## 1. 边界

Pika 架构参考不属于 Campaign Reference Catalog，也不会进入 Attempt worktree。Reference Catalog 只包含供 Kernel Agent 阅读的实现仓库；Skill Registry 则包含供 Backend Session 调用的外部 Skill。两者相互独立，且都不能进入候选 Patch 或 Campaign Best Branch。

## 2. 内置 Reference Catalog

初始清单与 [Atrex Kernel Agent `reference-projects/`](https://github.com/alibaba/atrex-kernel-agent/tree/main/reference-projects) 一致。

| ID / `ref/` 目录 | URL | Atrex 描述 |
|---|---|---|
| `cutlass` | `https://github.com/NVIDIA/cutlass.git` | NVIDIA CUTLASS，CUDA 线性代数模板 |
| `cutex` | `https://github.com/deciding/cutex.git` | CUDA Template Extensions |
| `cuLA` | `https://github.com/inclusionAI/cuLA.git` | inclusionAI CUDA Linear Algebra |
| `flash-attention` | `https://github.com/Dao-AILab/flash-attention.git` | Flash Attention |
| `flashinfer` | `https://github.com/flashinfer-ai/flashinfer.git` | LLM Serving Kernel Library |
| `FlyDSL` | `https://github.com/ROCm/FlyDSL.git` | ROCm FlyDSL |
| `triton` | `https://github.com/triton-lang/triton.git` | Triton Language and Compiler |
| `DeepGEMM` | `https://github.com/deepseek-ai/DeepGEMM.git` | DeepSeek DeepGEMM |
| `LeetCUDA` | `https://github.com/xlite-dev/LeetCUDA.git` | CUDA Learning |
| `FlashMLA` | `https://github.com/deepseek-ai/FlashMLA.git` | DeepSeek FlashMLA |
| `composable_kernel` | `https://github.com/ROCm/composable_kernel.git` | ROCm Composable Kernel |
| `cute-gemm` | `https://github.com/reed-lau/cute-gemm.git` | CuTe GEMM Examples |
| `hpc-ops` | `https://github.com/Tencent/hpc-ops.git` | Tencent HPC Ops |
| `aiter` | `https://github.com/ROCm/aiter.git` | ROCm AIter |
| `quack` | `https://github.com/Dao-AILab/quack.git` | Dao-AILab Quack |
| `tilelang` | `https://github.com/tile-ai/tilelang.git` | TileLang |

## 3. UI 选择

- Alignment UI 显示 16 项清单，默认全部选中。
- 用户可以在确认 Campaign Spec 前取消任意项目。
- Campaign Spec 保存选中项目 ID、URL、说明、解析分支和完整 commit SHA。
- Prompt 只包含选中项目的说明及 `ref/<id>` 本地路径。
- 未选项目不初始化 submodule，也不出现在 Agent Context。

## 4. 版本解析

Pika Server 首次初始化 Campaign 时，对所有选中项目执行以下操作：

1. 查询 Registry URL 的默认分支。
2. 获取默认分支最新 HEAD。
3. 保存完整 commit SHA，不保存模糊 branch-only 版本。
4. 用该 SHA 初始化 setup 和 Attempt worktree 中的临时 submodule。

Campaign 生命周期内 SHA 不变。服务重启、普通 Sync、Spec Revision 和后续 Attempt 都不刷新；启动另一个 Pika Server 才重新解析最新 HEAD。若首次解析或 clone 失败，Campaign 停在初始化阶段并显示项目级错误，不能悄悄跳过默认选中的 Ref。

## 5. Skill Registry

v1 初始 Skill：

| ID | URL | 用途 |
|---|---|---|
| `ncu-report-skill` | `https://github.com/mit-han-lab/ncu-report-skill` | Nsight Compute harness、采集、报告解析、六维分析、诊断 playbook 与优化报告 |

Skill 与 Ref 使用同一版本生命周期：Campaign 初始化时获取默认分支最新 HEAD 并固定完整 SHA；恢复、Sync 和 Attempt 不更新。Pika 不给该 Skill 追加硬件覆盖 Prompt，Agent 按上游 `SKILL.md` 自身说明区分通用流程和 B200/sm_100 专属内容。

## 6. 注入方式

- Reference 项目以 Git submodule 形式出现在 `ref/<id>`。
- Skill 保存在 Workspace 的 Pika-owned Skill 目录，通过 Agent Backend 的 Skill 发现机制或明确路径提供给 Backend Session。
- Codex App Server 使用 `skills/extraRoots/set`/`skills/list`，并通过系统级 `developerInstructions` 告知 Agent 固定 Skill 的路径；Cursor ACP 使用其 Backend discovery 配置。Backend-specific 入口可以不同，但必须指向同一份固定 SHA 内容。Skill 发现信息和 Pika Prompt 都不能成为或改写首条用户消息，Campaign kick-off 只能由用户发起。
- `ref/**`、Skill 目录及 Pika 产生的 `.gitmodules` 增量都是禁止交付区域。
- Integration Agent 在正式配对测量前移除它们并恢复基础 `.gitmodules`，验证候选没有运行时依赖。
