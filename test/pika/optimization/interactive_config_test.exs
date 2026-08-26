defmodule Pika.Optimization.InteractiveConfigTest do
  use ExUnit.Case, async: true

  import ExUnit.CaptureIO

  alias Pika.Optimization.InteractiveConfig

  test "configures every required Agent independently from complete provider model and effort lists" do
    root =
      Path.join(System.tmp_dir!(), "pika-interactive-init-#{System.unique_integer([:positive])}")

    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")

    catalog = fn backend ->
      prefix = if backend == "cursor_acp", do: "cursor", else: "codex"

      {:ok,
       Enum.map(1..16, fn index ->
         %{id: "#{prefix}-#{index}", label: "#{prefix} #{index}", description: nil}
       end)}
    end

    input =
      Enum.join(
        [
          # Accept a fixed access token instead of the random default
          "fixed-interactive-token",
          # Baseline Alignment: codex / codex-1 / low
          "1",
          "2",
          "1",
          # Baseline Verify: cursor / cursor-2 / medium
          "2",
          "3",
          "2",
          # Baseline Verify Follow-up disabled
          "n",
          # Iteration: codex / the formerly truncated codex-16 / high
          "1",
          "17",
          "3",
          # Iteration Follow-up disabled
          "n",
          # Integration: cursor / cursor-1 / ultra
          "2",
          "2",
          "6",
          # Integration Follow-up and Progress Summary disabled
          "n",
          "n"
        ],
        "\n"
      ) <> "\n"

    parent = self()

    output =
      capture_io(input, fn ->
        assert {:ok, settings} =
                 InteractiveConfig.collect_init(
                   workspace: workspace,
                   repo: repo,
                   iteration_agents: 1,
                   model_catalog: catalog
                 )

        send(parent, {:settings, settings})
      end)

    assert_receive {:settings, settings}
    assert settings.token == "fixed-interactive-token"
    assert settings.agents.baseline_alignment.backend == :codex_app_server
    assert settings.agents.baseline_alignment.model == "codex-1"
    assert settings.agents.baseline_alignment.reasoning_effort == "low"
    assert settings.agents.baseline_alignment.sandbox == "danger_full_access"

    assert settings.agents.baseline_verify.backend == :cursor_acp
    assert settings.agents.baseline_verify.model == "cursor-2"
    assert settings.agents.baseline_verify.reasoning_effort == "medium"
    assert settings.agents.baseline_verify.sandbox == "disabled"

    assert [iteration] = settings.agents.iteration
    assert iteration.backend == :codex_app_server
    assert iteration.model == "codex-16"
    assert iteration.reasoning_effort == "high"
    assert iteration.sandbox == "danger_full_access"

    assert settings.agents.integration.backend == :cursor_acp
    assert settings.agents.integration.model == "cursor-1"
    assert settings.agents.integration.reasoning_effort == "ultra"
    assert settings.agents.integration.sandbox == "disabled"

    assert output =~ "codex-16"
    assert output =~ "cursor-16"
    assert output =~ "6) ultra"
  end

  test "non-interactive init applies explicit defaults and optional Roles" do
    assert {:ok, settings} =
             InteractiveConfig.collect_init(
               workspace: "/tmp/pika-workspace",
               repo: "/tmp/pika-repo",
               backend: "cursor",
               model: "cursor-model",
               reasoning_effort: "xhigh",
               iteration_agents: 2,
               baseline_verify_followup: true,
               iteration_followup: true,
               integration_followup: true,
               progress_summary: true,
               yes: true
             )

    configured = [
      settings.agents.baseline_alignment,
      settings.agents.baseline_verify,
      settings.agents.baseline_verify_followup,
      settings.agents.iteration_followup,
      settings.agents.integration,
      settings.agents.integration_followup,
      settings.agents.progress_summary
      | settings.agents.iteration
    ]

    assert Enum.all?(configured, &(&1.backend == :cursor_acp))
    assert Enum.all?(configured, &(&1.model == "cursor-model"))
    assert Enum.all?(configured, &(&1.sandbox == "disabled"))
    assert length(settings.agents.iteration) == 2
    assert is_binary(settings.token)
    assert byte_size(settings.token) >= 43
  end

  test "non-interactive init selects Cursor Headless independently from Cursor ACP" do
    assert {:ok, settings} =
             InteractiveConfig.collect_init(
               workspace: "/tmp/pika-headless-workspace",
               repo: "/tmp/pika-headless-repo",
               backend: "cursor-headless",
               model: "gpt-headless",
               reasoning_effort: "max",
               iteration_agents: 1,
               yes: true
             )

    assert settings.agents.baseline_alignment.backend == :cursor_headless
    assert settings.agents.baseline_alignment.model == "gpt-headless"
    assert settings.agents.baseline_alignment.reasoning_effort == "max"
    assert settings.agents.baseline_alignment.sandbox == "disabled"
  end

  test "interactive init accepts the generated random token by default" do
    parent = self()

    output =
      capture_io("\n", fn ->
        assert {:ok, settings} =
                 InteractiveConfig.collect_init(
                   workspace: "/tmp/pika-workspace",
                   repo: "/tmp/pika-repo",
                   backend: "codex",
                   model: "codex-model",
                   reasoning_effort: "high",
                   iteration_agents: 1,
                   baseline_verify_followup: false,
                   iteration_followup: false,
                   integration_followup: false,
                   progress_summary: false
                 )

        send(parent, {:random_token, settings.token})
      end)

    assert_receive {:random_token, token}
    assert byte_size(token) >= 43
    assert output =~ "Access token"
    assert output =~ token
  end
end
