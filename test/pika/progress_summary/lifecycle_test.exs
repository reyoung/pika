defmodule Pika.ProgressSummary.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.ConversationJournal
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.ProgressSummary.{Lifecycle, PromptInput}
  alias Pika.Repo

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-progress-summary-#{System.unique_integer([:positive])}"
      )

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
    %{config: config, workspace: workspace}
  end

  test "creates a timezone-sharded frozen request and accepts only summary.md", %{
    config: config,
    workspace: workspace
  } do
    write_turn("baseline_alignment", "baseline_revision", "0", "正在梳理 baseline")
    start = ~U[2026-08-24 07:25:00Z]

    assert {:ok, :scheduled} = Lifecycle.tick(config, start)
    assert {:ok, :not_due} = Lifecycle.tick(config, DateTime.add(start, 299, :second))

    assert {:ok, {:requested, request}} =
             Lifecycle.tick(config, DateTime.add(start, 300, :second))

    assert request.sequence == 1

    assert request.relative_directory ==
             "progress-summaries/2026-08-24/153000-00000001"

    root = Path.join(workspace, request.relative_directory)
    assert File.regular?(Path.join(root, "status.json"))
    assert File.regular?(Path.join(root, "messages.jsonl"))
    refute File.exists?(Path.join(root, "previous-summary.md"))

    status = root |> Path.join("status.json") |> File.read!() |> Jason.decode!()
    assert status["schema_version"] == 1
    assert status["snapshot_cursor"] == 1
    assert status["optimization"]["status"] == "aligning_baseline"

    assert [%{"cursor" => 1, "role" => "baseline_alignment"}] =
             root
             |> Path.join("messages.jsonl")
             |> read_jsonl()
             |> Enum.map(&Map.take(&1, ["cursor", "role"]))

    assert [%{role_id: "progress_summary", work_id: work_id}] = Lifecycle.project_work()
    assert work_id == to_string(request.id)

    context_file = Path.join(root, "context.json")
    File.write!(context_file, Jason.encode!(%{"schema_version" => 1}))
    assert {:ok, prompt} = PromptInput.render(request.id, context_file)
    assert prompt =~ "Progress Summary Agent"
    assert prompt =~ Path.join(root, "status.json")

    assert {:ok, running} = Lifecycle.start(request.id)
    assert running.attempt_sequence == 1

    File.write!(Path.join(root, "wrong.md"), "wrong")

    assert {:error, :progress_summary_path_must_be_summary_md} =
             Lifecycle.submit(request.id, "wrong.md", config, DateTime.add(start, 301, :second))

    File.write!(Path.join(root, "summary.md"), "Baseline 正在梳理，尚无终态结果。\n")

    assert {:ok, %{summary: summary, next_request: nil}} =
             Lifecycle.submit(
               request.id,
               "summary.md",
               config,
               DateTime.add(start, 301, :second)
             )

    assert summary.sequence == 1
    assert summary.summary_relative_path == Path.join(request.relative_directory, "summary.md")
    assert Lifecycle.latest() == summary
    assert Lifecycle.project_work() == []
  end

  test "coalesces ticks while active and snapshots only incremental Turns next", %{
    config: config,
    workspace: workspace
  } do
    write_turn("baseline_verify", "baseline_revision", "1", "开始全量验证")
    start = ~U[2026-08-24 07:25:00Z]
    assert {:ok, :scheduled} = Lifecycle.tick(config, start)

    assert {:ok, {:requested, first}} =
             Lifecycle.tick(config, DateTime.add(start, 300, :second))

    assert {:ok, _running} = Lifecycle.start(first.id)
    write_turn("iteration", "attempt", "1", "Attempt 1 正在测量")

    assert {:ok, {:coalesced, coalesced}} =
             Lifecycle.tick(config, DateTime.add(start, 600, :second))

    assert coalesced.id == first.id
    first_root = Path.join(workspace, first.relative_directory)
    File.write!(Path.join(first_root, "summary.md"), "第一份摘要\n")

    assert {:ok, %{summary: first_summary, next_request: second}} =
             Lifecycle.submit(
               first.id,
               "summary.md",
               config,
               DateTime.add(start, 601, :second)
             )

    assert first_summary.sequence == 1
    assert second.sequence == 2
    assert second.relative_directory =~ "/153501-00000002"

    second_root = Path.join(workspace, second.relative_directory)
    assert File.read!(Path.join(second_root, "previous-summary.md")) == "第一份摘要\n"

    assert [%{"cursor" => 2, "role" => "iteration", "work_id" => "1"}] =
             second_root |> Path.join("messages.jsonl") |> read_jsonl()
  end

  test "retries a failed Summary Agent up to its frozen attempt limit", %{config: config} do
    start = ~U[2026-08-24 07:25:00Z]
    assert {:ok, :scheduled} = Lifecycle.tick(config, start)

    assert {:ok, {:requested, request}} =
             Lifecycle.tick(config, DateTime.add(start, 300, :second))

    assert {:ok, first} = Lifecycle.start(request.id)
    assert first.attempt_sequence == 1
    assert first.max_attempts == 2

    assert {:ok, retry} = Lifecycle.fail_attempt(request.id, :backend_crashed)
    assert retry.status == "requested"

    assert {:ok, second} = Lifecycle.start(request.id)
    assert second.attempt_sequence == 2

    assert {:ok, failed} = Lifecycle.fail_attempt(request.id, :backend_crashed_again)
    assert failed.status == "failed"
    assert failed.failure_reason =~ "backend_crashed_again"
    assert Lifecycle.project_work() == []
  end

  defp write_turn(role, work_kind, work_id, content) do
    assert {:ok, session} =
             ConversationJournal.start_session(role, work_kind, work_id, %{}, "prompt", "context")

    assert {:ok, turn} =
             ConversationJournal.start_turn(session.id, [
               %{"role" => "user", "content" => content}
             ])

    assert {:ok, _turn} = ConversationJournal.finish_turn(turn.id, "completed")
  end

  defp read_jsonl(path) do
    path
    |> File.stream!()
    |> Enum.map(&Jason.decode!/1)
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
      progress_summary:
        interval: 5m
        timezone: Asia/Shanghai
        backend: codex
        approval_policy: never
        sandbox: read-only
        max_followups: 1
    """
  end
end
