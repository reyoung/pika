---
status: accepted
---

# v1 采用单租户实例和启动时访问 Token

Pika v1 是单租户、自托管的常驻 HTTP 服务。服务每次启动时生成一个随机访问 Token，所有网站访问者必须持有该 Token；这用很小的实现和运维成本保护可能包含源码、Agent 输出和性能数据的界面，同时明确暂不承担多租户身份、权限、配额与凭证隔离的复杂度。

## Consequences

- v1 不提供用户、组织或角色模型。
- 服务重启后生成新的 Token，旧 Token 随进程生命周期结束而失效。
- 所有可访问的 HTTP 界面与后端通信入口都必须位于同一鉴权边界内，不能只保护 HTML 首页。
- Token 使用 256-bit 随机值且不进入 SQLite；API、SSE 和 WebSocket 接受 Bearer Token。
- 浏览器首次用 URL Token 换取 `HttpOnly`、`SameSite=Strict` Cookie 后必须立即清除地址栏中的 Token，应用不得把 Token 写入 localStorage 或访问日志。
