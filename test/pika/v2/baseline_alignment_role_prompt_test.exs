defmodule Pika.Agent.BaselineAlignmentRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompt.{Context, FileRef, Schemas, Section}
  alias Pika.Agent.RolePrompts.BaselineAlignment

  @project_root Path.expand("../../..", __DIR__)
  @schema_dir Path.join(@project_root, "priv/v2/roles/schemas")
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/baseline_alignment")
  @alignment_schema_keys ~w(baseline_definition benchmark_record cases metrics target_manifest verify_result)a

  setup_all do
    assert {:ok, schemas} = Schemas.from_dir(@schema_dir)
    %{schemas: schemas}
  end

  test "returns the complete prompt without a context section", %{schemas: schemas} do
    result = BaselineAlignment.system_prompt(%Context{schemas: schemas})

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_BASELINE_ALIGNMENT_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的 Baseline Alignment Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Baseline Alignment System Prompt"
    refute prompt =~ "你不是 Baseline Verify Agent"
    refute prompt =~ "## recover"
    refute prompt =~ "必须读取"

    assert prompt =~ "调用 `ask_questions`"
    assert prompt =~ "submit_baseline_definition(manifest_path, idempotency_key)"
    assert prompt =~ "./verify_cases.sh --case-id 0,1,2,3"
    assert prompt =~ "./benchmark_cases.sh --case-id 0,1,2,3"

    schemas
    |> Map.from_struct()
    |> Map.take(@alignment_schema_keys)
    |> Map.values()
    |> Enum.each(fn path ->
      assert prompt =~ "[#{Path.basename(path)}](<#{path}>)"
    end)
  end

  test "renders every supplied recover section and file", %{schemas: schemas} do
    sections = [recovery_section(0), recovery_section(1)]

    assert {:ok, prompt} =
             BaselineAlignment.system_prompt(%Context{schemas: schemas, sections: sections})

    assert prompt =~ "工作前可以读取以下信息。"
    assert prompt =~ "## recover 0"
    assert prompt =~ "## recover 1"

    for index <- 1..2,
        filename <- ~w(messages.jsonl state.json) do
      path = Path.join([@fixture_dir, "recovery-0#{index}", filename])
      assert prompt =~ "[#{filename}](<#{path}>)"
    end
  end

  test "reports missing Pika-owned schema files" do
    missing_dir = Path.join(System.tmp_dir!(), "pika-v2-missing-schemas")

    assert {:error, {:missing_schema_files, missing}} = Schemas.from_dir(missing_dir)
    assert length(missing) == 10
  end

  test "requires reading previous Baseline Verification failures", %{schemas: schemas} do
    sections = [verification_failure_section(0), verification_failure_section(1)]

    assert {:ok, prompt} =
             BaselineAlignment.system_prompt(%Context{schemas: schemas, sections: sections})

    assert prompt =~
             "开始工作前必须读取以下信息，并避免重复其中已经确认的问题。"

    assert prompt =~ "## baseline verification failure 0"
    assert prompt =~ "## baseline verification failure 1"

    for index <- 1..2 do
      path =
        Path.join([
          @fixture_dir,
          "verification-failures",
          "verification-0#{index}.json"
        ])

      assert prompt =~ "[verification-0#{index}.json](<#{path}>)"
    end
  end

  test "rejects a context section whose file does not exist", %{schemas: schemas} do
    section = %Section{
      title: "recover 0",
      files: [%FileRef{label: "历史消息", path: "/missing/messages.jsonl"}]
    }

    assert {:error, :invalid_context_sections} =
             BaselineAlignment.system_prompt(%Context{schemas: schemas, sections: [section]})
  end

  test "all injected schema files contain valid JSON", %{schemas: schemas} do
    schemas
    |> Map.from_struct()
    |> Map.values()
    |> Enum.each(fn path ->
      assert {:ok, _schema} = path |> File.read!() |> Jason.decode()
    end)
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

  defp verification_failure_section(index) do
    path =
      Path.join([
        @fixture_dir,
        "verification-failures",
        "verification-0#{index + 1}.json"
      ])

    %Section{
      title: "baseline verification failure #{index}",
      files: [%FileRef{label: "Verification Result", path: path}],
      required?: true
    }
  end
end
