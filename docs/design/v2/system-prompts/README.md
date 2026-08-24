# Pika v2 Role Prompt 审阅索引

v2 Role Prompt 的运行时权威是 `priv/v2/roles` 下的 Elixir interface 与实现，不是 Markdown template。全部八个 Role Prompt 已迁移完成。

1. [Baseline Alignment Elixir 实现](../../../../priv/v2/roles/baseline_alignment.ex)
   - [Prompt interface](../../../../priv/v2/roles/role_prompt.ex)
   - [Pika-owned JSON Schemas](../../../../priv/v2/roles/schemas/)
2. [Baseline Verify Elixir 实现](../../../../priv/v2/roles/baseline_verify.ex)
3. [Baseline Verify Follow-up Elixir 实现](../../../../priv/v2/roles/baseline_verify_followup.ex)
4. [Iteration Elixir 实现](../../../../priv/v2/roles/iteration.ex)
5. [Iteration Follow-up Elixir 实现](../../../../priv/v2/roles/iteration_followup.ex)
6. [Integration Elixir 实现](../../../../priv/v2/roles/integration.ex)
7. [Integration Follow-up Elixir 实现](../../../../priv/v2/roles/integration_followup.ex)
8. [Progress Summary Elixir 实现](../../../../priv/v2/roles/progress_summary.ex)

Follow-up 和 Progress Summary Role 未配置时不创建对应 Agent。
