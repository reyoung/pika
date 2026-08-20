---
status: superseded
---

# Backend 权限默认自动批准并以 YOLO 运行

本 ADR 中“固定为 YOLO”的决定已由 ADR-0032 替代；Pika 仍保留 YOLO 作为兼容默认值。

Pika v1 把单租户机器上的 Agent 视为受信任进程，默认自动批准 Agent Backend 权限请求并使用 YOLO 权限运行，不尝试成为主机安全沙箱。Codex adapter 将其映射到 App Server approval/sandbox 设置，Cursor adapter 将其映射到 ACP permission response。Kernel 编译、GPU 执行、Git 操作和 global skill 可以直接使用 Agent 环境；Pika 只在结果阶段强制检查受保护文件、临时参考仓库和接受门禁。凭证必须从启动环境或 Agent Profile 引用的环境变量继承，不能进入持久化状态、Prompt 或日志。

## Consequences

- 持有 HTTP Token 的用户仍不能把 Pika 当作隔离不可信 Kernel 的执行平台。
- 恶意或失控 Agent 可能访问、修改或删除当前用户有权限操作的主机资源。
- 更严格的 Profile 可以作为未来或显式配置，但不是 v1 默认行为。
