defmodule Pika.Agent.IntegrationRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompt.{FileRef, Section}
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Agent.RolePrompts.Integration
  alias Pika.Agent.RolePrompts.Integration.Input
  alias Pika.Integration.RunPaths

  @project_root Path.expand("../../..", __DIR__)
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/integration")

  setup_all do
    assert {:ok, schemas} = RolePromptRegistry.schemas()
    %{schemas: schemas}
  end

  test "renders concrete Full Case IDs without a title", %{schemas: schemas} do
    result = Integration.system_prompt(input(schemas, [0, 1, 4, 9]))

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_INTEGRATION_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的串行 Integration Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Integration System Prompt"
    refute prompt =~ "<all-case-ids>"

    assert prompt =~ "./verify_cases.sh --case-id 0,1,4,9"
    assert prompt =~ "./benchmark_cases.sh --case-id 0,1,4,9"
    assert prompt =~ "每个 `guard` Metric"
    assert prompt =~ "max(max_regression_ratio, Noise Tolerance)"
    assert prompt =~ "prepare_best_update(validation_path, idempotency_key)"
    assert prompt =~ "finish_integration(result_path, idempotency_key)"
    assert prompt =~ "`benchmarks/pika_adapter.py` 不是 Pika 硬保护路径"
    assert prompt =~ "PIKA_CANDIDATE_MANIFEST"
    assert prompt =~ "Integration Run 是 ID `42`、sequence `2`"
    assert prompt =~ "`integration/runs/000002/integration-result.json`"
    assert prompt =~ "最多 3 个 Sampling Feedback Case IDs"
    refute prompt =~ "最多 `regression_feedback_cases` 个"
    assert prompt =~ "[integration-validation.schema.json](<#{schemas.integration_validation}>)"
    assert prompt =~ "[integration-result.schema.json](<#{schemas.integration_result}>)"
  end

  test "renders multiple recovery directories and files", %{schemas: schemas} do
    sections = [recovery_section(0), recovery_section(1)]

    assert {:ok, prompt} =
             Integration.system_prompt(input(schemas, [0, 1], sections))

    assert prompt =~ "## recover 0"
    assert prompt =~ "## recover 1"

    for index <- 1..2 do
      directory = Path.join(@fixture_dir, "recovery-0#{index}")
      assert prompt =~ "[recovery-0#{index}](<#{directory}>)"
      assert prompt =~ "[messages.jsonl](<#{Path.join(directory, "messages.jsonl")}>)"
      assert prompt =~ "[state.json](<#{Path.join(directory, "state.json")}>)"
    end
  end

  test "rejects empty or duplicate Full Case IDs", %{schemas: schemas} do
    assert {:error, :invalid_full_case_ids} =
             Integration.system_prompt(input(schemas, []))

    assert {:error, :invalid_full_case_ids} =
             Integration.system_prompt(input(schemas, [0, 1, 1]))
  end

  defp input(schemas, case_ids, sections \\ []) do
    %Input{
      full_case_ids: case_ids,
      validation_schema: schemas.integration_validation,
      result_schema: schemas.integration_result,
      run_id: 42,
      run_sequence: 2,
      paths: RunPaths.for_run(2),
      regression_feedback_cases: 3,
      sections: sections
    }
  end

  defp recovery_section(index) do
    directory = Path.join(@fixture_dir, "recovery-0#{index + 1}")

    %Section{
      title: "recover #{index}",
      files: [
        %FileRef{label: "恢复目录", path: directory, kind: :directory},
        %FileRef{label: "历史消息", path: Path.join(directory, "messages.jsonl")},
        %FileRef{label: "状态文件", path: Path.join(directory, "state.json")}
      ]
    }
  end
end
