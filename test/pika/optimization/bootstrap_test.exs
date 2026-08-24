defmodule Pika.Optimization.BootstrapTest do
  use ExUnit.Case, async: false

  alias Pika.Optimization.{Bootstrap, Persistence}
  alias Pika.{Git, Repo}

  test "migrates, initializes, creates Baseline Revision 0, and then recovers" do
    root = Path.join(System.tmp_dir!(), "pika-v2-bootstrap-#{System.unique_integer([:positive])}")
    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    File.mkdir_p!(workspace)
    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "kernel.py"), "def run(): return 1\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "initial"])
    config_path = Path.join(workspace, "pika.yaml")
    File.write!(config_path, yaml(repo, workspace))
    on_exit(fn -> File.rm_rf!(root) end)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert {:ok, bootstrap} = Bootstrap.start_link(name: nil, config_path: config_path)
    snapshot = Bootstrap.snapshot(bootstrap)
    assert snapshot.recovery == :initialized
    assert snapshot.optimization.status == "aligning_baseline"
    assert snapshot.baseline.revision.revision == 0
    assert File.dir?(snapshot.baseline.paths.repo)
    initial_sha = Persistence.current().initial_sha

    GenServer.stop(bootstrap)
    assert {:ok, recovered} = Bootstrap.start_link(name: nil, config_path: config_path)
    assert Bootstrap.snapshot(recovered).recovery == :recovered
    assert Persistence.current().initial_sha == initial_sha

    GenServer.stop(recovered)
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
