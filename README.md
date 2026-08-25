<p align="center">
  <img src="docs/assets/pika.png" alt="Pika — Kernel Optimization Agent" width="920" />
</p>

# Pika

Pika 是单仓库、单优化目标的长期运行 Code Agent 调优服务。上层 Symphony 调度八种 Role；只有 Iteration 可以按配置并发，其他已启用 Role 的并发度都是 1。

工作流固定为：

```text
Baseline Alignment → 用户 Review → Baseline Verify
                  → 并发 Iteration → 串行 Integration → Best
```

Pika 只维护 Workspace 内部的 `pika/best`。最终合并到用户分支或推送远端由 Pika 之外的独立 Code Agent 完成。

## 快速开始

要求：Elixir 1.18+、Git，以及已安装的 `codex` 或 `cursor-agent`。

```bash
mix deps.get
cd assets && npm ci && cd ..
mix assets.build
mix release

./_build/prod/rel/pika/bin/pika init /absolute/path/to/workspace \
  --repo /absolute/path/to/clean/git/repo

./_build/prod/rel/pika/bin/pika serve \
  --workspace /absolute/path/to/workspace
```

`pika init` 默认启动交互式向导。向导首先生成一个随机 256-bit 访问 token，可直接回车接受，也可输入固定 token；最终 token 会写入权限为 `0600` 的 Workspace `pika.yaml`，因此重启 `pika serve` 后保持不变。手工修改 token 后需要重启 `pika serve`。每个必选 Role、每个 Iteration Agent，以及启用的可选 Role 都可以分别选择 Codex/Cursor、provider 返回的完整模型列表和 reasoning effort。脚本中可加 `--yes`，用命令行参数和默认值非交互初始化。

在 Workspace 目录内运行 `pika reconfiguration`（也可用 `pika reconfigure`）可以交互式修改某个 Role 或全部 Agent 配置；修改只影响之后创建的 Session。`pika init` 和 `pika reconfiguration --help` 列出了相应的非交互参数。

初始化会生成完整展开的 v2 `pika.yaml`。也可以从 [config/pika.example.yaml](config/pika.example.yaml) 开始；YAML anchor 只负责书写复用，运行时不存在 Agent Profile registry。

## 开发时自动重载

从源码 checkout 启动时，可以开启开发重载：

```bash
./bin/pika serve --workspace /absolute/path/to/workspace --reload
```

`--autoreload` 是同义选项。该模式会在请求时热编译 `lib/` 和 `priv/v2/` 下的 Elixir
代码、持续构建 `assets/js` 与 `assets/css`，并在这些源码变化后自动刷新浏览器。它只适用于
源码 checkout，release 中不会启用；`mix.exs`、依赖、运行时配置或 supervision tree 的结构
变化仍需手工重启。

## 核心契约

- Baseline 同时定义 Optimization Target 与 Development Baseline；两者可以相同，但必须分别记录。
- 标准脚本为 `verify_cases.sh` 与 `benchmark_cases.sh`，支持 `--list-cases` 和 `--case-id 0,1,...`。
- 大型 Definition、Result、Metrics 与历史通过文件传递，并由 Pika 重算 digest、Schema、Metrics 和 Git identity。
- Attempt 使用从 1 开始且永不复用的整数 ID。每个 Attempt 都保留 `message.jsonl` 和 `summary.jsonl`。
- Iteration 基于创建时的 Best；到达 FIFO 队首后若 Base 已 stale，会回到新的 Iteration Round merge 当前 Best。
- Integration 覆盖 Full Case Set；任何 `pika/best` mutation 之前必须获得持久化 Git Intent。
- Backend 崩溃后创建新 Session，不使用 provider resume。恢复信息来自 SQLite 生成的 `messages.jsonl` 与 `recovery-NN/recovery.json`，Git 现场保持原样。
- Follow-up Role 可省略；省略时直接发送“继续”。Baseline Verify 耗尽使进程失败，Iteration/Integration 耗尽拒绝 Attempt。
- Progress Summary 默认可每五分钟生成一次，按日期分片保存；运行一个月不会清理历史目录。

## 开发验证

```bash
mix format --check-formatted
mix compile --warnings-as-errors
mix test
```

权威架构、状态机、配置、数据库和 MCP 约束位于 [docs/design/v2/README.md](docs/design/v2/README.md)。
