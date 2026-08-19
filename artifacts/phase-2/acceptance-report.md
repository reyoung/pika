# Phase 2 验收报告

日期：2026-08-19

结论：Phase 2 的 Alignment、Campaign Spec、Reference/Skill 固定、Setup Merge、Baseline、Sampling 门禁、Profiler Artifact 与重启恢复已实现并通过验收。

## 用户拥有 kick-off

真实 Codex App Server Alignment smoke 通过，结果见
[`codex_app_server-boundary-smoke.json`](./codex_app_server-boundary-smoke.json)：

- `thread/start` 包含 2283 字节系统级 `developerInstructions`。
- Backend 建立 Session 后没有自动 `turn/start`。
- 用户发送测试需求后才产生唯一一次 `turn/start`；其输入完整等于用户消息，没有 Pika Prompt 或 Skill item 伪装成用户内容。
- Codex 调用 Boundary MCP 生成 Spec/Harness，并停在 `awaiting_confirmation`；Pika 未替用户确认。

这验证了 Campaign kick-off 由用户发起。Alignment、setup merge 和 Baseline 模板只作为系统指令注入；用户确认动作可以授权下一阶段，但系统模板不成为用户首条消息。

## 持久化流程

`test/pika/phase2_persistence_test.exs` 使用真实临时 Git Workspace、SQLite migration、Artifact 文件和共享 Campaign 状态机覆盖：

1. 用户 Alignment 消息与附件原子登记。
2. `submit_spec`/`submit_harness` 后停在 `AwaitingConfirmation`。
3. 进程重启恢复 Spec、Harness、Ref SHA 和 Skill SHA。
4. 用户确认后建立 setup worktree，完成 squash merge 到 `pika/best`。
5. `submit_baseline` 写入 Baseline Best Revision/Metrics，并停在 `SelectingIterationSample`。
6. `submit_iteration_sample` 写入最多十项的 Sampling Revision 后进入 `Optimizing`。
7. 再次重启恢复 Best/Baseline/Sampling；不同 Spec Revision 的 Metric series 保持分离。

SQLite 只登记 Artifact 相对路径和 SHA-256；in-flight 对话、Backend cursor 和 Phase 保存在 checkpoint，规范化表保存 Spec、Cases、Metrics、Sampling、Best、Ref/Skill 与 Agent Session。

## Registry、Harness 与 Baseline 门禁

- Reference Catalog 测试确认 Atrex 16 项默认全选、取消选择、项目级解析失败，以及已经固定的完整 SHA 在 materialize 时不刷新。
- Skill Registry 测试确认恢复时只接受固定 SHA，checkout 发生漂移即拒绝启动。
- Harness 测试确认 digest 对增加、删除、重命名和内容修改敏感，候选 commit 修改 protected path 会被拒绝。
- Spec 测试覆盖 target/guard/informational Case、minimize/maximize direction、任意 Metric 名称和语义门禁。
- Baseline 测试覆盖 warmup 10、严格 `pair_index=0..29`、交替次序、至少 24 个有效 Pair、中位数/MAD/noise floor，以及全局仅一次重跑。

## 真实 H20 Profiler 解析

由于本机没有 `ncu_report` Python 模块，使用 AFS H20 镜像对真实 `.ncu-rep` 运行固定 Skill SHA 的上游 parser。证据见
[`profiler-parser-receipt.json`](./profiler-parser-receipt.json)、
[`profiler-parser.log`](./profiler-parser.log) 和
[`profiler-parser-sha256.txt`](./profiler-parser-sha256.txt)：

- Skill：`ncu-report-skill@1cf238d6b41c79bd35041192506c4d45e765a3f1`。
- 输入报告 SHA-256：`11330aa68ee4934b4640c1ebf059c5056923844936b063418ac0b504a420e82b`。
- 上游 `helpers/analyze_reports.py` 成功解析 2179 个 Metrics，退出码 0。
- AFS 运行前后 proxy 均无残留任务，最终 `running=0`。

Profiler manifest 校验同时要求目标 SHA、目标 Case、固定 Skill SHA、工具、精确采集命令、`artifacts/profiles/` 目录、summary、`.ncu-rep`、parser 命令/输出和远程执行证据全部登记；依赖缺失时拒绝 Baseline。

## 验证命令

```text
mix check
MIX_ENV=prod mix release --overwrite
git diff --check
```

结果：`mix check` 为 87 passed、1 excluded（现有 external provider conformance）；生产 release 成功生成于 `_build/prod/rel/pika`；`git diff --check` 通过。
