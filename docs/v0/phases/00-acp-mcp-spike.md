# Phase 0 — ACP + MCP 协议 Spike

## 状态

Not started

## 目标

证明 Elixir 可以用同一个 `Pika.ACPClient` Behaviour 稳定驱动 Codex 和 Cursor，并让两个 Backend 都调用 Pika 提供的 Streamable HTTP MCP。该 Phase 只消除协议风险，不实现 Campaign、SQLite、Git worktree 或 GPU 调优。

## 前置条件

- Linux 或 macOS 开发机。
- Elixir/Erlang、Node.js、Codex CLI、Cursor Agent 已安装并登录。
- 能运行：

  ```bash
  cursor-agent acp --help
  npx -y @agentclientprotocol/codex-acp --help
  ```

## 本 Phase 交付

```text
mix.exs
config/
lib/
  pika/application.ex
  pika/acp_client.ex                 # Behaviour
  pika/acp/client.ex                 # Elixir adapter
  pika/acp/session.ex                # 一个受监督 Session
  pika/acp/backend_profile.ex
  pika/acp/jsonl_writer.ex
  pika/mcp/probe_router.ex           # 最小 Streamable HTTP MCP
  pika/mcp/probe_state.ex
test/
  support/fake_acp_server.exs
  pika/acp/conformance_test.exs
  pika/acp/codex_integration_test.exs
  pika/acp/cursor_integration_test.exs
scripts/
  acp_smoke.exs
```

允许使用现有 `agent_client_protocol` 0.x 包，但只能通过 `Pika.ACPClient` 访问。如果缺少 capability，可在仓库内受控 fork；领域代码不得 import 上游协议 struct。

## 实施顺序

### 0.1 建立最小 OTP/Phoenix 骨架

- [ ] 创建单一 Elixir app，保留未来 Phoenix Endpoint 的目录结构。
- [ ] 配置 `Pika.Application` Supervisor。
- [ ] 只加入 Phase 0 必需依赖；暂不创建 Campaign schema。
- [ ] 加入 formatter、Credo 或等价静态检查，以及 ExUnit。

验证：

```bash
mix format --check-formatted
mix test
```

### 0.2 定义 `Pika.ACPClient`

Behaviour 至少暴露：

```elixir
start_link(profile, handlers)
initialize(client_pid, capabilities)
new_session(client_pid, cwd, mcp_servers)
prompt(client_pid, session_id, content)
cancel(client_pid, session_id)
close_session(client_pid, session_id)
stop(client_pid)
```

- [ ] 定义 Pika 自有数据结构和错误类型。
- [ ] 统一请求 ID、超时、取消和 Agent→Client incoming request。
- [ ] 未知 notification 记录但不崩溃。
- [ ] capability 缺失返回结构化错误，不在调用方写 Backend 分支。

### 0.3 实现 ACP subprocess 管理

- [ ] 使用 Elixir `Port` 启动 ACP Server，绝不通过 PTY 抓屏。
- [ ] 每个 Session/Backend 使用独立受监督进程。
- [ ] stderr 写独立日志，stdout 只解析 JSON-RPC。
- [ ] 进程退出向父进程发送包含 exit status 的结构化事件。
- [ ] permission request 默认 approve-all。
- [ ] ACP 原始输入/输出追加到 JSONL；尾部半行不会破坏前序记录。

### 0.4 建立 Backend Profiles

```yaml
codex:
  command: npx
  args: ["-y", "@agentclientprotocol/codex-acp"]

cursor:
  command: cursor-agent
  args: ["acp"]
```

- [ ] Backend Profile 支持 command、args、env allowlist、model、reasoning effort。
- [ ] 不把登录 Token 或完整环境写进日志。
- [ ] `initialize` 后记录实际 capabilities 以便测试。

### 0.5 实现最小 MCP Probe

只实现两个工具：

- `get_probe_context()`：返回固定 Session identity 和 nonce。
- `complete_probe(idempotency_key, nonce, summary)`：记录一次结构化完成。

- [ ] 通过 loopback Streamable HTTP 暴露。
- [ ] 每个 ACP Session 使用不同 Bearer Token。
- [ ] Token 只存哈希，明文只存在 Session 内存和 ACP 配置。
- [ ] 相同 idempotency key + 相同请求返回旧响应；不同请求冲突。

### 0.6 验证 Skill 可见性

- [ ] 获取 `ncu-report-skill` 默认分支最新 HEAD，并记录完整 SHA。
- [ ] 把同一份 Skill 内容分别暴露给 Codex 和 Cursor Session。
- [ ] Prompt 要求 Agent 返回读取到的 Skill 名称，并调用 `complete_probe`。
- [ ] 不要求 Agent 真正运行 NCU。

### 0.7 共用 Conformance Suite

同一组测试必须分别套用 Codex 和 Cursor Profile：

- [ ] initialize 与 version/capability negotiation。
- [ ] session/new 并传入 cwd + HTTP MCP。
- [ ] session/prompt 与流式 session/update。
- [ ] Agent 实际调用 `get_probe_context`/`complete_probe`。
- [ ] permission 自动批准。
- [ ] session/cancel 返回 cancelled，而不是未知异常。
- [ ] session/close 释放资源。
- [ ] 强杀 Agent subprocess，只终止对应 Session。
- [ ] stderr 噪声不会污染 JSON-RPC parser。
- [ ] JSONL 可以完整回放已接收事件。

## Smoke 命令

实现一个面向开发者的命令：

```bash
mix run scripts/acp_smoke.exs -- \
  --backend codex \
  --workspace /tmp/pika-acp-smoke
```

输出必须包含 Backend、协商版本、Session ID、收到的事件类型、MCP 完成结果和退出状态；不得打印 Token。

## Exit Gate

以下条件全部满足才可进入 Phase 1：

- [ ] Codex 与 Cursor 运行同一套 conformance tests，全部通过。
- [ ] 两个 Backend 均实际调用 HTTP MCP，而非测试替身。
- [ ] cancel、close、进程异常退出都有确定结果。
- [ ] `ncu-report-skill` 对两个 Backend 可见，SHA 已固定。
- [ ] 领域调用方只依赖 `Pika.ACPClient`，没有 Backend-specific 分支。
- [ ] 保存一份 `artifacts/phase-0/conformance.json` 和两个 Backend JSONL 日志作为证据。

## 非目标

- Campaign/Attempt 状态机。
- SQLite 业务 schema。
- Git worktree、Ref Registry、Kernel Benchmark。
- 完整 Phoenix UI、启动 Token 和自动恢复。
- ACP resume；即使 Backend 支持，也不能成为正确性依赖。
