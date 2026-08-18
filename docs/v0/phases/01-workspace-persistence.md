# Phase 1 — Workspace、启动与持久状态

## 状态

Not started

## 依赖

[Phase 0](./00-agent-backend-protocol-spike.md) Exit Gate 全通过。

## 目标

交付一个可以在单机前台启动、管理一个 Campaign Workspace、持久化单例 Campaign，并在 kill -9 后安全恢复的 Pika Server。此时还不实现 Alignment Agent 或 Kernel 优化。

## 本 Phase 交付

- `pika serve` CLI 与 Elixir Release。
- YAML 配置解析、不可变/可变字段划分、`config.json` 有效配置快照。
- Owned Repo 与 Managed Repo 两种 Workspace 模式。
- Managed Repo advisory lock 与软链接身份校验。
- SQLite WAL、初始 migrations、Artifact Store、Domain Event outbox。
- 256-bit 启动 Token、HTML/API/LiveView/MCP 完整鉴权。
- Phoenix/LiveView 状态 shell 与启动诊断页面。
- 无 Agent 工作时的自动恢复骨架。

## 实施顺序

### 1.1 CLI 与配置

- [ ] 实现 `pika serve --workspace --repo --config --host --port`。
- [ ] 默认 `host=127.0.0.1`，默认端口由配置确定。
- [ ] YAML 校验错误逐字段报告，不能静默使用猜测值。
- [ ] 解析后写入 Workspace `config.json`，保存 SHA-256。
- [ ] 恢复时比较不可变字段：Workspace、Managed Repo、listen address、Backend type/command/protocol config。
- [ ] 允许未来修改的字段：Plan、max attempts、mainline validation、history N、Reference Catalog、停止条件。

### 1.2 Workspace 初始化

固定布局：

```text
workspace/
├── repo/
├── attempts/
├── artifacts/{plans,patches,profiles,prompts,logs}/
├── pika.sqlite3
└── config.json
```

- [ ] 空目录初始化；合法 Pika Workspace 恢复；其他非空目录拒绝。
- [ ] Owned Repo 在 `repo/` 创建实际仓库。
- [ ] Managed Repo 使用 `repo/` 软链接。
- [ ] 所有 Artifact API 只接受规范化相对路径并拒绝 `..` 逃逸。

### 1.3 Managed Repo 所有权

- [ ] 首次接管要求 working tree 和 index clean。
- [ ] 在 Git common directory 建立锁文件并持续持有 OS advisory lock。
- [ ] 锁诊断信息包含 Server UUID、PID、启动时间、Workspace canonical path。
- [ ] 恢复时校验 symlink canonical path、device/inode、Git common directory。
- [ ] 第二个 Pika Server 对同一 repo 启动时失败且不修改任何文件。

### 1.4 SQLite 基础

采用 [database-schema.md](../../design/database-schema.md) 的 schema，但本 Phase 先落：

- [ ] `campaigns`
- [ ] `artifacts`
- [ ] `operation_intents`
- [ ] `domain_events`
- [ ] `agent_sessions` 的最小恢复字段
- [ ] `idempotency_records`

连接设置：`foreign_keys=ON`、WAL、`synchronous=FULL`、`busy_timeout=5000`。

- [ ] 状态更新 + Domain Event 同事务。
- [ ] 事务成功后再 PubSub 广播。
- [ ] migration 失败时 Endpoint 和 Agent Supervisor 都不启动。

### 1.5 Artifact Store

- [ ] 原子写临时文件后 rename。
- [ ] 记录 relative path、SHA-256、size、MIME、owner。
- [ ] 注册时重新计算哈希，不信任调用方。
- [ ] 支持 JSONL append 和尾部半行恢复。
- [ ] Artifact 不写 SQLite Blob。

### 1.6 HTTP Token

- [ ] 启动生成 256-bit 随机 Token，只打印一次。
- [ ] `/?token=...` 换取 `HttpOnly`、`SameSite=Strict` Cookie 后立即重定向。
- [ ] HTML、JSON、LiveView websocket、SSE 和 `/mcp` 都保护。
- [ ] Token 不写 SQLite、localStorage、URL 日志或应用日志。
- [ ] 重启后旧 Token/Cookie 失效。

### 1.7 Phoenix Shell 与诊断

- [ ] 页面显示 Workspace、Repo mode、Campaign 状态、Best/Base SHA 占位与 preflight。
- [ ] preflight 检查 Git、Python、GPU/Driver、`codex app-server` 和 `cursor-agent acp`。
- [ ] 失败只阻止相关阶段；Workspace/数据库仍可诊断。
- [ ] 暂不实现最终 UI 细节。

### 1.8 恢复骨架

- [ ] 启动顺序：config → lock → migration → SQLite current state → Git identity → Artifact verification → Endpoint/Supervisors。
- [ ] 不可解释差异进入 Blocked。
- [ ] 所有恢复动作带 idempotency key。

## 测试

- [ ] 空 Workspace 初始化并二次恢复。
- [ ] 非空非法目录拒绝。
- [ ] Managed Repo dirty 时拒绝。
- [ ] 两个 OS 进程争抢同一 advisory lock。
- [ ] symlink 被替换、inode 改变、Git common dir 改变时拒绝恢复。
- [ ] SQLite transaction 回滚不广播 PubSub 事件。
- [ ] JSONL 尾部半行只丢弃半行。
- [ ] HTTP Token 覆盖所有入口。
- [ ] kill -9 后自动恢复 Campaign singleton，未重复初始化。

## Exit Gate

- [ ] `pika serve` 可作为前台 Release 启动并恢复。
- [ ] 本地与 Managed Repo 两种模式均通过故障测试。
- [ ] SQLite、Git、Artifact 三源身份一致性可被验证。
- [ ] 第二个 Server 不能管理同一 Managed Repo。
- [ ] 旧 HTTP/MCP Token 在重启后失效。
- [ ] 保存 `artifacts/phase-1/recovery-report.md`。

## 非目标

- 启动真实 Boundary/Iteration Agent。
- Campaign Spec、Baseline 和 Benchmark。
- Attempt、Integration、Sync 或最终 UI。
