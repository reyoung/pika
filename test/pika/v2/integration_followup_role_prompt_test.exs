defmodule Pika.Agent.IntegrationFollowupRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompt.FollowupInput
  alias Pika.Agent.RolePrompts.IntegrationFollowup

  @project_root Path.expand("../../..", __DIR__)
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/integration_followup")

  test "returns the complete prompt without a title" do
    input = input()
    result = IntegrationFollowup.system_prompt(input)

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_INTEGRATION_FOLLOWUP_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的 Integration Follow-up Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Integration Follow-up System Prompt"

    assert prompt =~ "Follow-up Context：`#{input.context_file}`"
    assert prompt =~ "目标 Work 历史：`#{input.target_messages_file}`"
    assert prompt =~ "目标 Work 当前状态：`#{input.target_state_file}`"
    assert prompt =~ "Git Intent"
    assert prompt =~ "submit_followup_message(message, idempotency_key)"
    assert prompt =~ "生成且只生成一条消息"
  end

  test "reports every missing injected file" do
    input = %FollowupInput{
      context_file: "/missing/context.json",
      target_messages_file: "/missing/messages.jsonl",
      target_state_file: "/missing/state.json"
    }

    assert {:error, {:missing_prompt_files, missing}} = IntegrationFollowup.system_prompt(input)

    assert missing ==
             Enum.sort([
               input.context_file,
               input.target_messages_file,
               input.target_state_file
             ])
  end

  defp input do
    %FollowupInput{
      context_file: Path.join(@fixture_dir, "context.json"),
      target_messages_file: Path.join(@fixture_dir, "messages.jsonl"),
      target_state_file: Path.join(@fixture_dir, "state.json")
    }
  end
end
