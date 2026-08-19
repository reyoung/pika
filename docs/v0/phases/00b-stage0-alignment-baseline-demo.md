# Stage0 Demo — Alignment 到 GPU Baseline Preview

## 状态

Done — 2026-08-18。真实 Codex + AFS H20 E2E 已通过。

## 定位

Stage0 Demo 是 Phase 0 Backend 之上的一次性内存纵切，不是 Phase 1 或 Phase 2。它验证真实用户界面、Boundary MCP、Agent-owned Git、Baseline 数据契约和 GPU/Profiler 接口；Pika 进程重启后不恢复状态，也不派发 Attempt。

## 启动

```bash
./bin/pika stage0-demo <git-repo> \
  [--backend codex|cursor] \
  [--model MODEL] [--effort high] \
  [--host 127.0.0.1] [--port 8080] \
  [--workspace EMPTY_DIR] [--skill-root PATH]
```

- repo 非 Git 根目录或显式 Workspace 非空时，命令在启动 Agent 前失败。
- Pika 固定源 repo 当前提交 HEAD 并重新 clone；working tree 与 untracked 文件不会带入。临时 clone 立即移除 `origin`，源 repo 的原始 status、HEAD、index、refs 和 remote 均不修改。
- 启动 Token 只打印一次，换取 `HttpOnly`、`SameSite=Strict` Cookie；HTML、LiveView 和 MCP 受保护。

## 运行流程

1. 从源 HEAD 创建独立 clone，在 clone 中建立 `pika/best` 和 `pika/setup/1` worktree。
2. Boundary Backend Session 在 setup worktree 中访谈用户、接收上传 Artifact，创建 Reference、正确性测试与 Benchmark Harness。
3. Agent 必须调用 `submit_spec` 和 `submit_harness`；两项通过后 UI 才允许用户确认。
4. 用户确认后，Agent commit setup、squash merge 临时 `pika/best`，调用 `complete_setup_merge`；Pika 核验父 SHA、Diff 与 protected digest。
5. 新 Baseline Session 在 Best SHA 上运行 Full Case Set 的所有正确性和 30 Pair 自配对，生成 Pair JSONL、Profiler 与远程执行/清理 Artifact，并调用 `submit_baseline`。
6. Pika 重算中位数、MAD、noise tolerance 和有效 Pair 数，进入 `SelectingIterationSample`。
7. Baseline Agent 调用 `submit_iteration_sample`，从全量结果中自动选择最多十个初始 Cases、逐项理由和成本摘要。Pika 创建 Sampling Revision v1 后进入 `Optimizing`，但 Preview 明确停止，不创建 Attempt。

用户在活跃 Turn 中发送新消息时使用统一 `steer`。Cursor 的旧 cancelled Turn completion 不能清空或取消新的 follow-up Turn；该时序有专门回归测试。任何阶段缺少必需 MCP 操作时无限 follow-up，不设置次数或时间预算。

## Baseline Pair JSONL v1

每行固定包含：

```json
{
  "schema_version": 1,
  "measured_sha": "40-hex-sha",
  "case_id": "decode_b1_s2048",
  "metric_id": "latency_us",
  "pair_index": 0,
  "order": "ab",
  "a": 37.81,
  "b": 37.96,
  "valid": true,
  "error": null
}
```

每个 Campaign Spec Case/Metric 组合必须恰好出现 `pair_index=0..29`。Pika 只使用 finite、positive 且 `valid=true` 的 Pair；少于 24 个时要求一次整组重跑，第二次仍不足则保留 `BuildingBaseline` 和错误。

## 已完成验证

- 单元与集成：Spec、Harness digest、Artifact path、Git clone/dirty 拒绝、Token、LiveView、上传、MCP 幂等、Baseline 统计与单次重跑。
- Fake Backend E2E：`DraftingSpec → AwaitingConfirmation → BuildingBaseline → SelectingIterationSample → Optimizing`，所有状态推进均由正式 MCP 调用触发。
- 真实 Codex/Cursor Boundary smoke：两个 Backend 均创建 Harness、调用 `submit_spec`/`submit_harness`、到达 `AwaitingConfirmation`，且源 repo 未变化。证据在 `artifacts/stage0-demo/`。
- dirty repo 的 committed-HEAD re-clone 已验证：未提交文件不会进入 Workspace；在无外部并发变化的集成测试中，源 repo 原始 SHA/status 前后完全一致。

## 真实 H20 结果

- [x] 从 `/Users/yuyang/projs/welm_v45_80a3_attention` 的 committed SHA `fc55f8f` 建立独立 clone；16 条当时未提交状态未进入验收，临时 clone 删除 `origin`。
- [x] Codex 完成真实 Alignment、16 项 Reference SHA snapshot、Spec/Harness 确认与 Agent-owned setup squash；临时 Best 为 `2d21882`。
- [x] Agent 通过 global AFS skill 在 H20 上构建四个 verify executors，三个 target trace case 的正确性为 3/3，通过 90/90 自配对样本。
- [x] 使用 `SYS_ADMIN` capability 完成 NCU full 44-pass 与 source 5-pass 报告，解析 2,179 Metrics、177 个 source stall 位置和 PM timeline。
- [x] Workspace 保存 proxy pre/post、可见 workload 输出、精确退出码、三次环境失败 trail、成功 raw bundle、两份 `.ncu-rep` 和远程清理证据；最终 proxy 为 `running=0`。
- [x] `submit_baseline` 由 Pika 从 Pair JSONL 重算通过；该历史 E2E 早于 Sampling 门禁，未包含新的 `submit_iteration_sample`，不可作为 Sampling 流程已通过的证据。
- [x] 正式 Artifact 复制完成后清理临时 Best 内未跟踪 `profile/`，最终 `pika/best` working tree clean。
- [x] E2E 结束后无 Stage0 Backend 子进程。源 repo 在运行期间被其他本地工作从 `fc55f8f` 推进到 `be04be8`；Stage0 clone 无 `origin` 且始终固定测量 `fc55f8f`，该并发变化已在证据中显式记录。

Baseline 结果：

| Case | Median latency | MAD | Noise tolerance | Valid pairs |
|---|---:|---:|---:|---:|
| `trace_5493` | 75.6960 μs | 0.003796 | 1.6884% | 30/30 |
| `trace_1104` | 548.1760 μs | 0.002898 | 1.2888% | 30/30 |
| `trace_179` | 544.4800 μs | 0.001321 | 0.5876% | 30/30 |

Profiler 对 `trace_5493` 报告 71.648 μs、18.64% achieved occupancy、43.64% SM throughput、29.86% peak DRAM-read throughput、168 registers/thread；主要 barrier stall 位于 `verify_attention_welmv45_warp_mma.cuh:1093`。结构化证据位于 `artifacts/stage0-demo/welm-h20-gpu-e2e.json`，大文件仍留在该文件记录的临时 Workspace。

## 非目标

- SQLite、重启恢复、Managed Repo lock、Spec Revision 2+。
- Attempt、Integration Full Regression、Metrics Timeline、Sync 或 Push。
- Pika GPU Worker、AFS 任务状态或远程执行协议。
