defmodule Pika.Test.OptimizationFixtures do
  alias Pika.{Git, Harness, TargetSnapshot}
  alias Pika.Alignment.ArtifactStore
  alias Pika.Test.{AlignmentFixtures, CampaignFixtures}
  alias Pika.{Config, Persistence, Repo, Workspace}

  def setup_campaign(opts \\ []) do
    max_attempts = Keyword.get(opts, :max_attempts, 3)
    plan_enabled = Keyword.get(opts, :plan_enabled, false)
    pair_count = Keyword.get(opts, :pair_count, AlignmentFixtures.pair_count())
    min_valid_pairs = Keyword.get(opts, :min_valid_pairs, AlignmentFixtures.min_valid_pairs())
    root = CampaignFixtures.workspace()
    config_path = CampaignFixtures.config_file(config(max_attempts, plan_enabled))
    {:ok, config} = Config.load(config_path, workspace: root)
    {:ok, plan} = Workspace.plan(config)
    {:ok, workspace} = Workspace.activate(plan)

    Git.run!(workspace.repo, ["config", "user.name", "Pika Test"])
    Git.run!(workspace.repo, ["config", "user.email", "pika@example.invalid"])
    AlignmentFixtures.create_harness(workspace.repo)
    Git.run!(workspace.repo, ["add", "."])
    Git.run!(workspace.repo, ["commit", "-m", "Baseline harness"])
    best_sha = Git.run!(workspace.repo, ["rev-parse", "HEAD"])
    workspace = %{workspace | base_sha: best_sha}

    spec =
      AlignmentFixtures.spec()
      |> put_in(["benchmark", "pair_count"], pair_count)
      |> put_in(["benchmark", "min_valid_pairs"], min_valid_pairs)

    {:ok, target_snapshot} =
      TargetSnapshot.prepare(
        workspace.root,
        1,
        workspace.repo,
        best_sha,
        spec,
        []
      )

    :ok = TargetSnapshot.link(workspace.root, workspace.repo, target_snapshot)

    Application.put_env(:pika, Repo,
      database: workspace.database,
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    {:ok, repo_pid} = Repo.start_link()
    Process.unlink(repo_pid)
    :ok = Persistence.migrate()
    {:ok, campaign, :initialized} = Persistence.initialize_or_recover(workspace)
    {:ok, harness} = Harness.validate(workspace.repo, harness_args())

    ids = insert_alignment_state(campaign.id, best_sha, harness, spec, target_snapshot)
    campaign = Persistence.current_campaign()

    %{
      workspace: workspace,
      campaign: campaign,
      best_sha: best_sha,
      harness: harness,
      target_snapshot: target_snapshot,
      spec: spec,
      spec_id: ids.spec_id,
      case_id: ids.case_id,
      metric_id: ids.metric_id,
      sampling_id: ids.sampling_id
    }
  end

  def stop_repo do
    if pid = Process.whereis(Repo) do
      try do
        GenServer.stop(pid)
      catch
        :exit, _reason -> :ok
      end
    end
  end

  def write_iteration_artifacts(
        workspace_root,
        attempt_id,
        _base_sha,
        candidate_sha,
        opts \\ []
      ) do
    improvement = Keyword.get(opts, :improvement, 0.02)
    pair_count = Keyword.get(opts, :pair_count, AlignmentFixtures.pair_count())
    valid_count = Keyword.get(opts, :valid_count, pair_count)
    samples_relative = "artifacts/logs/#{attempt_id}/pairs.jsonl"
    correctness_relative = "artifacts/logs/#{attempt_id}/correctness.json"
    samples_path = Path.join(workspace_root, samples_relative)
    correctness_path = Path.join(workspace_root, correctness_relative)
    File.mkdir_p!(Path.dirname(samples_path))

    records =
      for index <- 0..(pair_count - 1) do
        target = 10.0 + index / 10_000

        %{
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
          "target" => target,
          "candidate" => target * (1.0 - improvement),
          "valid" => index < valid_count,
          "error" => if(index < valid_count, do: nil, else: "fixture invalid")
        }
      end

    File.write!(samples_path, Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n")

    File.write!(
      correctness_path,
      Jason.encode!(%{
        "schema_version" => 2,
        "target_snapshot_id" => active_target_snapshot_id(),
        "candidate_sha" => candidate_sha,
        "cases" => [
          %{
            "case_id" => "target_case",
            "target_passed" => true,
            "candidate_passed" => true
          }
        ]
      })
    )

    {:ok, samples} = ArtifactStore.register(workspace_root, samples_relative)
    {:ok, correctness} = ArtifactStore.register(workspace_root, correctness_relative)
    {samples, correctness}
  end

  def add_guard_case(context) do
    case_id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO benchmark_cases(id, spec_revision_id, ordinal, name, kind, shape_json, dtype_json, layout_json, frequency_weight) VALUES (?, ?, 2, 'guard_case', 'guard', '{\"n\":2048}', '\"float16\"', '\"contiguous\"', 0.5)",
      [case_id, context.spec_id]
    )

    [[best_revision_id]] =
      Repo.query!(
        "SELECT id FROM best_revisions WHERE campaign_id = ? ORDER BY sequence DESC LIMIT 1",
        [context.campaign.id]
      ).rows

    pair_count = get_in(context.spec, ["benchmark", "pair_count"])

    Repo.query!(
      "INSERT INTO best_metrics(best_revision_id, benchmark_case_id, metric_definition_id, measured_sha, value, baseline_value, improvement_ratio, mad, noise_tolerance, pair_count, valid_pair_count, source, measured_at, target_snapshot_id, target_value, target_relative_improvement, best_relative_improvement) VALUES (?, ?, ?, ?, 10.0, 10.0, 0.0, 0.001, 0.005, ?, ?, 'baseline', ?, ?, 10.0, 0.0, 0.0)",
      [
        best_revision_id,
        case_id,
        context.metric_id,
        context.best_sha,
        pair_count,
        pair_count,
        now,
        context.target_snapshot.id
      ]
    )

    Map.put(context, :guard_case_id, case_id)
  end

  def config(max_attempts, plan_enabled) do
    """
    server:
      host: 127.0.0.1
      port: 18080
    backend:
      type: codex_app_server
      command: [codex, app-server, --listen, stdio://]
      protocol_config: {}
    campaign:
      plan: #{plan_enabled}
      max_attempts: #{max_attempts}
      history_n: 10
      iteration_agents:
        - name: slot-1
          backend: codex_app_server
          reasoning_effort: high
        - name: slot-2
          backend: codex_app_server
          reasoning_effort: high
        - name: slot-3
          backend: codex_app_server
          reasoning_effort: high
      reference_catalog: []
      stop_conditions:
        mode: all_goals
    """
  end

  defp insert_alignment_state(campaign_id, best_sha, harness, spec, target_snapshot) do
    now = System.system_time(:microsecond)
    spec_id = Ecto.UUID.generate()
    case_id = Ecto.UUID.generate()
    metric_id = Ecto.UUID.generate()
    sampling_id = Ecto.UUID.generate()
    best_id = Ecto.UUID.generate()

    Repo.query!(
      """
      INSERT INTO spec_revisions(
        id, campaign_id, revision, status, spec_json, protected_paths_json,
        protected_digest, baseline_sha, reference_snapshot_json, skill_snapshot_json,
        confirmed_at, inserted_at, updated_at
      ) VALUES (?, ?, 1, 'confirmed', ?, ?, ?, ?, '[]', ?, ?, ?, ?)
      """,
      [
        spec_id,
        campaign_id,
        Jason.encode!(spec),
        Jason.encode!(harness.protected_paths),
        harness.digest,
        best_sha,
        Jason.encode!(%{
          name: "ncu-report-skill",
          url: "https://example.invalid/ncu-report-skill.git",
          branch: "main",
          sha: String.duplicate("c", 40)
        }),
        now,
        now,
        now
      ]
    )

    Repo.query!(
      """
      INSERT INTO target_snapshots(
        id, campaign_id, spec_revision_id, source_kind, source_reference_id,
        source_sha, tree_sha, entrypoint, digest, checkout_relative_path, inserted_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      """,
      [
        target_snapshot.id,
        campaign_id,
        spec_id,
        target_snapshot.source_kind,
        target_snapshot.source_reference_id,
        target_snapshot.source_sha,
        target_snapshot.tree_sha,
        target_snapshot.entrypoint,
        target_snapshot.digest,
        target_snapshot.checkout_relative_path,
        now
      ]
    )

    Repo.query!(
      """
      UPDATE spec_revisions
      SET target_snapshot_id = ?, development_baseline_sha = ?, implementation_manifest_json = ?
      WHERE id = ?
      """,
      [
        target_snapshot.id,
        best_sha,
        Jason.encode!(spec["implementations"]),
        spec_id
      ]
    )

    Repo.query!(
      """
      INSERT INTO benchmark_cases(
        id, spec_revision_id, ordinal, name, kind, shape_json, dtype_json,
        layout_json, frequency_weight
      ) VALUES (?, ?, 1, 'target_case', 'target', '{"n":1024}', '"float16"', '"contiguous"', 1.0)
      """,
      [case_id, spec_id]
    )

    Repo.query!(
      """
      INSERT INTO metric_definitions(
        id, spec_revision_id, name, unit, direction, role,
        min_improvement_ratio, parser_json
      ) VALUES (?, ?, 'latency_us', 'us', 'minimize', 'target', 0.01, '{}')
      """,
      [metric_id, spec_id]
    )

    Repo.query!(
      """
      INSERT INTO sampling_revisions(
        id, campaign_id, spec_revision_id, sequence, cause, summary,
        estimated_cost_json, created_at
      ) VALUES (?, ?, ?, 1, 'baseline', 'initial sample', '{}', ?)
      """,
      [sampling_id, campaign_id, spec_id, now]
    )

    Repo.query!(
      "INSERT INTO sampling_revision_cases(sampling_revision_id, benchmark_case_id, reason) VALUES (?, ?, 'target representative')",
      [sampling_id, case_id]
    )

    Repo.query!(
      """
      INSERT INTO best_revisions(
        id, campaign_id, sequence, sha, cause, spec_revision_id, summary, inserted_at
      ) VALUES (?, ?, 1, ?, 'baseline', ?, 'fixture baseline', ?)
      """,
      [best_id, campaign_id, best_sha, spec_id, now]
    )

    Repo.query!(
      """
      INSERT INTO best_metrics(
        best_revision_id, benchmark_case_id, metric_definition_id, measured_sha,
        value, baseline_value, improvement_ratio, mad, noise_tolerance,
        pair_count, valid_pair_count, source, measured_at, target_snapshot_id,
        target_value, target_relative_improvement, best_relative_improvement
      ) VALUES (?, ?, ?, ?, 10.0, 10.0, 0.0, 0.001, 0.005, ?, ?, 'baseline', ?, ?, 10.0, 0.0, 0.0)
      """,
      [
        best_id,
        case_id,
        metric_id,
        best_sha,
        spec["benchmark"]["pair_count"],
        spec["benchmark"]["pair_count"],
        now,
        target_snapshot.id
      ]
    )

    Repo.query!(
      "UPDATE campaigns SET status = 'optimizing', best_sha = ?, current_spec_revision_id = ?, updated_at = ? WHERE id = ?",
      [best_sha, spec_id, now, campaign_id]
    )

    %{spec_id: spec_id, case_id: case_id, metric_id: metric_id, sampling_id: sampling_id}
  end

  defp harness_args do
    %{
      "oracle_path" => nil,
      "correctness_paths" => ["kernel/test_correctness.py"],
      "benchmark_path" => "kernel/bench.py",
      "protected_paths" => [
        "kernel/test_correctness.py",
        "kernel/bench.py"
      ]
    }
  end

  defp active_target_snapshot_id do
    case Repo.query!("""
         SELECT sr.target_snapshot_id
         FROM campaigns c
         JOIN spec_revisions sr ON sr.id = c.current_spec_revision_id
         ORDER BY c.updated_at DESC
         LIMIT 1
         """).rows do
      [[id]] when is_binary(id) -> id
      _ -> raise "active Campaign has no Optimization Target snapshot"
    end
  end
end
