# Phase 1 — Workspace、启动与持久状态

## 状态

Completed — 2026-08-19

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

- [x] 实现 `pika serve --workspace --repo --config --host --port`。
- [x] 默认 `host=127.0.0.1`，默认端口由配置确定。
- [x] YAML 校验错误逐字段报告，不能静默使用猜测值。
- [x] 解析后写入 Workspace `config.json`，保存 SHA-256。
- [x] 恢复时比较不可变字段：Workspace、Managed Repo、listen address、Alignment/Baseline Backend type/command/protocol config。
- [x] 允许未来修改的字段：Plan、max attempts、history N、Iteration Agent profiles、Reference Catalog、停止条件。

### 1.2 Workspace 初始化

固定布局：

```text
workspace/
├── repo/
├── attempts/
├── prompts/{alignment,attempt,integration,sync}/  # 使用 pika init 时生成
├── artifacts/{plans,patches,profiles,prompts,logs}/
├── pika.sqlite3
├── pika.yaml                 # 使用 pika init 时生成
└── config.json
```

- [x] 空目录或仅含当前 `pika.yaml` 的目录初始化；合法 Pika Workspace 恢复；其他非空目录拒绝。
- [x] Owned Repo 在 `repo/` 创建实际仓库。
- [x] Managed Repo 使用 `repo/` 软链接。
- [x] 所有 Artifact API 只接受规范化相对路径并拒绝 `..` 逃逸。

### 1.3 Managed Repo 所有权

- [x] 首次接管要求 working tree 和 index clean。
- [x] 在 Git common directory 建立锁文件并持续持有 OS advisory lock。
- [x] 锁诊断信息包含 Server UUID、PID、启动时间、Workspace canonical path。
- [x] 恢复时校验 symlink canonical path、device/inode、Git common directory。
- [x] 第二个 Pika Server 对同一 repo 启动时失败且不修改任何文件。

### 1.4 SQLite 基础

采用 [database-schema.md](../../design/database-schema.md) 的 schema，但本 Phase 先落：

- [x] `campaigns`
- [x] `artifacts`
- [x] `operation_intents`
- [x] `domain_events`
- [x] `agent_sessions` 的最小恢复字段
- [x] `idempotency_records`

连接设置：`foreign_keys=ON`、WAL、`synchronous=FULL`、`busy_timeout=5000`。

- [x] 状态更新 + Domain Event 同事务。
- [x] 事务成功后再 PubSub 广播。
- [x] migration 失败时 Endpoint 和 Agent Supervisor 都不启动。

### 1.5 Artifact Store

- [x] 原子写临时文件后 rename。
- [x] 记录 relative path、SHA-256、size、MIME、owner。
- [x] 注册时重新计算哈希，不信任调用方。
- [x] 支持 JSONL append 和尾部半行恢复。
- [x] Artifact 不写 SQLite Blob。

### 1.6 HTTP Token

- [x] 启动生成 256-bit 随机 Token，只打印一次。
- [x] `/?token=...` 换取 `HttpOnly`、`SameSite=Strict` Cookie 后立即重定向。
- [x] HTML、JSON、LiveView websocket、SSE 和 `/mcp` 都保护。
- [x] Token 不写 SQLite、localStorage、URL 日志或应用日志。
- [x] 重启后旧 Token/Cookie 失效。

### 1.7 Phoenix Shell 与诊断

- [x] 页面显示 Workspace、Repo mode、Campaign 状态、Best/Base SHA 占位与 preflight。
- [x] preflight 检查 Git、Python、GPU/Driver、`codex app-server` 和 `cursor-agent acp`。
- [x] 失败只阻止相关阶段；Workspace/数据库仍可诊断。
- [x] 暂不实现最终 UI 细节。

### 1.8 恢复骨架

- [x] 启动顺序：config → lock → migration → SQLite current state → Git identity → Artifact verification → Endpoint/Supervisors。
- [x] 不可解释差异进入 Blocked。
- [x] 所有恢复动作带 idempotency key。

## 测试

- [x] 空 Workspace 初始化并二次恢复。
- [x] 非空非法目录拒绝。
- [x] Managed Repo dirty 时拒绝。
- [x] 两个 OS 进程争抢同一 advisory lock。
- [x] symlink 被替换、inode 改变、Git common dir 改变时拒绝恢复。
- [x] SQLite transaction 回滚不广播 PubSub 事件。
- [x] JSONL 尾部半行只丢弃半行。
- [x] HTTP Token 覆盖所有入口。
- [x] kill -9 后自动恢复 Campaign singleton，未重复初始化。

## Exit Gate

- [x] `pika serve` 可作为前台 Release 启动并恢复。
- [x] 本地与 Managed Repo 两种模式均通过故障测试。
- [x] SQLite、Git、Artifact 三源身份一致性可被验证。
- [x] 第二个 Server 不能管理同一 Managed Repo。
- [x] 旧 HTTP/MCP Token 在重启后失效。
- [x] 保存 `artifacts/phase-1/recovery-report.md`。

## 实施结果

- `./bin/pika serve` 与组装后的 Release `bin/pika serve` 均以前台模式完成真实启动。
- Owned Repo 与 Managed Repo 都通过进程级 `kill -9` 恢复；Campaign ID 保持不变，`campaigns` 单例行未重复创建。
- Managed Repo 使用 Git common directory 中的 `.pika.lock` 持续持有 OS `flock`；第二个 Pika OS 进程接管失败，且未物化第二个 Workspace。
- SQLite migrations、WAL/`FULL`/foreign key/busy timeout、事务 outbox、Artifact 校验、JSONL 半行恢复及幂等恢复 Intent 已落地。
- HTML/LiveView、JSON、SSE 和 `/mcp` 共用启动 Token 鉴权边界；重启后旧 URL Token、Cookie 与 Bearer Token 均失效。
- `mix check`：`76 passed, 1 excluded`；排除项是既有 Phase 0 外部 Provider conformance，不属于 Phase 1 门禁。
- 完整故障矩阵与 Release 运行证据见 [`artifacts/phase-1/recovery-report.md`](../../../artifacts/phase-1/recovery-report.md)。

## 非目标

- 启动真实 Boundary/Iteration Agent。
- Campaign Spec、Baseline 和 Benchmark。
- Attempt、Integration、Sync 或最终 UI。
