defmodule Pika.Optimization.InitTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.{Config, ConfigFile, Init}
  alias Pika.Git

  test "creates a self-contained v2 YAML with expanded Role configs" do
    root = Path.join(System.tmp_dir!(), "pika-v2-init-#{System.unique_integer([:positive])}")
    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "kernel.py"), "def run(): return 1\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "initial"])
    on_exit(fn -> File.rm_rf!(root) end)

    assert {:ok, config} =
             Init.run(workspace, repo,
               backend: "codex",
               model: "gpt-test",
               iteration_agents: 2,
               progress_summary: true
             )

    assert %Config{} = config
    assert length(config.iteration.agents) == 2
    assert config.progress_summary.time_zone == "Asia/Shanghai"
    assert is_binary(config.token)
    assert byte_size(config.token) >= 43
    assert File.regular?(Path.join(workspace, "pika.yaml"))
    assert Bitwise.band(File.stat!(Path.join(workspace, "pika.yaml")).mode, 0o777) == 0o600
    contents = File.read!(Path.join(workspace, "pika.yaml"))
    refute contents =~ "campaign"
    refute contents =~ "sync"
    refute contents =~ "profile"
    assert contents =~ "max_pending_attempts: 0"
    assert contents =~ "token: #{Jason.encode!(config.token)}"

    assert {:error, {:v2_config_already_exists, _path}} = Init.run(workspace, repo)

    assert {:error, :cursor_acp_deprecated} =
             Init.run(Path.join(root, "legacy-workspace"), repo, backend: "cursor")

    primary = ConfigFile.agent("codex", "model", "high")
    legacy = ConfigFile.agent("cursor_acp", "legacy", "high")

    assert {:error, :cursor_acp_deprecated} =
             Init.run(Path.join(root, "legacy-fallback-workspace"), repo,
               agents: %{integration: %{primary | fallbacks: [legacy]}}
             )
  end
end
