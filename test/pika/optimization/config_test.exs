defmodule Pika.Optimization.ConfigTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.Config
  alias Pika.Optimization.Config.Agent
  alias Pika.Optimization.RoleRegistry

  test "loads expanded per-Role Backend configurations and YAML anchors" do
    path = config_file(complete_yaml())

    assert {:ok, config} = Config.load(path)
    assert config.repo == Path.expand("repo", Path.dirname(path))
    assert config.workspace == Path.expand("workspace", Path.dirname(path))

    assert config.baseline_alignment.agent.backend == :codex_app_server
    assert config.baseline_verify.max_followups == 8
    assert config.baseline_verify_followup.generator_max_attempts == 3

    assert length(Config.iteration_agents(config)) == 2

    assert Enum.map(Config.iteration_agents(config), & &1.backend) == [
             :codex_app_server,
             :cursor_acp
           ]

    assert config.iteration.history_limit == 20
    assert config.iteration.max_followups == 5
    assert config.iteration.max_pending_attempts == 0
    assert config.integration.regression_feedback_cases == 3
    assert config.progress_summary.interval_ms == 300_000

    assert RoleRegistry.enabled(config) |> Enum.map(& &1.id) |> Enum.sort() ==
             RoleRegistry.all() |> Map.keys() |> Enum.sort()

    assert {:ok, iteration_role} = RoleRegistry.fetch(:iteration)
    assert RoleRegistry.concurrency(iteration_role, config) == 2

    assert {:ok, integration} = Config.role_agent(config, :integration)
    assert %Agent{} = integration

    profile = Agent.backend_profile(integration)
    assert profile["backend"] == :codex_app_server
    assert profile["sandbox_policy"] == "workspace-write"
    refute Map.has_key?(profile, "name")
    refute Map.has_key?(profile, "profile")
  end

  test "allows optional Follow-up and Progress Summary Roles to be absent" do
    path = config_file(minimal_yaml())

    assert {:ok, config} = Config.load(path)
    assert is_nil(config.baseline_verify_followup)
    assert is_nil(config.iteration_followup)
    assert is_nil(config.integration_followup)
    assert is_nil(config.progress_summary)

    assert RoleRegistry.enabled(config) |> Enum.map(& &1.id) |> Enum.sort() ==
             ~w(baseline_alignment baseline_verify integration iteration)

    assert {:error, {:agent_role_disabled, "progress_summary"}} =
             Config.role_agent(config, :progress_summary)
  end

  test "rejects Agent Profile fields and empty Iteration concurrency" do
    profile_yaml =
      String.replace(
        minimal_yaml(),
        "  baseline_alignment:\n    backend: codex",
        "  baseline_alignment:\n    backend: codex\n    profile: shared"
      )

    assert {:error, {:invalid_v2_config, [message]}} = Config.load(config_file(profile_yaml))
    assert message =~ "Agent Profile fields are not allowed"

    empty_agents =
      String.replace(
        minimal_yaml(),
        "  iteration:\n    agents:\n      - backend: codex\n        approval_policy: never\n        sandbox: workspace-write",
        "  iteration:\n    agents: []"
      )

    assert {:error, {:invalid_v2_config, [message]}} = Config.load(config_file(empty_agents))
    assert message =~ "agents.iteration.agents: must be a non-empty list"
  end

  test "requires version 2 and all mandatory Roles" do
    assert {:error, {:invalid_v2_config, ["version: must be 2"]}} =
             minimal_yaml()
             |> String.replace("version: 2", "version: 1")
             |> config_file()
             |> Config.load()

    missing = String.replace(minimal_yaml(), ~r/\n  integration:\n(?:    .*\n)+/, "")

    assert {:error, {:invalid_v2_config, ["agents.integration: is required"]}} =
             missing |> config_file() |> Config.load()
  end

  defp config_file(contents) do
    directory =
      Path.join(System.tmp_dir!(), "pika-v2-config-#{System.unique_integer([:positive])}")

    File.mkdir_p!(directory)
    path = Path.join(directory, "pika.yaml")
    File.write!(path, contents)
    on_exit(fn -> File.rm_rf!(directory) end)
    path
  end

  defp complete_yaml do
    """
    version: 2
    repo: repo
    workspace: workspace

    agents:
      baseline_alignment: &writer
        backend: codex
        model: gpt-test
        reasoning_effort: high
        approval_policy: never
        sandbox: workspace-write
        env:
          PIKA_TEST: enabled

      baseline_verify:
        <<: *writer
        max_followups: 8

      baseline_verify_followup: &reader
        backend: codex
        model: gpt-test
        reasoning_effort: medium
        approval_policy: never
        sandbox: read-only
        generator_max_attempts: 3

      iteration:
        history_limit: 20
        max_followups: 5
        max_pending_attempts: 0
        agents:
          - <<: *writer
          - backend: cursor
            model: auto
            approval_policy: never
            sandbox: workspace-write

      iteration_followup:
        <<: *reader

      integration:
        <<: *writer
        max_followups: 8
        regression_feedback_cases: 3

      integration_followup:
        <<: *reader

      progress_summary:
        interval: 5m
        backend: codex
        model: gpt-test
        reasoning_effort: low
        approval_policy: never
        sandbox: read-only
        max_followups: 3
    """
  end

  defp minimal_yaml do
    """
    version: 2
    repo: repo
    workspace: workspace

    agents:
      baseline_alignment:
        backend: codex
        approval_policy: never
        sandbox: workspace-write

      baseline_verify:
        backend: codex
        approval_policy: never
        sandbox: workspace-write

      iteration:
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write

      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
