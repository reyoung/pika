defmodule Pika.Optimization.RuntimeTest do
  use ExUnit.Case, async: false

  alias Pika.Optimization.{Config, Persistence, Runtime}
  alias Pika.Repo

  @initial_sha String.duplicate("a", 40)

  setup do
    root = Path.join(System.tmp_dir!(), "pika-v2-runtime-#{System.unique_integer([:positive])}")
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
             Persistence.initialize_or_recover(config, @initial_sha)

    on_exit(fn -> File.rm_rf!(root) end)
    %{}
  end

  test "pauses, resumes, drains, and completes the Optimization" do
    set_status("optimizing")
    runtime = start_supervised!({Runtime, name: nil})

    assert {:ok, paused} = Runtime.pause(runtime)
    assert paused.status == "paused"
    assert paused.resume_status == "optimizing"

    assert {:ok, resumed} = Runtime.resume(runtime)
    assert resumed.status == "optimizing"
    assert is_nil(resumed.resume_status)

    assert {:ok, draining} = Runtime.drain(runtime, "max duration")
    assert draining.status == "draining"
    assert draining.stop_reason == "max duration"

    assert {:ok, completed} = Runtime.complete_if_drained(runtime)
    assert completed.status == "completed"
    assert {:error, {:already_terminal, "completed"}} = Runtime.stop_now(runtime, "late stop")
  end

  test "Stop Now and failure are durable terminal states" do
    stopped_runtime = start_supervised!({Runtime, name: nil})
    assert {:ok, stopped} = Runtime.stop_now(stopped_runtime, "user requested")
    assert stopped.status == "stopped"
    assert stopped.stop_reason == "user requested"
    stop_supervised(Runtime)

    set_status("aligning_baseline")
    failed_runtime = start_supervised!({Runtime, name: nil})
    assert {:ok, failed} = Runtime.fail(failed_runtime, "follow_up_exhausted")
    assert failed.status == "failed"
    assert Persistence.current().status == "failed"
  end

  test "Stop Now cancels active Attempts and aborts pending Git intents" do
    insert_active_attempt_and_intent()
    runtime = start_supervised!({Runtime, name: nil})
    assert {:ok, %{status: "stopped"}} = Runtime.stop_now(runtime, "operator")

    assert [["cancelled", "rejected", "operator"]] =
             Repo.query!("SELECT status, outcome, failure_reason FROM attempts WHERE id = 1").rows

    assert [["aborted"]] = Repo.query!("SELECT state FROM operation_intents").rows
  end

  test "rejects invalid transitions" do
    runtime = start_supervised!({Runtime, name: nil})
    assert {:error, {:cannot_pause, "aligning_baseline"}} = Runtime.pause(runtime)
    assert {:error, {:cannot_resume, "aligning_baseline", nil}} = Runtime.resume(runtime)
    assert {:error, {:not_draining, "aligning_baseline"}} = Runtime.complete_if_drained(runtime)
  end

  defp set_status(status) do
    Repo.query!("UPDATE optimizations SET status = ? WHERE id = 'optimization'", [status])
  end

  defp insert_active_attempt_and_intent do
    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO baseline_revisions(optimization_id, revision, status, work_relative_path, inserted_at, updated_at) VALUES ('optimization', 0, 'accepted', 'baseline/revisions/000000', ?, ?)",
      [now, now]
    )

    [[baseline_id]] = Repo.query!("SELECT last_insert_rowid()").rows

    Repo.query!(
      "INSERT INTO sampling_revisions(optimization_id, baseline_revision_id, sequence, cause, created_at) VALUES ('optimization', ?, 0, 'test', ?)",
      [baseline_id, now]
    )

    [[sampling_id]] = Repo.query!("SELECT last_insert_rowid()").rows

    Repo.query!(
      """
      INSERT INTO attempts(
        id, optimization_id, status, work_relative_path, branch, slot_index,
        base_best_revision, base_sha, sampling_revision_id, current_iteration_round,
        inserted_at, updated_at
      ) VALUES (1, 'optimization', 'iterating', 'attempts/000001',
                'pika/attempt/000001', 0, 0, ?, ?, 1, ?, ?)
      """,
      [@initial_sha, sampling_id, now, now]
    )

    Repo.query!(
      """
      INSERT INTO operation_intents(
        id, optimization_id, kind, owner_type, owner_id, state,
        idempotency_key, created_at, updated_at
      ) VALUES ('intent-1', 'optimization', 'best_update', 'attempt', '1', 'pending',
                'intent-key', ?, ?)
      """,
      [now, now]
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
