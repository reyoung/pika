defmodule Pika.Optimization.ArtifactStoreTest do
  use ExUnit.Case, async: false

  alias Pika.Optimization.{ArtifactStore, Config, FileContract, Persistence}
  alias Pika.Repo

  setup do
    root = Path.join(System.tmp_dir!(), "pika-v2-artifacts-#{System.unique_integer([:positive])}")
    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    File.mkdir_p!(workspace)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(repo, workspace))
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
             Persistence.initialize_or_recover(config, String.duplicate("a", 40))

    on_exit(fn -> File.rm_rf!(root) end)
    %{workspace: workspace}
  end

  test "replays an identical registration but rejects path identity mutation", %{
    workspace: workspace
  } do
    File.write!(Path.join(workspace, "result.json"), "{\"value\":1}")
    assert {:ok, _contents, first} = FileContract.read(workspace, "result.json")
    assert {:ok, artifact_id} = ArtifactStore.register("attempt", "1", "result", first)
    assert {:ok, ^artifact_id} = ArtifactStore.register("attempt", "1", "result", first)

    File.write!(Path.join(workspace, "result.json"), "{\"value\":2}")
    assert {:ok, _contents, changed} = FileContract.read(workspace, "result.json")

    assert {:error, {:artifact_immutable_conflict, "result.json", conflict}} =
             ArtifactStore.register("attempt", "1", "result", changed)

    assert conflict.expected.sha256 == first.sha256
    assert conflict.actual.sha256 == changed.sha256

    assert [[sha256]] =
             Repo.query!("SELECT sha256 FROM artifacts WHERE id = ?", [artifact_id]).rows

    assert sha256 == first.sha256
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
