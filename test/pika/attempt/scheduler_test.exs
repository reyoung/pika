Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Attempt.SchedulerTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ContextBundle, PromptBuilder, Work}
  alias Pika.Attempt.{PromptInput, Scheduler, Workspace}
  alias Pika.Baseline.Lifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Optimization.Config.ReferenceProject
  alias Pika.Test.V2BaselineFixtures
  alias Pika.{Git, Repo}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-attempt-scheduler-#{System.unique_integer([:positive])}"
      )

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

    assert {:ok, _draft} = Lifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, approved} =
             Lifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             Lifecycle.finish_verification(
               0,
               baseline.root,
               "baseline-verification-result.json"
             )

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline: baseline, config: config, root: root, workspace: workspace}
  end

  test "fills Iteration slots with monotonic integer Attempt worktrees", %{
    baseline: baseline,
    config: config,
    workspace: workspace
  } do
    assert {:ok, attempts} = Scheduler.spawn_available(config)
    assert Enum.map(attempts, & &1.id) == [1, 2]
    assert Enum.map(attempts, & &1.slot_index) == [0, 1]

    for attempt <- attempts do
      paths = Workspace.paths(workspace, attempt.id)
      assert File.dir?(paths.repo)
      assert File.regular?(Path.join(paths.root, "message.jsonl"))
      assert File.regular?(Path.join(paths.root, "summary.jsonl"))
      assert Git.run!(paths.repo, ["rev-parse", "HEAD"]) == baseline.development_sha
      assert Git.run!(paths.repo, ["branch", "--show-current"]) == paths.branch

      [[target_relative_path]] = Repo.query!("SELECT relative_path FROM target_snapshots").rows

      assert File.read_link!(Path.join(paths.repo, "target")) ==
               Path.join(Persistence.current().workspace_canonical_path, target_relative_path)
    end

    assert Enum.map(Scheduler.project_work(), & &1.work_id) == ~w(1 2)
    assert {:ok, []} = Scheduler.spawn_available(config)
  end

  test "max pending gate defaults to waiting for all pending Attempts", %{config: config} do
    assert config.iteration.max_pending_attempts == 0
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)
    set_status(first.id, "ready_for_integration")
    set_status(second.id, "rejected")

    assert Scheduler.pending_count() == 1
    assert {:ok, []} = Scheduler.spawn_available(config)

    set_status(first.id, "rejected")
    assert Scheduler.pending_count() == 0
    assert {:ok, [_first, _second]} = Scheduler.spawn_available(config)
  end

  test "positive max pending values allow new work below the configured threshold", %{
    config: config
  } do
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)
    set_status(first.id, "ready_for_integration")
    set_status(second.id, "rejected")

    limited = %{config | iteration: %{config.iteration | max_pending_attempts: 2}}
    assert Scheduler.pending_count() == 1
    assert {:ok, [_first, _second]} = Scheduler.spawn_available(limited)
  end

  test "refuses to spawn from a mutated Target Snapshot", %{config: config} do
    [[relative_path]] = Repo.query!("SELECT relative_path FROM target_snapshots").rows
    root = Path.join(Persistence.current().workspace_canonical_path, relative_path)
    target_file = Path.join(root, "target.py")
    File.chmod!(target_file, 0o644)
    File.write!(target_file, "tampered\n")

    assert {:error, {:target_snapshot_identity_mismatch, ^target_file, _identity}} =
             Scheduler.spawn_available(config)

    assert Repo.query!("SELECT COUNT(*) FROM attempts").rows == [[0]]
  end

  test "Guidance revisions affect only future Attempts", %{config: config} do
    assert {:ok, first_guidance} = Scheduler.add_guidance("use vectorized loads")
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)
    assert attempt_guidance(first.id) == first_guidance.id
    assert attempt_guidance(second.id) == first_guidance.id

    assert {:ok, second_guidance} = Scheduler.add_guidance("avoid extra shared memory")
    set_status(first.id, "rejected")

    assert {:ok, [third]} = Scheduler.spawn_available(config)
    assert attempt_guidance(second.id) == first_guidance.id
    assert attempt_guidance(third.id) == second_guidance.id
  end

  test "configured Reference Projects flow through Attempt manifests into Iteration prompts", %{
    config: config,
    root: root,
    workspace: workspace
  } do
    reference_repo = Path.join(root, "prompt-reference")
    File.mkdir_p!(reference_repo)
    Git.run!(reference_repo, ["init", "--initial-branch=main"])
    Git.run!(reference_repo, ["config", "user.name", "Pika Test"])
    Git.run!(reference_repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(reference_repo, "kernel.cu"), "reference implementation\n")
    Git.run!(reference_repo, ["add", "."])
    Git.run!(reference_repo, ["commit", "-m", "reference"])
    reference_sha = Git.run!(reference_repo, ["rev-parse", "HEAD"])

    configured = %{
      config
      | reference_projects: [
          %ReferenceProject{
            id: "prompt-reference",
            url: reference_repo,
            description: "Prompt-visible kernel examples",
            revision: nil
          }
        ]
    }

    assert {:ok, attempts} = Scheduler.spawn_available(configured)

    for attempt <- attempts do
      paths = Workspace.paths(workspace, attempt.id)
      assert File.dir?(Path.join(paths.repo, "ref/prompt-reference"))

      assert {:ok, prompt} =
               PromptInput.render(
                 attempt.id,
                 configured.workspace,
                 configured.repo,
                 configured.iteration.history_limit
               )

      assert prompt =~ "## Reference Projects"
      assert prompt =~ "ref/prompt-reference"
      assert prompt =~ "Prompt-visible kernel examples"
      assert prompt =~ reference_sha
    end
  end

  test "stale FIFO head returns to Iteration and remains at the queue head", %{
    baseline: baseline,
    config: config
  } do
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)
    set_status(first.id, "ready_for_integration")
    set_status(second.id, "ready_for_integration")

    File.write!(Path.join(baseline.repo, "best-advance.txt"), "advance\n")
    Git.run!(baseline.repo, ["add", "best-advance.txt"])
    Git.run!(baseline.repo, ["commit", "-m", "advance best"])
    new_best = Git.run!(baseline.repo, ["rev-parse", "HEAD"])
    Git.run!(baseline.repo, ["branch", "-f", "pika/best", new_best])
    advance_best(new_best)

    assert {:refresh, refreshed} = Scheduler.next_queue_action()
    assert refreshed.id == first.id
    assert refreshed.status == "refreshing_iteration"
    assert refreshed.current_iteration_round == 2
    assert refreshed.base_sha == new_best

    round_paths = Workspace.round_paths(config.workspace, first.id, 2)
    initial_paths = Workspace.paths(config.workspace, first.id)
    assert round_paths.root != initial_paths.root
    assert File.dir?(round_paths.repo)
    assert Git.run!(round_paths.repo, ["rev-parse", "HEAD"]) == first.base_sha

    assert [[relative_root, branch]] =
             Repo.query!(
               "SELECT work_relative_path, branch FROM iteration_rounds WHERE attempt_id = ? AND round = 2",
               [first.id]
             ).rows

    assert relative_root == round_paths.relative_root
    assert branch == round_paths.branch

    assert [%{attempt: projected}] = Scheduler.project_work()
    assert projected.id == first.id
    assert {:waiting, waiting} = Scheduler.next_queue_action()
    assert waiting.id == first.id

    assert {:ok, prompt} =
             PromptInput.render(
               first.id,
               config.workspace,
               config.repo,
               config.iteration.history_limit
             )

    assert prompt =~ "## stale refresh"
    assert prompt =~ "Best Commit 已更新为 `#{new_best}`"

    session_id = Ecto.UUID.generate()

    assert {:ok, bundle} =
             ContextBundle.build(config, session_id, "iteration", :attempt, to_string(first.id))

    work = %Work{
      role_id: "iteration",
      kind: :attempt,
      id: to_string(first.id),
      payload: %{attempt: refreshed}
    }

    assert {:ok, agent_paths} = Pika.Agent.Workspace.resolve(config, work)
    assert agent_paths.work_root == round_paths.root
    assert agent_paths.cwd == round_paths.repo
    assert bundle.context.work_root == round_paths.root
    assert bundle.context.round_workspace_root == round_paths.root

    assert {:ok, rendered} = PromptBuilder.build(config, work, bundle)
    assert {:start_turn, initial_user_prompt} = rendered.activation
    assert initial_user_prompt =~ "旧 Base #{baseline.development_sha} 已 stale"
    assert initial_user_prompt =~ "当前 Best 是 #{new_best}"
    assert initial_user_prompt =~ "git merge #{new_best}"
    assert initial_user_prompt =~ "独立的 Round workspace"
    assert initial_user_prompt =~ "iteration-result.json"
  end

  test "Iteration Prompt contains the configured recent terminal Attempt history", %{
    config: config
  } do
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)

    Repo.query!(
      "UPDATE attempts SET status = 'rejected', summary = 'shared memory attempt failed' WHERE id = ?",
      [first.id]
    )

    assert {:ok, prompt} =
             PromptInput.render(
               second.id,
               config.workspace,
               config.repo,
               config.iteration.history_limit
             )

    assert prompt =~ "最近1次尝试包含"
    assert prompt =~ "| Attempt 1 | shared memory attempt failed | 拒绝 |"
    assert prompt =~ "--case-id 0,1"
  end

  test "Iteration Prompt skips legacy failed Attempts without journals", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [first, second]} = Scheduler.spawn_available(config)

    Repo.query!(
      "UPDATE attempts SET status = 'rejected', summary = 'workspace setup failed' WHERE id = ?",
      [first.id]
    )

    first_root = Workspace.paths(workspace, first.id).root
    File.rm!(Path.join(first_root, "message.jsonl"))
    File.rm!(Path.join(first_root, "summary.jsonl"))

    assert {:ok, prompt} =
             PromptInput.render(
               second.id,
               config.workspace,
               config.repo,
               config.iteration.history_limit
             )

    refute prompt =~ "workspace setup failed"
    assert prompt =~ "最近没有历史尝试。"
  end

  defp set_status(id, status) do
    if status == "ready_for_integration" do
      Repo.query!("UPDATE attempts SET status = ?, candidate_sha = base_sha WHERE id = ?", [
        status,
        id
      ])
    else
      Repo.query!("UPDATE attempts SET status = ? WHERE id = ?", [status, id])
    end
  end

  defp attempt_guidance(id) do
    [[guidance_id]] =
      Repo.query!("SELECT guidance_revision_id FROM attempts WHERE id = ?", [id]).rows

    guidance_id
  end

  defp advance_best(sha) do
    now = System.system_time(:microsecond)

    [[baseline_revision_id]] =
      Repo.query!("SELECT id FROM baseline_revisions WHERE revision = 0").rows

    Repo.query!(
      """
      INSERT INTO best_revisions(
        optimization_id, sequence, sha, source_kind, source_attempt_id,
        baseline_revision_id, summary, created_at
      ) VALUES ('optimization', 1, ?, 'attempt', 999, ?, 'advanced', ?)
      """,
      [sha, baseline_revision_id, now]
    )

    Repo.query!("UPDATE optimizations SET best_sha = ? WHERE id = 'optimization'", [sha])
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
        history_limit: 10
        max_pending_attempts: 0
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write
          - backend: cursor
            approval_policy: force
            sandbox: disabled
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
