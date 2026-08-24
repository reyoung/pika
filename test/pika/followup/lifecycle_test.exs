defmodule Pika.Followup.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.ConversationJournal
  alias Pika.Followup.{Lifecycle, PromptInput}
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-followup-#{System.unique_integer([:positive])}")

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

    baseline_id = insert_baseline_revision(workspace)

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline_id: baseline_id, config: config, workspace: workspace}
  end

  test "dedicated generator uses frozen history, retries, delivers, and exhausts durably", %{
    baseline_id: baseline_id,
    config: config
  } do
    session = target_session("baseline_verify", "baseline_revision", to_string(baseline_id))
    write_turn(session.id, "全量 Benchmark 已启动", "completed_without_terminal_mcp")

    assert {:ok, first} =
             Lifecycle.request(
               config,
               session.id,
               "finish_baseline_verification",
               %{domain_status: "verifying"}
             )

    assert first.status == "generating"
    assert first.target_followup_sequence == 1
    assert first.generator_max_attempts == 2
    assert first.relative_directory =~ "follow-ups/baseline_verify/#{baseline_id}/000001"
    assert ConversationJournal.session(session.id).status == "awaiting_followup"

    root = Lifecycle.workdir(first)

    assert [%{"input_messages" => [%{"content" => "全量 Benchmark 已启动"}]}] =
             root |> Path.join("messages.jsonl") |> read_jsonl()

    state = root |> Path.join("state.json") |> File.read!() |> Jason.decode!()
    assert state["required_operation"] == "finish_baseline_verification"
    assert state["target_followups_remaining"] == 1
    assert state["domain_status"] == "verifying"

    context_file = Path.join(root, "context.json")
    File.write!(context_file, Jason.encode!(%{"schema_version" => 1}))
    assert {:ok, prompt} = PromptInput.render(first.id, context_file)
    assert prompt =~ "Baseline Verify Follow-up Agent"
    assert prompt =~ Path.join(root, "messages.jsonl")

    assert [%{role_id: "baseline_verify_followup", work_id: work_id}] =
             Lifecycle.project_work()

    assert work_id == to_string(first.id)
    assert {:ok, running} = Lifecycle.start_generator(first.id)
    assert running.generator_attempt_sequence == 1

    assert {:ok, generated} =
             Lifecycle.submit_message(
               first.id,
               "等待 Benchmark 完整输出，再核对文件并调用终态 MCP。"
             )

    assert generated.status == "generated"
    assert {:ok, delivered} = Lifecycle.deliver(first.id)
    assert delivered.message =~ "等待 Benchmark"
    assert ConversationJournal.session(session.id).status == "running"
    assert {:ok, _running} = Lifecycle.target_turn_started(first.id)

    write_turn(session.id, "继续后仍未提交 Result", "completed_without_terminal_mcp")
    assert {:ok, second} = Lifecycle.target_turn_incomplete(first.id, config, %{phase: "result"})
    assert second.target_followup_sequence == 2

    assert {:ok, generator_attempt_1} = Lifecycle.start_generator(second.id)
    assert generator_attempt_1.generator_attempt_sequence == 1
    assert {:ok, retry} = Lifecycle.generator_failed(second.id, :backend_crash)
    assert retry.status == "generating"

    assert {:ok, generator_attempt_2} = Lifecycle.start_generator(second.id)
    assert generator_attempt_2.generator_attempt_sequence == 2
    assert {:ok, exhausted} = Lifecycle.generator_failed(second.id, :missing_mcp_again)
    assert exhausted.status == "exhausted"
    assert exhausted.failure_reason == "follow_up_generator_exhausted"

    assert Persistence.current().status == "failed"
    assert Persistence.current().stop_reason == "follow_up_generator_exhausted"

    assert [["failed", "follow_up_generator_exhausted"]] =
             Repo.query!("SELECT status, terminal_reason FROM baseline_revisions WHERE id = ?", [
               baseline_id
             ]).rows
  end

  test "omitted generator directly yields the fixed continue message", %{
    baseline_id: baseline_id,
    config: config
  } do
    config = %{config | baseline_verify_followup: nil}
    session = target_session("baseline_verify", "baseline_revision", to_string(baseline_id))
    write_turn(session.id, "等待用户输入", "completed_without_terminal_mcp")

    assert {:ok, request} =
             Lifecycle.request(config, session.id, "finish_baseline_verification", %{})

    assert request.status == "generated"
    assert request.message == "继续"
    assert request.generator_role == nil
    assert Lifecycle.project_work() == []
    assert {:error, :followup_generator_not_configured} = PromptInput.build(request.id, "missing")

    assert {:ok, delivered} = Lifecycle.deliver(request.id)
    assert delivered.status == "delivered"
    assert ConversationJournal.session(session.id).status == "running"
  end

  test "Iteration follow-up exhaustion rejects the Attempt and records summary.jsonl", %{
    baseline_id: baseline_id,
    config: config,
    workspace: workspace
  } do
    attempt_id = insert_attempt(baseline_id, workspace)
    config = put_in(config.iteration.max_followups, 0)
    session = target_session("iteration", "attempt", to_string(attempt_id))
    write_turn(session.id, "没有调用 finish_iteration", "completed_without_terminal_mcp")

    assert {:ok, exhausted} =
             Lifecycle.request(config, session.id, "finish_iteration", %{attempt_id: attempt_id})

    assert exhausted.status == "exhausted"
    assert exhausted.failure_reason == "follow_up_exhausted"

    assert [["rejected", "rejected", "follow_up_exhausted"]] =
             Repo.query!("SELECT status, outcome, failure_reason FROM attempts WHERE id = ?", [
               attempt_id
             ]).rows

    summary_path = Path.join([workspace, "attempts", "000001", "summary.jsonl"])

    assert [%{"outcome" => "rejected", "failure_reason" => "follow_up_exhausted"}] =
             read_jsonl(summary_path)
  end

  defp target_session(role, work_kind, work_id) do
    assert {:ok, session} =
             ConversationJournal.start_session(role, work_kind, work_id, %{}, "prompt", "context")

    session
  end

  defp write_turn(session_id, content, ended_reason) do
    assert {:ok, turn} =
             ConversationJournal.start_turn(session_id, [
               %{"role" => "user", "content" => content}
             ])

    assert {:ok, _turn} = ConversationJournal.finish_turn(turn.id, ended_reason)
  end

  defp insert_baseline_revision(workspace) do
    now = System.system_time(:microsecond)
    relative = "baseline/revisions/0"
    File.mkdir_p!(Path.join(workspace, relative))

    Repo.query!(
      """
      INSERT INTO baseline_revisions(
        optimization_id, revision, status, work_relative_path, inserted_at, updated_at
      ) VALUES ('optimization', 0, 'verifying', ?, ?, ?)
      """,
      [relative, now, now]
    )

    [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
    id
  end

  defp insert_attempt(baseline_id, workspace) do
    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO sampling_revisions(optimization_id, baseline_revision_id, sequence, cause, created_at) VALUES ('optimization', ?, 0, 'test', ?)",
      [baseline_id, now]
    )

    [[sampling_id]] = Repo.query!("SELECT last_insert_rowid()").rows
    attempt_root = Path.join([workspace, "attempts", "000001"])
    File.mkdir_p!(attempt_root)
    File.write!(Path.join(attempt_root, "message.jsonl"), "")
    File.write!(Path.join(attempt_root, "summary.jsonl"), "")

    Repo.query!(
      """
      INSERT INTO attempts(
        id, optimization_id, status, work_relative_path, branch, slot_index,
        base_best_revision, base_sha, sampling_revision_id, current_iteration_round,
        inserted_at, updated_at
      ) VALUES (1, 'optimization', 'iterating', 'attempts/000001',
                'pika/attempt/000001', 0, 0, ?, ?, 1, ?, ?)
      """,
      [String.duplicate("a", 40), sampling_id, now, now]
    )

    1
  end

  defp read_jsonl(path), do: path |> File.stream!() |> Enum.map(&Jason.decode!/1)

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
        max_followups: 2
      baseline_verify_followup:
        backend: codex
        approval_policy: never
        sandbox: read-only
        generator_max_attempts: 2
      iteration:
        max_followups: 2
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write
      iteration_followup:
        backend: codex
        approval_policy: never
        sandbox: read-only
        generator_max_attempts: 2
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      integration_followup:
        backend: codex
        approval_policy: never
        sandbox: read-only
        generator_max_attempts: 2
    """
  end
end
