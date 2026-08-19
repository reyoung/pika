<p align="center">
  <img src="docs/design/prototype/public/og.png" alt="Pika — Kernel Optimization Agent" width="920" />
</p>

# Pika

Pika 是一个常驻的单租户 HTTP 服务，用多个 Coding Agent 并行完成 GPU Kernel 的定义、实现、测量和持续调优。

一次 Pika Server 运行对应一个仓库的一次完整 Optimization Campaign。每个性能尝试位于独立 Git worktree；通过正确性与 Pareto Metrics 门禁的结果由 Agent 串行 squash merge 到 Campaign Best Branch。SQLite、Git 和 Artifact Workspace 共同支持机器或进程崩溃后的自动恢复。

## 当前状态

**v1 设计已冻结；Backend conformance、Workspace 持久化、Alignment → H20 Baseline、Attempt Loop、Integration、人工 Sync 与 Control UI 均已实现。**

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

## Backend 协议一致性验证

需要已登录的 Codex CLI 与 Cursor Agent，以及 Elixir/Erlang：

```bash
mix deps.get
mix test
mix run scripts/backend_smoke.exs -- \
  --backend all \
  --workspace /tmp/pika-backend-smoke
```

Smoke 会验证 Codex App Server、Cursor ACP、真实 HTTP MCP、Skill 可见性、steer、interrupt、子进程隔离与无 resume 恢复，并把脱敏证据写入 `artifacts/backend-conformance/`。

## Alignment → Baseline Preview

Preview 使用统一 Agent Backend 打通内存态目标对齐、Campaign Spec/Harness 确认、Full Case Set Baseline 和初始 Iteration Sample 选择，不依赖 SQLite，也不派发优化 Attempt：

```bash
./bin/pika preview /absolute/path/to/clean/git/repo
```

命令固定源 repo 当前提交的 HEAD，并重新 clone 到独立 Workspace；dirty working tree 和 untracked 文件不会带入。临时 clone 中创建 `pika/best` 与 `pika/setup/1`，移除 `origin` 后再启动 Agent，并打印一次性 Token URL。源 repo 的原始 dirty 状态、分支、refs 和远端不会被修改。退出后内存状态丢失，但打印的 Workspace 与 Artifact 保留。可用参数：

```text
--backend codex|cursor  --model MODEL  --effort high
--host 127.0.0.1       --port 8080    --workspace EMPTY_DIR
--skill-root PATH
```

Alignment、setup merge 和 Baseline Agent Instructions 是 `priv/prompts/alignment/*.md.eex` 独立资源；`config :pika, Pika.PromptCatalog` 可以分别改为绝对路径。它们作为 Backend 系统级上下文注入，不占用首条用户 Prompt，也不自动 Kick-off；Campaign 由用户首条消息启动，用户确认 Spec 的动作继续驱动 setup merge 与 Baseline。配置缺失或模板无法编译时，服务在打开 Backend Session 前失败。

确定性全流程、两个真实 Backend 的 Boundary MCP smoke 及实现边界见 [Stage0 Demo 文档](docs/v0/phases/00b-stage0-alignment-baseline-demo.md)。

原始 Full Baseline H20 E2E 已在 WeLM v4.5 80A3 verify-attention 的固定 committed SHA 上通过：3 个 trace case、90/90 有效 Pair、3/3 correctness，以及 full/source NCU report。脱敏后的结构化结果位于 `artifacts/stage0-demo/welm-h20-gpu-e2e.json`；该历史证据早于 `submit_iteration_sample` 门禁和用户拥有 Campaign Kick-off 的新语义，新的 Sampling/Kick-off 状态由协议测试与 Fake Backend E2E 覆盖，完整 H20 流程需后续重新生成证据。

## 初始化持久化 Workspace

`pika init` 提供交互式向导，分别询问 Alignment/Baseline Agent 与 Iteration Agent 的 Backend，以及 Repo 模式、Workspace 路径、监听地址、Iteration Agent 模型与并发数、推理强度、最大 Attempt 数和可选 Git Sync。两个阶段可以独立选择 Codex 或 Cursor。初始化会生成 `pika.yaml`、可编辑的完整 Prompt 模板、固定 Workspace 布局与 Git `pika/best` 分支，但不会启动 Server：

选择 Iteration Agent 模型时，向导会从当前已登录的 Codex App Server 或 Cursor CLI 动态读取模型列表，显示常用候选、provider 默认值和自定义 model id 入口。使用 `--model MODEL` 可直接进行非交互选择，`--yes` 则保留 provider 默认值。

```bash
pika init
```

也可以先指定 Workspace，或者通过参数完成非交互初始化：

```bash
pika init /absolute/path/to/pika-workspace \
  --repo /absolute/path/to/clean/git/repo \
  --alignment-backend cursor \
  --iteration-backend codex \
  --iteration-agents 2 \
  --effort high \
  --no-sync \
  --yes
```

兼容参数 `--backend codex|cursor` 会同时设置两类 Backend；任一专用参数都可以覆盖对应阶段。

Managed Repo 的默认 Workspace 位于目标仓库旁的 `.pika-workspaces/<repo-name>`，避免 Pika 状态污染目标仓库。初始化结束后进入 Workspace，直接运行 `pika serve` 即可；用 `pika init --help` 查看全部参数。

## 持久化 Server

使用 `pika init` 初始化后，在 Workspace 根目录直接启动：

```bash
cd /absolute/path/to/pika-workspace
pika serve
```

`serve` 会自动使用当前目录与其中的 `pika.yaml`。从其他目录启动或使用外部配置时，仍可显式传参：

复制并编辑示例 YAML，然后以前台进程启动一个 Owned Repo Workspace：

```bash
cp config/pika.example.yaml /tmp/pika.yaml
./bin/pika serve \
  --workspace /absolute/path/to/pika-workspace \
  --config /tmp/pika.yaml
```

显式管理一个已有且 clean 的本地 Git 仓库时增加：

```text
--repo /absolute/path/to/repository
```

此模式会在 Workspace 的 `repo/` 建立软链接，并在目标仓库 Git common directory 的 `.pika.lock` 上持续持有 OS advisory lock。第二个 Pika 进程不能同时接管该仓库。

正式 Release 同样提供前台 `serve` 命令：

```bash
MIX_ENV=prod mix release
_build/prod/rel/pika/bin/pika serve \
  --workspace /absolute/path/to/pika-workspace \
  --config /tmp/pika.yaml
```

每次启动只打印一次带 256-bit Token 的 URL。浏览器访问后 Token 会换成 `HttpOnly`、`SameSite=Strict` Cookie 并立即从地址栏移除；JSON API、SSE 和 `/mcp` 使用 Bearer Token。重启会使旧 Token 与 Cookie 失效。

固定 Workspace 布局为：

```text
workspace/
├── repo/
├── attempts/
├── prompts/{alignment,attempt,integration,sync}/
├── artifacts/{plans,patches,profiles,prompts,logs}/
├── pika.sqlite3
├── pika.yaml
└── config.json
```

`config.json` 是经过字段校验的有效配置快照；Workspace、Repo identity、监听地址和 Backend 协议配置不可变。历史故障验收证据保留在 `artifacts/`。

## v1 非目标

- 多租户、RBAC、计费和跨 Pika Server 调度
- Pika 自有 GPU Worker 或远程 GPU RPC
- 未实现 `Pika.AgentBackend` conformance contract 的 Agent，以及 Cursor ACP v2
- S3 Artifact、自动 Push、内置 daemon 和容器编排
