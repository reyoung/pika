defmodule Pika.InitTest do
  use ExUnit.Case, async: false

  import ExUnit.CaptureIO

  alias Pika.Test.{AlignmentFixtures, CampaignFixtures}
  alias Pika.{Config, Git, Init}

  @prompt_templates %{
    "alignment" => {"alignment/alignment.md.eex", "prompts/alignment/alignment.md.eex"},
    "setup_merge" => {"alignment/setup_merge.md.eex", "prompts/alignment/setup_merge.md.eex"},
    "baseline" => {"alignment/baseline.md.eex", "prompts/alignment/baseline.md.eex"},
    "plan" => {"attempt/plan.md.eex", "prompts/attempt/plan.md.eex"},
    "iteration" => {"attempt/iteration.md.eex", "prompts/attempt/iteration.md.eex"},
    "integration" => {"integration/integration.md.eex", "prompts/integration/integration.md.eex"},
    "sync" => {"sync/sync.md.eex", "prompts/sync/sync.md.eex"}
  }

  test "initializes a managed Workspace and writes the selected configuration" do
    repo = AlignmentFixtures.git_repo()
    workspace = CampaignFixtures.workspace()

    output =
      capture_io(fn ->
        assert {:ok, result} =
                 Init.run(
                   workspace: workspace,
                   repo: repo,
                   host: "127.0.0.1",
                   port: 18_081,
                   alignment_backend: "cursor",
                   alignment_model: "alignment-test-model",
                   alignment_effort: "max",
                   alignment_approval_policy: "auto_review",
                   alignment_sandbox_policy: "enabled",
                   iteration_backend: "codex",
                   iteration_model: "iteration-test-model",
                   iteration_effort: "xhigh",
                   iteration_approval_policy: "untrusted",
                   iteration_sandbox_policy: "workspace_write",
                   integration_backend: "codex",
                   integration_model: "integration-test-model",
                   integration_effort: "max",
                   integration_approval_policy: "on_request",
                   integration_sandbox_policy: "workspace_write",
                   iteration_agents: 2,
                   max_attempts: 7,
                   max_unverified_attempts: 3,
                   sync_remote: "origin",
                   sync_branch: "main",
                   yes: true
                 )

        assert result.mode == :managed_repo
        assert result.repo == Pika.Paths.canonical!(repo)
        assert result.config_path == Path.join(Pika.Paths.canonical!(workspace), "pika.yaml")
      end)

    assert output =~ "Initialized Pika Workspace"
    assert output =~ "pika serve"
    assert File.regular?(Path.join(workspace, "pika.yaml"))
    assert File.regular?(Path.join(workspace, "config.json"))
    assert File.regular?(Path.join(workspace, "pika.sqlite3"))
    assert File.lstat!(Path.join(workspace, "repo")).type == :symlink
    assert Git.run!(repo, ["show-ref", "--verify", "refs/heads/pika/best"]) != ""

    assert {:ok, config} =
             Config.load(Path.join(workspace, "pika.yaml"), workspace: workspace, repo: repo)

    assert config.backend["type"] == "cursor_acp"
    assert config.backend["model"] == "alignment-test-model"
    assert config.backend["reasoning_effort"] == "max"
    assert config.backend["approval_policy"] == "auto_review"
    assert config.backend["sandbox_policy"] == "enabled"
    assert config.campaign["max_attempts"] == 7
    assert config.campaign["max_unverified_attempts"] == 3

    assert Enum.map(config.campaign["iteration_agents"], & &1["name"]) ==
             ~w(codex-1 codex-2)

    assert Enum.all?(config.campaign["iteration_agents"], fn profile ->
             profile["backend"] == "codex_app_server" and
               profile["model"] == "iteration-test-model" and
               profile["reasoning_effort"] == "xhigh" and
               profile["approval_policy"] == "untrusted" and
               profile["sandbox_policy"] == "workspace_write"
           end)

    assert config.campaign["integration_agent"] == %{
             "name" => "integration",
             "backend" => "codex_app_server",
             "command" => ["codex", "app-server", "--listen", "stdio://"],
             "model" => "integration-test-model",
             "reasoning_effort" => "max",
             "approval_policy" => "on_request",
             "sandbox_policy" => "workspace_write",
             "env" => %{},
             "protocol_config" => %{}
           }

    assert config.sync == %{"remote" => "origin", "branch" => "main"}

    for {kind, {source, destination}} <- @prompt_templates do
      installed = Path.join(workspace, destination)
      bundled = Application.app_dir(:pika, Path.join("priv/prompts", source))
      assert config.prompts[kind] == Pika.Paths.canonical!(installed)
      assert File.read!(installed) == File.read!(bundled)
    end
  end

  test "asks for missing settings and initializes an owned Workspace" do
    workspace = CampaignFixtures.workspace()
    File.rmdir!(workspace)

    input =
      [
        "owned",
        workspace,
        "",
        "",
        "18082",
        "2",
        "2",
        "2",
        "2",
        "4",
        "1",
        "2",
        "2",
        "2",
        "3",
        "1",
        "2",
        "2",
        "2",
        "3",
        "2",
        "",
        "0",
        "n"
      ]
      |> Enum.join("\n")
      |> Kernel.<>("\n")

    output =
      capture_io(input, fn ->
        assert {:ok, result} =
                 Init.run(
                   model_catalog: fn
                     "cursor_acp" ->
                       {:ok,
                        [
                          %{
                            id: "cursor-test-model",
                            label: "Cursor Test",
                            description: "Deterministic Cursor fixture model"
                          }
                        ]}

                     "codex_app_server" ->
                       {:ok,
                        [
                          %{
                            id: "codex-test-model",
                            label: "Codex Test",
                            description: "Deterministic Codex fixture model"
                          }
                        ]}
                   end
                 )

        assert result.mode == :owned_repo
        assert result.repo == nil
      end)

    assert output =~ "Repository mode (managed/owned)"
    assert output =~ "Workspace path"
    assert output =~ "Alignment/Baseline Agent backend type:"
    assert output =~ "Select Alignment/Baseline Agent backend [1]:"
    assert output =~ "Select Alignment/Baseline Agent approval policy [1]:"
    assert output =~ "Select Alignment/Baseline Agent sandbox policy [1]:"
    assert output =~ "Alignment/Baseline Agent reasoning effort:"
    assert output =~ "Select Alignment/Baseline Agent reasoning effort [3]:"
    assert output =~ "Iteration Agent backend type:"
    assert output =~ "Select Iteration Agent backend [2]:"
    assert output =~ "Select Iteration Agent approval policy [1]:"
    assert output =~ "Select Iteration Agent sandbox policy [1]:"
    assert output =~ "Iteration Agent reasoning effort:"
    assert output =~ "Select Iteration Agent reasoning effort [3]:"
    assert output =~ "4) xhigh · More reasoning for difficult tasks"
    refute output =~ "[codex_app_server]"
    refute output =~ "[cursor_acp]"
    assert output =~ "Available Alignment/Baseline Agent models (cursor)"
    assert output =~ "cursor-test-model · Cursor Test"
    assert output =~ "Available Iteration Agent models (codex)"
    assert output =~ "codex-test-model · Codex Test"
    assert output =~ "Integration Agent backend type:"
    assert output =~ "Available Integration Agent models (codex)"
    assert output =~ "Concurrent Iteration Agents"
    assert output =~ "Configure Git sync?"

    assert {:ok, config} =
             Config.load(Path.join(workspace, "pika.yaml"), workspace: workspace)

    assert config.port == 18_082
    assert config.backend["type"] == "cursor_acp"
    assert config.backend["model"] == "cursor-test-model"
    assert config.backend["reasoning_effort"] == "xhigh"
    assert config.backend["approval_policy"] == "auto_review"
    assert config.backend["sandbox_policy"] == "enabled"
    assert Enum.all?(config.campaign["iteration_agents"], &(&1["backend"] == "codex_app_server"))
    assert Enum.all?(config.campaign["iteration_agents"], &(&1["model"] == "codex-test-model"))

    assert Enum.all?(
             config.campaign["iteration_agents"],
             &(&1["approval_policy"] == "on_request")
           )

    assert Enum.all?(
             config.campaign["iteration_agents"],
             &(&1["sandbox_policy"] == "workspace_write")
           )

    assert length(config.campaign["iteration_agents"]) == 2
    assert config.campaign["integration_agent"]["backend"] == "codex_app_server"
    assert config.campaign["integration_agent"]["model"] == "codex-test-model"
    assert Git.run!(Path.join(workspace, "repo"), ["branch", "--show-current"]) == "pika/best"
  end

  test "keeps --backend as a shorthand for both Agent stages" do
    workspace = CampaignFixtures.workspace()

    capture_io(fn ->
      assert {:ok, _result} =
               Init.run(
                 workspace: workspace,
                 owned: true,
                 backend: "cursor",
                 no_sync: true,
                 yes: true
               )
    end)

    assert {:ok, config} =
             Config.load(Path.join(workspace, "pika.yaml"), workspace: workspace)

    assert config.backend["type"] == "cursor_acp"
    assert config.backend["approval_policy"] == "force"
    assert config.backend["sandbox_policy"] == "disabled"
    assert Enum.all?(config.campaign["iteration_agents"], &(&1["backend"] == "cursor_acp"))
  end

  test "refuses to place Workspace state inside the managed repository" do
    repo = AlignmentFixtures.git_repo()
    workspace = Path.join(repo, ".pika-workspace")

    assert {:error, {:workspace_inside_managed_repo, ^workspace, _repo}} =
             Init.run(workspace: workspace, repo: repo, yes: true)

    refute File.exists?(workspace)
    assert Git.clean?(repo)
  end
end
