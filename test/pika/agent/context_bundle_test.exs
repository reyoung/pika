Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.ContextBundleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ContextBundle, ConversationJournal}
  alias Pika.Attempt.Scheduler
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-context-#{System.unique_integer([:positive])}")

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

    assert {:ok, _draft} = BaselineLifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             BaselineLifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, approved} =
             BaselineLifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             BaselineLifecycle.finish_verification(
               0,
               baseline.root,
               "baseline-verification-result.json"
             )

    on_exit(fn -> File.rm_rf!(root) end)
    %{config: config}
  end

  test "freezes baseline, sampling, guidance, Best, and events for one Session", %{config: config} do
    assert {:ok, guidance} = Scheduler.add_guidance("优先优化访存合并")
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    now = System.system_time(:microsecond)

    Repo.query!(
      """
      INSERT INTO baseline_revisions(
        optimization_id, revision, status, work_relative_path, terminal_reason,
        inserted_at, updated_at
      ) VALUES ('optimization', 1, 'superseded', 'baseline/revisions/000001',
                'replaced by reconsidered v0', ?, ?)
      """,
      [now, now]
    )

    session_id = ConversationJournal.allocate_session_id()

    assert {:ok, bundle} =
             ContextBundle.build(
               config,
               session_id,
               "iteration",
               :attempt,
               to_string(attempt.id)
             )

    assert bundle.context.schema_version == 1
    assert bundle.context.role == "iteration"
    assert bundle.context.attempt_id == attempt.id
    assert bundle.context.iteration_round == 1
    assert bundle.context.baseline_revision == 0
    assert bundle.context.sampling_case_ids == [0, 1]
    assert bundle.context.best_sha_at_session_start == attempt.base_sha

    for filename <-
          ~w(context.json baseline-definition.json cases.json metrics.json guidance.md events.jsonl) do
      assert File.regular?(Path.join(bundle.directory, filename))
    end

    assert File.read!(Path.join(bundle.directory, "guidance.md")) == guidance.body <> "\n"

    assert {:ok, session} =
             ConversationJournal.start_session(
               "iteration",
               :attempt,
               to_string(attempt.id),
               %{"backend" => "codex"},
               "system prompt",
               bundle.contents,
               id: session_id
             )

    assert session.id == session_id
    assert session.context_sha256 == bundle.sha256

    assert {:error, {:context_bundle_already_exists, directory}} =
             ContextBundle.build(
               config,
               session_id,
               "iteration",
               :attempt,
               to_string(attempt.id)
             )

    assert directory == bundle.directory

    replacement_id = ConversationJournal.allocate_session_id()

    assert {:ok, replacement} =
             ContextBundle.build(
               config,
               replacement_id,
               "iteration",
               :attempt,
               to_string(attempt.id)
             )

    refute replacement.directory == bundle.directory
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
