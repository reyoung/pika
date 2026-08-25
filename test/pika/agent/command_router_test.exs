Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.CommandRouterTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{CommandRouter, SessionBinding}
  alias Pika.Baseline.Lifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-command-router-#{System.unique_integer([:positive])}")

    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(baseline.repo, workspace))
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
             Persistence.initialize_or_recover(config, baseline.development_sha)

    assert {:ok, draft} = Lifecycle.ensure_draft(baseline.root)
    context_file = Path.join(baseline.root, "context.json")
    File.write!(context_file, Jason.encode!(%{"schema_version" => 1}))

    binding = %SessionBinding{
      actor: self(),
      role_id: "baseline_alignment",
      work_kind: :baseline_revision,
      work_id: to_string(draft.id),
      session_id: Ecto.UUID.generate(),
      context_file: context_file,
      work_root: baseline.root
    }

    insert_answered_question_batch(draft.id, binding.session_id)

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline: baseline, binding: binding, config: config}
  end

  test "requires an answered ask_questions batch before definition submission", %{
    binding: binding,
    config: config
  } do
    Repo.query!(
      "DELETE FROM baseline_question_batches WHERE baseline_revision_id = ?",
      [String.to_integer(binding.work_id)]
    )

    args = %{
      "definition_path" => "baseline-definition.json",
      "idempotency_key" => "submit-without-questions"
    }

    assert {:error, :baseline_questions_required} =
             CommandRouter.invoke(binding, "submit_baseline_definition", args, config)
  end

  test "authorizes get_context and ask_questions from the frozen binding", %{
    binding: binding,
    config: config
  } do
    assert {:ok, context} = CommandRouter.invoke(binding, "get_context", %{}, config)
    assert context.context_file == binding.context_file
    assert context.role == "baseline_alignment"

    questions = [
      %{
        "id" => "target-source",
        "question" => "优化目标来自哪里？",
        "options" => [
          %{"label" => "当前代码", "description" => "使用当前仓库实现"},
          %{"label" => "独立目标", "description" => "生成独立 Target"}
        ]
      }
    ]

    handler = fn received_binding, received_questions ->
      assert received_binding.session_id == binding.session_id
      assert received_questions == questions
      {:ok, [%{"id" => "target-source", "answer" => "独立目标"}]}
    end

    assert {:ok, %{answers: [%{"answer" => "独立目标"}]}} =
             CommandRouter.invoke(
               binding,
               "ask_questions",
               %{"questions" => questions},
               config,
               question_handler: handler
             )

    assert {:error, {:forbidden_operation, "baseline_alignment", "finish_iteration"}} =
             CommandRouter.invoke(binding, "finish_iteration", %{}, config)
  end

  test "file digest participates in command idempotency", %{
    baseline: baseline,
    binding: binding,
    config: config
  } do
    args = %{
      "definition_path" => "baseline-definition.json",
      "idempotency_key" => "submit-definition"
    }

    assert {:ok, first} =
             CommandRouter.invoke(binding, "submit_baseline_definition", args, config)

    assert first["status"] == "awaiting_review"

    assert {:ok, replayed} =
             CommandRouter.invoke(binding, "submit_baseline_definition", args, config)

    assert replayed == first

    definition = V2BaselineFixtures.read_json(baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_json(
      baseline.root,
      "baseline-definition.json",
      Map.put(definition, "summary", "changed after submit")
    )

    assert {:error, :idempotency_conflict} =
             CommandRouter.invoke(binding, "submit_baseline_definition", args, config)
  end

  test "rejects missing idempotency and unknown arguments before domain mutation", %{
    binding: binding,
    config: config
  } do
    assert {:error, {:invalid_tool_arguments, errors}} =
             CommandRouter.invoke(
               binding,
               "submit_baseline_definition",
               %{"definition_path" => "baseline-definition.json", "extra" => true},
               config
             )

    assert Enum.any?(errors, &(&1.path == "/idempotency_key"))
    assert Enum.any?(errors, &(&1.path == "/extra"))
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

  defp insert_answered_question_batch(revision_id, session_id) do
    now = System.system_time(:microsecond)

    Repo.query!(
      """
      INSERT INTO baseline_question_batches(
        id, optimization_id, baseline_revision_id, session_id, status,
        questions_json, answers_json, created_at, answered_at
      ) VALUES (?, 'optimization', ?, ?, 'answered', ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        revision_id,
        session_id,
        Jason.encode!([
          %{
            "id" => "measurement",
            "question" => "确认测量协议？",
            "options" => [
              %{"label" => "确认", "description" => "使用当前协议"},
              %{"label" => "修改", "description" => "提供自定义协议"}
            ]
          }
        ]),
        Jason.encode!([%{"id" => "measurement", "answer" => "确认"}]),
        now,
        now
      ]
    )
  end
end
