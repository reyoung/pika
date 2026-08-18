---
status: accepted
---

# v1 以 Linux 前台 Elixir Release 交付

Pika v1 正式运行在有本地 GPU 的 Linux x86_64 主机，以前台 Elixir Release 交付，不内置 daemon、Docker 或 GPU Worker。外部进程管理器负责常驻与拉起，Pika 在启动后恢复自身状态；Git、ACP Agent Backend、Python 和 GPU 环境由用户预装，Pika 只做诊断。macOS 支持 UI、目标对齐和开发，但不承诺 GPU 优化阶段。

## Consequences

- 部署不依赖 Kubernetes、Temporal、消息队列或容器编排。
- Workspace 和监听等身份配置启动后不可变，可调 Campaign 参数通过 UI 持久化。
- 启动诊断必须在创建 Agent 或修改 Git 前完成。
