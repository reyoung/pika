defmodule Pika.Baseline.QuestionsTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, SessionBinding}
  alias Pika.Baseline.Questions
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  setup do
    root = Path.join(System.tmp_dir!(), "pika-v2-questions-#{System.unique_integer([:positive])}")
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

    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO baseline_revisions(optimization_id, revision, status, work_relative_path, inserted_at, updated_at) VALUES ('optimization', 0, 'drafting', 'baseline/revisions/000000', ?, ?)",
      [now, now]
    )

    [[baseline_id]] = Repo.query!("SELECT last_insert_rowid()").rows

    assert {:ok, session} =
             ConversationJournal.start_session(
               "baseline_alignment",
               :baseline_revision,
               to_string(baseline_id),
               %{},
               "prompt",
               "context"
             )

    questions = start_supervised!({Questions, name: nil})

    binding = %SessionBinding{
      actor: self(),
      role_id: "baseline_alignment",
      work_kind: :baseline_revision,
      work_id: to_string(baseline_id),
      session_id: session.id,
      context_file: Path.join(workspace, "context.json"),
      work_root: workspace
    }

    on_exit(fn -> File.rm_rf!(root) end)
    %{binding: binding, questions: questions}
  end

  test "persists a whole batch and returns answers to the blocked Agent call", %{
    binding: binding,
    questions: server
  } do
    questions = [
      %{
        "id" => "target",
        "question" => "Target 是什么？",
        "options" => [
          %{"label" => "A", "description" => "方案 A"},
          %{"label" => "B", "description" => "方案 B"}
        ]
      },
      %{
        "id" => "metric",
        "question" => "主指标是什么？",
        "options" => [
          %{"label" => "延迟", "description" => "最小化延迟"},
          %{"label" => "吞吐", "description" => "最大化吞吐"}
        ]
      }
    ]

    task = Task.async(fn -> Questions.ask(binding, questions, server) end)
    assert eventually(fn -> Questions.pending(server) != nil end)
    batch = Questions.pending(server)
    assert batch.questions == questions

    answers = [
      %{"id" => "target", "answer" => "A"},
      %{"id" => "metric", "answer" => "延迟"}
    ]

    assert {:ok, completed} = Questions.answer(batch.id, answers, server)
    assert completed.status == "answered"
    assert completed.answers == answers
    assert Task.await(task) == {:ok, answers}
    assert Questions.pending(server) == nil
  end

  defp eventually(check, attempts \\ 100)
  defp eventually(_check, 0), do: false

  defp eventually(check, attempts) do
    if check.() do
      true
    else
      Process.sleep(5)
      eventually(check, attempts - 1)
    end
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
