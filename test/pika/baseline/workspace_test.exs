defmodule Pika.Baseline.WorkspaceTest do
  use ExUnit.Case, async: false

  alias Pika.Baseline.Workspace
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.{Git, Repo}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-baseline-workspace-#{System.unique_integer([:positive])}"
      )

    source = Path.join(root, "source")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(source)
    File.mkdir_p!(workspace)
    Git.run!(source, ["init", "--initial-branch=main"])
    Git.run!(source, ["config", "user.name", "Pika Test"])
    Git.run!(source, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(source, "kernel.py"), "def run(): return 1\n")
    Git.run!(source, ["add", "."])
    Git.run!(source, ["commit", "-m", "initial"])
    initial_sha = Git.run!(source, ["rev-parse", "HEAD"])
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(source, workspace))
    assert {:ok, config} = Config.load(config_path)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()

    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, initial_sha)

    on_exit(fn -> File.rm_rf!(root) end)
    %{config: config, initial_sha: initial_sha}
  end

  test "creates a numbered Baseline worktree and reuses its exact identity", %{
    config: config,
    initial_sha: initial_sha
  } do
    assert {:ok, first} = Workspace.ensure_current(config)
    assert first.revision.revision == 0
    assert first.paths.relative_root == "baseline/revisions/000000"
    assert first.paths.branch == "pika/baseline/000000"
    assert Git.run!(first.paths.repo, ["rev-parse", "HEAD"]) == initial_sha
    assert File.dir?(first.paths.target)

    assert {:ok, repeated} = Workspace.ensure_current(config)
    assert repeated.paths == first.paths
    assert repeated.revision.id == first.revision.id
  end

  test "rebuilds a stale Pika revision branch at the required base", %{
    config: config,
    initial_sha: initial_sha
  } do
    File.write!(Path.join(config.repo, "kernel.py"), "def run(): return 2\n")
    Git.run!(config.repo, ["add", "kernel.py"])
    Git.run!(config.repo, ["commit", "-m", "later source commit"])
    stale_sha = Git.run!(config.repo, ["rev-parse", "HEAD"])
    stale_paths = Workspace.paths(config.workspace, 0)

    Git.run!(config.repo, ["branch", stale_paths.branch, stale_sha])
    File.mkdir_p!(stale_paths.root)
    Git.run!(config.repo, ["worktree", "add", stale_paths.repo, stale_paths.branch])
    File.rm_rf!(stale_paths.repo)

    assert {:ok, current} = Workspace.ensure_current(config)
    assert current.revision.revision == 0
    assert Git.run!(current.paths.repo, ["rev-parse", "HEAD"]) == initial_sha
    assert Git.run!(config.repo, ["rev-parse", stale_paths.branch]) == initial_sha
  end

  defp yaml(repo, workspace) do
    """
    version: 2
    repo: #{repo}
    workspace: #{workspace}
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
