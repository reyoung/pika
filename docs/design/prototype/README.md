# Pika UI Prototype

已确认的 Pika v1 交互原型，包含：

- Alignment Conversation 与 Campaign Spec 确认
- 并发 Attempt 列表与标准化 Backend Session 工作对话
- 从指定 Attempt fork 的 BTW Conversation
- Metrics Timeline、Metric 切换、点选详情和 hover Summary

该目录只用于设计评审。最终产品使用 Phoenix/LiveView，不复用此 React/Vinext 运行时。

## 本地运行

要求 Node.js `>=22.13.0`：

```bash
npm install
npm run dev
```

访问 `http://localhost:3000/`。

## 验证

```bash
npm test
```

测试会完成生产构建并检查服务端渲染结果。
