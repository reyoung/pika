defmodule Pika.Agent.ProgressSummaryRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompts.ProgressSummary
  alias Pika.Agent.RolePrompts.ProgressSummary.Input

  @project_root Path.expand("../../..", __DIR__)
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/progress_summary")

  test "renders a summary.md-only prompt without a title" do
    input = input()
    result = ProgressSummary.system_prompt(input)

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_PROGRESS_SUMMARY_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的 Progress Summary Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Progress Summary System Prompt"
    refute prompt =~ "progress-summary.json"
    refute prompt =~ "result_path"

    assert prompt =~ "尽量控制在 800 字以内"
    assert prompt =~ "摘要必须包含“关键优化”小节"
    assert prompt =~ "status.json.best_history"
    assert prompt =~ "为什么可能更快"
    assert prompt =~ "尚无已接受的代码优化"
    assert prompt =~ "在当前目录写 `summary.md`"
    assert prompt =~ "submit_progress_summary(summary_path, idempotency_key)"
    assert prompt =~ "本次独立工作目录：`#{input.summary_workdir}`"
    refute prompt =~ "上一份 Summary："
  end

  test "includes an optional previous Summary" do
    previous = Path.join(@fixture_dir, "previous-summary.md")
    input = input(previous_summary_file: previous)

    assert {:ok, prompt} = ProgressSummary.system_prompt(input)
    assert prompt =~ "上一份 Summary：`#{previous}`"
  end

  test "reports missing input files and output directory" do
    missing_file = "/missing/messages.jsonl"

    assert {:error, {:missing_prompt_files, [^missing_file]}} =
             ProgressSummary.system_prompt(input(messages_file: missing_file))

    missing_dir = "/missing/summary-workdir"

    assert {:error, {:missing_summary_workdir, ^missing_dir}} =
             ProgressSummary.system_prompt(input(summary_workdir: missing_dir))
  end

  defp input(overrides \\ []) do
    defaults = [
      context_file: Path.join(@fixture_dir, "context.json"),
      status_file: Path.join(@fixture_dir, "status.json"),
      messages_file: Path.join(@fixture_dir, "messages.jsonl"),
      summary_workdir: Path.join(@fixture_dir, "output")
    ]

    struct!(Input, Keyword.merge(defaults, overrides))
  end
end
