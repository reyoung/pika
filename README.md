<p align="center">
  <img src="docs/design/prototype/public/og.png" alt="Pika — Kernel Optimization Agent" width="920" />
</p>

# Pika

Pika 是一个常驻的单租户 HTTP 服务，用多个 Coding Agent 并行完成 GPU Kernel 的定义、实现、测量和持续调优。

一次 Pika Server 运行对应一个仓库的一次完整 Optimization Campaign。每个性能尝试位于独立 Git worktree；通过正确性与 Pareto Metrics 门禁的结果由 Agent 串行 squash merge 到 Campaign Best Branch。SQLite、Git 和 Artifact Workspace 共同支持机器或进程崩溃后的自动恢复。

## 当前状态

**v1 设计已冻结；Phase 0 Backend conformance 与 Stage0 Alignment → H20 Baseline Preview 均已实现并通过真实验收。**

- 后端：Elixir/OTP、Phoenix、LiveView、SQLite WAL
- Agent 协议：Codex App Server 原生协议、Cursor ACP v1
- Agent 语义接口：角色化 Streamable HTTP MCP
- Agent Backend：Codex App Server、Cursor ACP，以及其他实现 `Pika.AgentBackend` conformance contract 的 Backend
- GPU 执行：由 Agent 直接驱动本机 GPU；Pika 不实现 GPU Worker
- 并发：显式 Iteration Agent Slots
- UI：目标对齐、Attempt/BTW 对话、Metrics Timeline

## 设计文档

- [设计索引](docs/design/README.md)
- [主设计](docs/design/kernel-optimization-agent.md)
- [领域语言](docs/design/CONTEXT.md)
- [架构与监督树](docs/design/architecture.md)
- [状态机与恢复](docs/design/state-machine.md)
- [SQLite Schema](docs/design/database-schema.md)
- [Pika MCP API](docs/design/mcp-api.md)
- [Reference 与 Skill Registry](docs/design/reference-registry.md)
- [实现与验收计划](docs/design/implementation-plan.md)
- [逐阶段实现清单](docs/v0/phases/README.md)
- [Architecture Decision Records](docs/design/adr/)

## Reference 与 Skill

Campaign Reference Catalog 采用 Atrex Kernel Agent `reference-projects/` 的 16 项 Kernel 仓库，Alignment UI 默认全选。Pika Server 初始化 Campaign 时获取这些 Ref 的最新默认分支 HEAD，随后在整个 Campaign 内固定 commit SHA。

Skill Registry 初始包含 [`mit-han-lab/ncu-report-skill`](https://github.com/mit-han-lab/ncu-report-skill)，并与 Kernel Ref 分开管理。

## UI 原型

已确认的交互原型位于 [`docs/design/prototype/`](docs/design/prototype/)：

```bash
cd docs/design/prototype
npm install
npm run dev
```

原型只用于设计评审，不是最终 Phoenix/LiveView 产品实现。

## Phase 0 协议 Spike

需要已登录的 Codex CLI 与 Cursor Agent，以及 Elixir/Erlang：

```bash
mix deps.get
mix test
mix run scripts/backend_smoke.exs -- \
  --backend all \
  --workspace /tmp/pika-backend-smoke
```

Smoke 会验证 Codex App Server、Cursor ACP、真实 HTTP MCP、Skill 可见性、steer、interrupt、子进程隔离与无 resume 恢复，并把脱敏证据写入 `artifacts/phase-0/`。

## Stage0 Alignment → Baseline Preview

Stage0 Demo 使用 Phase 0 Backend 打通内存态目标对齐、Campaign Spec/Harness 确认、Full Case Set Baseline 和初始 Iteration Sample 选择，不依赖 SQLite，也不派发优化 Attempt：

```bash
./bin/pika stage0-demo /absolute/path/to/clean/git/repo
```

命令固定源 repo 当前提交的 HEAD，并重新 clone 到独立 Workspace；dirty working tree 和 untracked 文件不会带入。临时 clone 中创建 `pika/best` 与 `pika/setup/1`，移除 `origin` 后再启动 Agent，并打印一次性 Token URL。源 repo 的原始 dirty 状态、分支、refs 和远端不会被修改。退出后内存状态丢失，但打印的 Workspace 与 Artifact 保留。可用参数：

```text
--backend codex|cursor  --model MODEL  --effort high
--host 127.0.0.1       --port 8080    --workspace EMPTY_DIR
--skill-root PATH
```

Alignment、setup merge 和 Baseline Prompt 是 `priv/prompts/stage0/*.md.eex` 独立资源；`config :pika, Pika.Stage0.PromptCatalog` 可以分别改为绝对路径。配置缺失或模板无法编译时，服务在启动 Backend Turn 前失败。

确定性全流程、两个真实 Backend 的 Boundary MCP smoke 及实现边界见 [Stage0 Demo 文档](docs/v0/phases/00b-stage0-alignment-baseline-demo.md)。

原始 Full Baseline H20 E2E 已在 WeLM v4.5 80A3 verify-attention 的固定 committed SHA 上通过：3 个 trace case、90/90 有效 Pair、3/3 correctness，以及 full/source NCU report。脱敏后的结构化结果位于 `artifacts/stage0-demo/welm-h20-gpu-e2e.json`；该历史证据早于 `submit_iteration_sample` 门禁，新的 Sampling 状态由单元/Fake Backend E2E 覆盖，完整 H20 流程需后续重新生成证据。

## v1 非目标

- 多租户、RBAC、计费和跨 Pika Server 调度
- Pika 自有 GPU Worker 或远程 GPU RPC
- 未实现 `Pika.AgentBackend` conformance contract 的 Agent，以及 Cursor ACP v2
- S3 Artifact、自动 Push、内置 daemon 和容器编排
