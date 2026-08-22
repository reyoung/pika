defmodule Pika.ReconfigurationTest do
  use ExUnit.Case, async: false

  import ExUnit.CaptureIO

  alias Pika.Test.CampaignFixtures
  alias Pika.{Config, Init, Reconfiguration}

  test "enables and configures the Progress Summary Agent without rewriting unrelated YAML" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")
    original = File.read!(config_path)
    File.write!(config_path, "# user-owned comment\n" <> original)

    output =
      capture_io(fn ->
        assert {:ok, result} =
                 Reconfiguration.run(
                   workspace: workspace,
                   config: config_path,
                   progress_summary: true,
                   summary_backend: "cursor",
                   summary_model: "summary-model",
                   summary_effort: "high",
                   summary_interval_minutes: 7,
                   yes: true
                 )

        assert result.progress_summary.enabled
      end)

    assert output =~ "New Agent Sessions"
    assert File.read!(config_path) =~ "# user-owned comment"
    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    assert config.campaign["progress_summary"]["enabled"]
    assert config.campaign["progress_summary"]["backend"] == "cursor_acp"
    assert config.campaign["progress_summary"]["model"] == "summary-model"
    assert config.campaign["progress_summary"]["reasoning_effort"] == "high"
    assert config.campaign["progress_summary"]["interval_minutes"] == 7
  end

  test "console flow prompts for a configurable section and can disable summaries" do
    workspace = initialized_workspace(progress_summary: true)
    config_path = Path.join(workspace, "pika.yaml")

    output =
      capture_io("6\nn\n", fn ->
        assert {:ok, result} =
                 Reconfiguration.run(workspace: workspace, config: config_path)

        refute result.progress_summary.enabled
      end)

    assert output =~ "Configurable sections"
    assert output =~ "Progress Summary Agent"
    assert output =~ "Server listen address"
    assert output =~ "Alignment/Baseline Agent"
    assert output =~ "Iteration Agents"
    assert output =~ "Integration Agent"
    assert output =~ "Campaign limits"
    assert output =~ "Sync"
    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    refute config.campaign["progress_summary"]["enabled"]
  end

  test "configures the Integration FollowUp Agent independently" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")

    assert {:ok, result} =
             Reconfiguration.run(
               workspace: workspace,
               config: config_path,
               followup_backend: "cursor",
               followup_model: "followup-model",
               followup_effort: "low",
               yes: true
             )

    assert result.integration_followup_agent.model == "followup-model"
    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    profile = config.campaign["integration_followup_agent"]
    assert profile["backend"] == "cursor_acp"
    assert profile["model"] == "followup-model"
    assert profile["reasoning_effort"] == "low"
    assert profile["sandbox_policy"] == "disabled"
  end

  test "interactive FollowUp configuration lists backends and provider model names" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")

    output =
      capture_io("5\n2\n2\nlow\nauto_review\nenabled\n", fn ->
        assert {:ok, _result} =
                 Reconfiguration.run(
                   workspace: workspace,
                   config: config_path,
                   model_catalog: fn
                     "cursor_acp" ->
                       {:ok,
                        [%{id: "cursor-fast", label: "Cursor Fast", description: "Fast model"}]}
                   end
                 )
      end)

    assert output =~ "1) Codex · Codex App Server"
    assert output =~ "2) Cursor · Agent Client Protocol"
    assert output =~ "Available Integration FollowUp Agent models (Cursor)"
    assert output =~ "cursor-fast · Cursor Fast"
    assert output =~ "FollowUp reasoning effort:"
    assert output =~ "1) low · Fastest responses with minimal reasoning"
    assert output =~ "6) ultra · Deepest supported reasoning"

    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    assert config.campaign["integration_followup_agent"]["model"] == "cursor-fast"
  end

  test "interactive Summary configuration lists provider model names" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")

    output =
      capture_io("6\ny\n1\n2\nmedium\n10\n", fn ->
        assert {:ok, _result} =
                 Reconfiguration.run(
                   workspace: workspace,
                   config: config_path,
                   model_catalog: fn
                     "codex_app_server" ->
                       {:ok, [%{id: "codex-fast", label: "Codex Fast", description: nil}]}
                   end
                 )
      end)

    assert output =~ "1) Codex · Codex App Server"
    assert output =~ "Available Progress Summary Agent models (Codex)"
    assert output =~ "codex-fast · Codex Fast"
    assert output =~ "Summary reasoning effort:"
    assert output =~ "3) high · Thorough reasoning (recommended)"
  end

  test "interactive reasoning effort accepts a listed number" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")

    output =
      capture_io("4\n1\n1\n4\n1\n1\n", fn ->
        assert {:ok, _result} =
                 Reconfiguration.run(
                   workspace: workspace,
                   config: config_path,
                   model_catalog: fn "codex_app_server" -> {:ok, []} end
                 )
      end)

    assert output =~ "Select Integration Agent reasoning effort [3]:"
    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    assert config.campaign["integration_agent"]["reasoning_effort"] == "xhigh"
  end

  test "reconfigures every init configuration domain" do
    workspace = initialized_workspace()
    config_path = Path.join(workspace, "pika.yaml")

    changes = [
      [host: "0.0.0.0", port: 19_001],
      [
        alignment_backend: "cursor",
        alignment_model: "align-v2",
        alignment_effort: "low",
        alignment_approval_policy: "auto_review",
        alignment_sandbox_policy: "enabled"
      ],
      [
        iteration_backend: "cursor",
        iteration_model: "iter-v2",
        iteration_effort: "medium",
        iteration_approval_policy: "auto_review",
        iteration_sandbox_policy: "enabled",
        iteration_agents: 3
      ],
      [
        integration_backend: "cursor",
        integration_model: "integrate-v2",
        integration_effort: "high",
        integration_approval_policy: "auto_review",
        integration_sandbox_policy: "enabled"
      ],
      [max_attempts: 11, max_unverified_attempts: 4],
      [sync_remote: "upstream", sync_branch: "release"]
    ]

    Enum.each(changes, fn change ->
      assert {:ok, _} =
               Reconfiguration.run(
                 [workspace: workspace, config: config_path, yes: true] ++ change
               )
    end)

    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    assert {config.host, config.port} == {"0.0.0.0", 19_001}
    assert config.backend["type"] == "cursor_acp"
    assert config.backend["model"] == "align-v2"
    assert length(config.campaign["iteration_agents"]) == 3
    assert hd(config.campaign["iteration_agents"])["model"] == "iter-v2"
    assert config.campaign["integration_agent"]["model"] == "integrate-v2"
    assert config.campaign["max_attempts"] == 11
    assert config.campaign["max_unverified_attempts"] == 4
    assert config.sync == %{"remote" => "upstream", "branch" => "release"}
  end

  defp initialized_workspace(overrides \\ []) do
    workspace = CampaignFixtures.workspace()
    File.rmdir!(workspace)

    opts =
      [
        workspace: workspace,
        owned: true,
        host: "127.0.0.1",
        port: 18_080,
        yes: true
      ]
      |> Keyword.merge(overrides)

    capture_io(fn -> assert {:ok, _result} = Init.run(opts) end)
    workspace
  end
end
