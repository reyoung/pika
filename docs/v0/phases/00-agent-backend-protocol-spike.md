# Phase 0 — Agent Backend 协议 Spike

## 状态

Not started

## 目标

证明 Elixir 可以用统一 `Pika.AgentBackend` Behaviour 分别驱动 Codex 原生 App Server 与 Cursor ACP，并把 provider-specific 消息转换为同一事件与 Pika MCP 完成语义。该 Phase 只消除协议风险，不实现 Campaign、SQLite、Git worktree 或 GPU 调优。

## 前置条件

- Linux 或 macOS 开发机。
- Elixir/Erlang、Codex CLI、Cursor Agent 已安装并登录。
- 能运行：

  ```bash
  codex app-server --help
  codex app-server generate-json-schema --help
  cursor-agent acp --help
  ```

## 本 Phase 交付

```text
mix.exs
config/
lib/
  pika/application.ex
  pika/agent_backend.ex
  pika/agent_backend/event.ex
  pika/agent_backend/session.ex
  pika/agent_backend/profile.ex
  pika/agent_backend/jsonl_writer.ex
  pika/agent_backend/codex_app_server.ex
  pika/agent_backend/cursor_acp.ex
  pika/mcp/probe_router.ex
  pika/mcp/probe_state.ex
test/
  support/fake_agent_backend.exs
  pika/agent_backend/conformance_test.exs
  pika/agent_backend/codex_protocol_test.exs
  pika/agent_backend/cursor_protocol_test.exs
scripts/
  backend_smoke.exs
```

Codex adapter 直接实现 App Server stdio JSON-RPC；Cursor adapter 可以使用现有 Elixir ACP 库或受控 fork。领域代码不得 import 任一 provider 的 wire struct。

## 实施顺序

### 0.1 建立最小 OTP/Phoenix 骨架

- [ ] 创建单一 Elixir app，保留未来 Phoenix Endpoint 目录结构。
- [ ] 配置 `Pika.Application` Supervisor。
- [ ] 只加入 Phase 0 必需依赖；暂不创建 Campaign schema。
- [ ] 加入 formatter、静态检查和 ExUnit。

### 0.2 定义 `Pika.AgentBackend`

Behaviour 固定为：

```elixir
start_link(profile, event_sink)
open_session(cwd, model, reasoning_effort, mcp, skill_roots)
start_turn(session, input)
steer(session, input)
interrupt(session)
close_session(session)
capabilities(session)
```

- [ ] 定义 Pika 自有 Session ID、Turn ID、capability 和错误类型。
- [ ] 标准化事件：session/turn start、message delta、plan、tool lifecycle、command output、file change、usage、turn complete、backend error、process exit。
- [ ] `event_sink` 接收标准事件，不接收 provider wire message。
- [ ] capability 缺失返回结构化错误，不在调用方写 Codex/Cursor 分支。
- [ ] `steer` 失败统一返回 `steer_failed`。

### 0.3 通用 subprocess 与 JSONL

- [ ] 使用 Elixir `Port`，绝不通过 PTY 抓屏。
- [ ] 每个 Backend Session 使用独立受监督进程。
- [ ] stderr 写独立日志；stdout 只解析对应协议 JSONL。
- [ ] provider 原始输入/输出追加到 Session JSONL。
- [ ] 尾部半行不会破坏前序记录。
- [ ] 进程退出产生标准 `process_exited`，只影响对应 Session。

### 0.4 Codex App Server Adapter

启动命令：

```text
codex app-server --listen stdio://
```

- [ ] 执行 `initialize`，随后发送 `initialized` notification。
- [ ] `open_session` 映射到 `thread/start`，设置 cwd、model、reasoning effort、approvalPolicy=never 和 YOLO/sandbox 配置。
- [ ] `start_turn` 映射到 `turn/start`。
- [ ] `steer` 映射到原生 `turn/steer`，不 interrupt 当前 Turn。
- [ ] `interrupt` 映射到 `turn/interrupt`。
- [ ] `close_session` 终止该独立 App Server 进程；不依赖 thread resume。
- [ ] `item/*`、`turn/*`、command/file/tool/usage 通知转换成标准事件。
- [ ] 未知稳定通知记录但不崩溃；实验 API 默认不启用。
- [ ] 运行 `codex app-server generate-json-schema`，把 CLI 版本、schema 哈希和 required method 检查保存到 Phase Artifact。

Pika MCP 通过进程级 config override 注入：

```text
mcp_servers.pika.url=<loopback URL>
mcp_servers.pika.bearer_token_env_var=PIKA_MCP_TOKEN
mcp_servers.pika.required=true
```

明文 Token 只放在该 App Server 进程环境中。

Skill 注入：

- [ ] 调用 `skills/extraRoots/set` 或 `skills/list` 的 extra roots。
- [ ] `turn/start` 输入附带明确的 `skill` item。

### 0.5 Cursor ACP Adapter

启动命令：

```text
cursor-agent acp
```

- [ ] ACP initialize/version/capabilities。
- [ ] `open_session` 映射到 `session/new`，传入 cwd、HTTP MCP 与 Skill roots。
- [ ] `start_turn` 映射到 `session/prompt`。
- [ ] `steer` 先 `session/cancel`，等待 cancelled，再在同一 Session 发送 follow-up Prompt。
- [ ] `interrupt` 映射到 `session/cancel`。
- [ ] `close_session` 映射到 `session/close`。
- [ ] `session/update`、Plan、Tool Call、Diff、Terminal 与 usage 转换成标准事件。

### 0.6 最小 Pika MCP Probe

只实现：

- `get_probe_context()`：返回固定 Backend Session identity 和 nonce。
- `complete_probe(idempotency_key, nonce, summary)`：记录一次结构化完成。

- [ ] loopback Streamable HTTP。
- [ ] 每个 Backend Session 不同 Bearer Token。
- [ ] Token 只存哈希。
- [ ] 相同 idempotency key + 相同请求返回旧响应；不同请求冲突。
- [ ] 两个 Backend 实际调用同一 MCP 工具，不使用 provider-specific tool。

### 0.7 验证 Skill 可见性

- [ ] 获取 `ncu-report-skill` 默认分支最新 HEAD 并记录完整 SHA。
- [ ] 同一份 Skill 内容分别注入 Codex 与 Cursor Backend Session。
- [ ] Prompt 要求读取 Skill 名称并调用 `complete_probe`。
- [ ] 不要求真正运行 NCU。

### 0.8 Provider 协议测试

Codex：

- [ ] initialize/initialized。
- [ ] thread/start、turn/start、流式 item/turn events。
- [ ] turn/steer 不产生 interrupted completion。
- [ ] turn/interrupt 产生确定的 interrupted 结果。
- [ ] skills 与 required HTTP MCP。
- [ ] schema 生成与 required methods 校验。

Cursor：

- [ ] initialize、session/new、session/prompt、session/update。
- [ ] cancel + follow-up Prompt 的 steer 模拟。
- [ ] session/cancel、session/close。
- [ ] Skill roots 与 HTTP MCP。

### 0.9 统一 Conformance Suite

同一组测试套用两个 Backend：

- [ ] open session 与 start turn。
- [ ] 标准事件顺序和 stable identity。
- [ ] 实际 MCP completion。
- [ ] permission 自动批准。
- [ ] steer、interrupt 和 close 的统一结果。
- [ ] 强杀子进程只终止对应 Session。
- [ ] JSONL 可以回放 provider 原始消息并重建标准游标。
- [ ] Backend 崩溃后创建新 Session，不调用 provider resume。

## Smoke 命令

```bash
mix run scripts/backend_smoke.exs -- \
  --backend codex_app_server \
  --workspace /tmp/pika-backend-smoke
```

输出必须包含 Backend、协议版本、Session/Turn ID、标准事件类型、MCP 完成结果和退出状态；不得打印 Token。

## Exit Gate

- [ ] Codex 与 Cursor 各自协议测试全部通过。
- [ ] 两个 Backend 的统一 conformance suite 全通过。
- [ ] 两个 Backend 均实际调用 HTTP MCP。
- [ ] Codex 原生 steer、Cursor 模拟 steer、两者 interrupt 都有确定结果。
- [ ] `ncu-report-skill` 对两个 Backend 可见，SHA 已固定。
- [ ] 领域调用方只依赖 `Pika.AgentBackend`，没有 provider 分支。
- [ ] 保存 `artifacts/phase-0/conformance.json`、Codex schema 证据和两个 Backend JSONL。

## 非目标

- Campaign/Attempt 状态机。
- SQLite 业务 schema。
- Git worktree、Ref Registry、Kernel Benchmark。
- 完整 Phoenix UI、启动 Token和自动恢复。
- Codex SDK sidecar、`codex exec --json`、Codex thread resume 或 ACP resume 正确性依赖。
