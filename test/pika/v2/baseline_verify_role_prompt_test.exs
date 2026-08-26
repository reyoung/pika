defmodule Pika.Agent.BaselineVerifyRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompt.{Context, FileRef, Schemas, Section}
  alias Pika.Agent.RolePrompts.BaselineVerify

  @project_root Path.expand("../../..", __DIR__)
  @schema_dir Path.join(@project_root, "priv/v2/roles/schemas")
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/baseline_verify")

  setup_all do
    assert {:ok, schemas} = Schemas.from_dir(@schema_dir)
    %{schemas: schemas}
  end

  test "returns a schema-linked prompt without a title", %{schemas: schemas} do
    result = BaselineVerify.system_prompt(%Context{schemas: schemas})

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_BASELINE_VERIFY_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的 Baseline Verify Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Baseline Verify System Prompt"
    refute prompt =~ "自然语言、命令退出或文件写完都不构成完成"

    assert prompt =~ "./verify_cases.sh --list-cases"
    assert prompt =~ "./benchmark_cases.sh --list-cases"
    assert prompt =~ "PIKA_CANDIDATE_MANIFEST"
    assert prompt =~ "Candidate Artifact 注入审计"
    assert prompt =~ "不要求与旧 Best 完全相等"
    assert prompt =~ "Pika 负责推进新的 Best Revision"
    assert prompt =~ "Benchmark 单进程门禁"
    assert prompt =~ "同一个长期运行的 Python 进程或同一次 `torchrun`"
    assert prompt =~ "禁止脚本按 Case 循环"
    assert prompt =~ "只初始化一次 CUDA / Distributed / NCCL"
    assert prompt =~ "不得接受通过逐 Case 独立进程产生的测量"
    assert prompt =~ "Revision Work Root"
    assert prompt =~ "repo/target"
    assert prompt =~ "repo/development"
    assert prompt =~ "artifact_identity"
    assert prompt =~ "20–30 分钟属于可接受的预期运行时"
    assert prompt =~ "不得仅因运行了数分钟"
    assert prompt =~ "可持续的后台作业并轮询状态"
    assert prompt =~ "不能把 `Case × pair × side × iteration` 计数误当成独立"
    assert prompt =~ "每一个 `case_id × metric_id`"
    assert prompt =~ "outcome=accepted"
    assert prompt =~ "outcome=definition_rejected"
    assert prompt =~ "无论验证通过或失败，都必须调用"
    assert prompt =~ "finish_baseline_verification(result_path, idempotency_key)"

    path = schemas.baseline_verification_result
    assert prompt =~ "[#{Path.basename(path)}](<#{path}>)"
  end

  test "renders multiple recover sections", %{schemas: schemas} do
    sections = [recovery_section(0), recovery_section(1)]

    assert {:ok, prompt} =
             BaselineVerify.system_prompt(%Context{schemas: schemas, sections: sections})

    assert prompt =~ "工作前可以读取以下信息。"
    assert prompt =~ "## recover 0"
    assert prompt =~ "## recover 1"

    for index <- 1..2,
        filename <- ~w(messages.jsonl state.json) do
      path = Path.join([@fixture_dir, "recovery-0#{index}", filename])
      assert prompt =~ "[#{filename}](<#{path}>)"
    end
  end

  test "result schema declares accepted and rejected outcomes with accepted metrics", %{
    schemas: schemas
  } do
    schema = schemas.baseline_verification_result |> File.read!() |> Jason.decode!()

    assert get_in(schema, ["properties", "outcome", "enum"]) ==
             ~w(accepted definition_rejected)

    [accepted, rejected] = schema["allOf"]
    assert get_in(accepted, ["if", "properties", "outcome", "const"]) == "accepted"

    assert get_in(accepted, ["then", "properties", "details", "$ref"]) ==
             "#/$defs/accepted_details"

    assert "case_metrics" in get_in(schema, [
             "$defs",
             "accepted_details",
             "allOf",
             Access.at(1),
             "required"
           ])

    assert get_in(rejected, ["if", "properties", "outcome", "const"]) ==
             "definition_rejected"
  end

  test "reports a missing result schema", %{schemas: schemas} do
    missing = "/missing/baseline-verification-result.schema.json"
    schemas = %{schemas | baseline_verification_result: missing}

    assert {:error, {:missing_schema_files, [^missing]}} =
             BaselineVerify.system_prompt(%Context{schemas: schemas})
  end

  defp recovery_section(index) do
    directory = Path.join(@fixture_dir, "recovery-0#{index + 1}")

    %Section{
      title: "recover #{index}",
      files: [
        %FileRef{label: "历史消息", path: Path.join(directory, "messages.jsonl")},
        %FileRef{label: "状态文件", path: Path.join(directory, "state.json")}
      ]
    }
  end
end
