defmodule Pika.Optimization.PersistenceTest do
  use ExUnit.Case, async: false

  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  @initial_sha String.duplicate("a", 40)

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-persistence-#{System.unique_integer([:positive])}")

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

    on_exit(fn -> File.rm_rf!(root) end)
    %{config: config, repo: repo, root: root, workspace: workspace}
  end

  test "creates only the fresh v2 schema with required SQLite pragmas" do
    tables =
      Repo.query!("SELECT name FROM sqlite_master WHERE type='table'").rows
      |> List.flatten()

    for table <-
          ~w(
            optimizations artifacts baseline_revisions target_snapshots baseline_reviews
            baseline_question_batches baseline_verifications benchmark_cases metric_definitions sampling_revisions
            sampling_revision_cases best_revisions best_metrics guidance_revisions attempts
            iteration_rounds attempt_metrics integration_runs operation_intents agent_sessions
            conversation_turns followup_requests progress_summary_requests progress_summaries
            operation_receipts domain_events
          ) do
      assert table in tables
    end

    refute "campaigns" in tables
    refute "sync_runs" in tables

    assert Persistence.pragma_values() == %{
             foreign_keys: 1,
             journal_mode: "wal",
             synchronous: 2,
             busy_timeout: 5_000
           }
  end

  test "initializes and recovers exactly one Optimization", %{config: config} do
    assert {:ok, optimization, :initialized} =
             Persistence.initialize_or_recover(config, @initial_sha)

    assert optimization.id == "optimization"
    assert optimization.status == "aligning_baseline"
    assert optimization.initial_sha == @initial_sha
    assert optimization.best_branch == "pika/best"
    assert is_nil(optimization.best_sha)

    assert {:ok, recovered, :recovered} =
             Persistence.initialize_or_recover(config, @initial_sha)

    assert recovered.id == optimization.id
    assert [[1]] = Repo.query!("SELECT COUNT(*) FROM optimizations").rows
    assert [[1]] = Repo.query!("SELECT COUNT(*) FROM domain_events").rows

    assert_raise Exqlite.Error, fn ->
      Repo.query!(
        """
        INSERT INTO optimizations(
          id, singleton_key, status, repo_canonical_path, workspace_canonical_path,
          initial_sha, best_branch, config_sha256, inserted_at, updated_at
        ) VALUES ('other', 1, 'aligning_baseline', '/repo', '/workspace', ?, 'pika/best', ?, 1, 1)
        """,
        [@initial_sha, String.duplicate("b", 64)]
      )
    end
  end

  test "rejects recovery identity drift", %{config: config} do
    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, @initial_sha)

    changed_sha = String.duplicate("b", 40)

    assert {:error, {:optimization_identity_mismatch, mismatch}} =
             Persistence.initialize_or_recover(config, changed_sha)

    assert mismatch.expected.initial_sha == @initial_sha
    assert mismatch.actual.initial_sha == changed_sha
  end

  test "Attempt integer IDs are monotonic and never reused", %{config: config} do
    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, @initial_sha)

    now = System.system_time(:microsecond)

    Repo.query!(
      """
      INSERT INTO baseline_revisions(
        optimization_id, revision, status, work_relative_path, inserted_at, updated_at
      ) VALUES ('optimization', 0, 'accepted', 'baseline/revisions/0', ?, ?)
      """,
      [now, now]
    )

    [[baseline_id]] = Repo.query!("SELECT id FROM baseline_revisions WHERE revision = 0").rows

    Repo.query!(
      """
      INSERT INTO sampling_revisions(
        optimization_id, baseline_revision_id, sequence, cause, created_at
      ) VALUES ('optimization', ?, 0, 'baseline', ?)
      """,
      [baseline_id, now]
    )

    [[sampling_id]] = Repo.query!("SELECT id FROM sampling_revisions WHERE sequence = 0").rows

    assert {:ok, 1} = Persistence.allocate_attempt_id()
    insert_attempt(1, sampling_id, now)
    assert {:ok, 2} = Persistence.allocate_attempt_id()
    insert_attempt(2, sampling_id, now)
    Repo.query!("DELETE FROM attempts WHERE id = 2")
    assert {:ok, 3} = Persistence.allocate_attempt_id()
    insert_attempt(3, sampling_id, now)

    assert Repo.query!("SELECT id FROM attempts ORDER BY id").rows == [[1], [3]]
  end

  defp insert_attempt(id, sampling_id, now) do
    Repo.query!(
      """
      INSERT INTO attempts(
        id, optimization_id, status, work_relative_path, branch,
        base_best_revision, base_sha, sampling_revision_id,
        current_iteration_round, inserted_at, updated_at
      ) VALUES (?, 'optimization', 'queued', ?, ?, 0, ?, ?, 1, ?, ?)
      """,
      [
        id,
        "attempts/#{String.pad_leading(Integer.to_string(id), 6, "0")}",
        "pika/attempt/#{String.pad_leading(Integer.to_string(id), 6, "0")}",
        @initial_sha,
        sampling_id,
        now,
        now
      ]
    )
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
