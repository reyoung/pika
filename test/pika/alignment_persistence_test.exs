defmodule Pika.AlignmentPersistenceTest do
  use ExUnit.Case, async: false

  alias Pika.Metrics
  alias Pika.CampaignStore, as: Store
  alias Pika.ReferenceRegistry, as: Registry
  alias Pika.Git
  alias Pika.Alignment.{ArtifactStore, Campaign}
  alias Pika.Test.{AlignmentFixtures, CampaignFixtures}
  alias Pika.{Config, Persistence, Repo, Workspace}

  @token "alignment-persistence-token"

  setup do
    stop_campaign()
    root = CampaignFixtures.workspace()
    config_path = CampaignFixtures.config_file()
    {:ok, config} = Config.load(config_path, workspace: root)
    {:ok, plan} = Workspace.plan(config)
    {:ok, workspace} = Workspace.activate(plan)

    Application.put_env(:pika, Repo,
      database: workspace.database,
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()
    assert {:ok, campaign, :initialized} = Persistence.initialize_or_recover(workspace)
    assert {:ok, stage_workspace} = Pika.CampaignWorkspace.prepare(workspace, campaign)

    skill_dir = Path.join(root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)
    File.write!(Path.join(skill_dir, "SKILL.md"), "---\nname: ncu-report-skill\n---\n")

    skill = %{
      name: "ncu-report-skill",
      url: "https://example.invalid/ncu-report-skill.git",
      sha: String.duplicate("c", 40),
      path: skill_dir
    }

    references = [
      %{
        id: "cutlass",
        url: "https://github.com/NVIDIA/cutlass.git",
        description: "NVIDIA CUTLASS",
        selected: true,
        status: :resolved,
        branch: "main",
        sha: String.duplicate("a", 40)
      }
    ]

    on_exit(&stop_campaign/0)

    %{
      workspace: workspace,
      stage_workspace: stage_workspace,
      campaign: campaign,
      skill: skill,
      references: references
    }
  end

  test "persists and restores user-added Campaign Reference Projects", context do
    pid = start_campaign(context)

    assert {:ok, added} =
             Campaign.add_reference_project(%{
               "url" => "https://github.com/example/campaign-kernels.git",
               "description" => "Campaign-owned reference"
             })

    assert added.origin == :user
    assert {:ok, durable} = Store.load(context.campaign.id)
    assert {:ok, restored_references} = Registry.references([], durable)
    assert List.last(restored_references) == added

    GenServer.stop(pid)

    restarted_context = %{context | references: restored_references}
    _pid = start_campaign(restarted_context, durable)

    assert List.last(Campaign.snapshot().references) == added
    assert "campaign-kernels" in Campaign.snapshot().spec["reference_ids"]
  end

  test "persists and restores normalized Alignment through Baseline state", context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    upload = Path.join(context.workspace.root, "shape-input.jsonl")
    File.write!(upload, "{\"n\":1024}\n")

    assert {:ok, input_artifact} =
             ArtifactStore.copy_upload(context.workspace.root, upload, "shape.jsonl")

    assert :ok = Campaign.send_message("使用这个生产 Shape", [input_artifact])
    assert {:ok, composed} = Store.load(context.campaign.id)

    assert [["blob"]] =
             Repo.query!(
               "SELECT typeof(state_blob) FROM campaign_runtime_snapshots WHERE campaign_id = ?",
               [context.campaign.id]
             ).rows

    assert %{role: :user, content: "使用这个生产 Shape", attachments: [^input_artifact]} =
             List.last(composed.messages)

    assert [[1]] =
             Repo.query!(
               "SELECT COUNT(*) FROM artifacts WHERE campaign_id = ? AND kind = 'input'",
               [
                 context.campaign.id
               ]
             ).rows

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "spec-1",
               "spec" => AlignmentFixtures.spec()
             })

    [[case_id_before]] =
      Repo.query!(
        "SELECT id FROM benchmark_cases WHERE spec_revision_id = (SELECT current_spec_revision_id FROM campaigns WHERE id = ?)",
        [context.campaign.id]
      ).rows

    assert {:ok, %{digest: digest}} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "harness-1")
             )

    [[case_id_after]] =
      Repo.query!(
        "SELECT id FROM benchmark_cases WHERE spec_revision_id = (SELECT current_spec_revision_id FROM campaigns WHERE id = ?)",
        [context.campaign.id]
      ).rows

    assert case_id_after == case_id_before

    assert {:ok, _} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               %{root: context.workspace.root},
               "persisted-review"
             )

    assert Campaign.snapshot().status == :awaiting_confirmation
    assert is_binary(digest)

    GenServer.stop(pid)
    assert {:ok, durable} = Store.load(context.campaign.id)
    assert durable.status == :awaiting_confirmation
    assert durable.harness.digest == digest
    assert {:ok, [frozen]} = Registry.references([], durable)
    assert frozen.sha == String.duplicate("a", 40)

    target_link = Path.join(context.stage_workspace.setup_worktree, "target")
    assert File.lstat!(target_link).type == :symlink
    File.rm!(target_link)
    assert {:error, :enoent} = File.lstat(target_link)

    pid = start_campaign(context, durable)
    assert Campaign.snapshot().status == :awaiting_confirmation
    assert Campaign.snapshot().references |> hd() |> Map.fetch!(:sha) == frozen.sha
    assert File.lstat!(target_link).type == :symlink

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    {setup_sha, best_sha} = merge_setup(context.stage_workspace)

    assert {:ok, %{best_sha: ^best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "merge-1",
               "base_sha" => context.stage_workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    AlignmentFixtures.write_baseline_artifacts(
      context.stage_workspace,
      best_sha,
      context.skill.sha
    )

    manifest =
      AlignmentFixtures.write_baseline_manifest(
        context.stage_workspace,
        best_sha,
        "persistent fixture baseline"
      )

    assert {:ok, %{status: "validating_baseline"}} =
             Campaign.mcp_call(@token, "submit_baseline", %{
               "idempotency_key" => "baseline-1",
               "manifest_artifact" => manifest
             })

    assert eventually(fn -> Campaign.snapshot().status == :selecting_iteration_sample end)

    assert Repo.query!("SELECT status FROM campaigns WHERE id = ?", [context.campaign.id]).rows ==
             [
               ["selecting_iteration_sample"]
             ]

    assert {:ok, %{status: "optimizing"}} =
             Campaign.mcp_call(@token, "submit_iteration_sample", %{
               "idempotency_key" => "sample-1",
               "case_ids" => ["target_case"],
               "reasons" => %{"target_case" => "representative target"},
               "estimated_cost" => %{
                 "iteration_seconds" => 1.0,
                 "full_seconds" => 2.0,
                 "savings_ratio" => 0.5
               },
               "summary" => "initial persistent sample"
             })

    assert Store.counts(context.campaign.id) == %{
             "benchmark_cases" => 1,
             "best_metrics" => 1,
             "best_revisions" => 1,
             "metric_definitions" => 1,
             "sampling_revision_cases" => 1,
             "sampling_revisions" => 1,
             "spec_revisions" => 1,
             "target_snapshots" => 1
           }

    assert [["cutlass", "main", ref_sha, skill_sha, ^digest]] =
             Repo.query!(
               """
               SELECT
                 json_extract(reference_snapshot_json, '$[0].id'),
                 json_extract(reference_snapshot_json, '$[0].branch'),
                 json_extract(reference_snapshot_json, '$[0].sha'),
                 json_extract(skill_snapshot_json, '$.sha'),
                 protected_digest
               FROM spec_revisions WHERE campaign_id = ? AND revision = 1
               """,
               [context.campaign.id]
             ).rows

    assert ref_sha == String.duplicate("a", 40)
    assert skill_sha == context.skill.sha

    profiler_rows =
      Repo.query!(
        "SELECT relative_path, sha256 FROM artifacts WHERE campaign_id = ? AND relative_path LIKE 'artifacts/profiles/%'",
        [context.campaign.id]
      ).rows

    assert length(profiler_rows) >= 3

    assert Enum.all?(profiler_rows, fn [path, sha] ->
             Path.type(path) == :relative and String.starts_with?(path, "artifacts/profiles/") and
               byte_size(sha) == 64
           end)

    GenServer.stop(pid)
    assert {:ok, final_durable} = Store.load(context.campaign.id)
    _pid = start_campaign(context, final_durable)
    snapshot = Campaign.snapshot()
    assert snapshot.status == :optimizing
    assert snapshot.best_sha == best_sha
    assert snapshot.baseline.summary == "persistent fixture baseline"
    assert snapshot.iteration_sampling.case_ids == ["target_case"]

    insert_second_revision(context.campaign.id)
    series = Metrics.baseline_series(context.campaign.id)
    assert Enum.map(series, & &1.spec_revision) == [1, 2]
    assert Enum.map(series, & &1.spec_revision_id) |> Enum.uniq() |> length() == 2
    assert Enum.all?(series, &(length(&1.points) == 1))
  end

  test "restores the provider session without replaying the original kickoff", context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "resume-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "resume-harness")
             )

    assert {:ok, _} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               %{root: context.workspace.root},
               "resume-review"
             )

    assert Campaign.snapshot().status == :awaiting_confirmation
    GenServer.stop(pid)

    {:ok, durable} = Store.load(context.campaign.id)
    durable = Map.put(durable, :provider_session_id, "persisted-provider-session")

    {:ok, _pid} =
      Campaign.start_link(
        workspace: context.stage_workspace,
        campaign_id: context.campaign.id,
        persistence: Store,
        durable_state: durable,
        backend: :codex_app_server,
        backend_module: Pika.Test.AlignmentAgentBackend,
        start_backend: true,
        resolve_references: false,
        references: context.references,
        mcp_url: "http://127.0.0.1:1/mcp",
        skill: context.skill
      )

    assert eventually(fn ->
             Enum.any?(Campaign.snapshot().messages, fn message ->
               message.role == :system and String.contains?(message.content, "Session 已恢复")
             end)
           end)

    snapshot = Campaign.snapshot()
    assert snapshot.status == :awaiting_confirmation
    refute snapshot.agent_responding
  end

  test "returns a legacy AwaitingConfirmation snapshot to Agent evidence collection", context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "legacy-review-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "legacy-review-harness")
             )

    assert {:ok, durable} = Store.load(context.campaign.id)
    GenServer.stop(pid)

    legacy =
      durable
      |> Map.delete(:implementation_review_evidence)
      |> Map.put(:status, :awaiting_confirmation)
      |> Map.put(:required, MapSet.new())

    _pid = start_campaign(context, legacy)
    snapshot = Campaign.snapshot()
    assert snapshot.status == :drafting_spec

    assert snapshot.required_operations == [
             "submit_implementation_bundle",
             "submit_implementation_review"
           ]

    assert snapshot.last_error =~ "Optimization Target 或 Development 提交身份缺失"
  end

  test "opens an ambiguous Campaign Spec v1 as an explicit v2 draft without inferring roles",
       context do
    pid = start_campaign(context)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "legacy-v1-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, durable} = Store.load(context.campaign.id)
    GenServer.stop(pid)

    legacy_spec =
      durable.spec_result.spec
      |> Map.put("schema_version", 1)
      |> Map.delete("implementations")
      |> put_in(["computation", "reference_path"], "kernel/reference.py")

    legacy = %{
      durable
      | status: :optimizing,
        spec_result: %{durable.spec_result | spec: legacy_spec, ready?: true},
        required: MapSet.new()
    }

    assert {:ok, revision_workspace} =
             Pika.CampaignWorkspace.prepare(context.workspace, context.campaign, 2)

    _pid = start_campaign(context, legacy, revision_workspace)
    snapshot = Campaign.snapshot()

    assert snapshot.status == :drafting_spec
    assert snapshot.spec["schema_version"] == 2
    assert snapshot.spec["revision"] == 2
    refute Map.has_key?(snapshot.spec, "implementations")
    refute snapshot.spec_ready

    assert snapshot.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    assert snapshot.last_error =~ "ambiguous v1 Reference model"
  end

  test "revalidates legacy generic Metric errors when restoring", context do
    pid = start_campaign(context)

    invalid_spec =
      AlignmentFixtures.spec()
      |> put_in(["metrics", Access.at(0), "direction"], "sideways")

    assert {:ok, %{ready: false}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "invalid-metric-spec",
               "spec" => invalid_spec
             })

    assert {:ok, durable} = Store.load(context.campaign.id)
    GenServer.stop(pid)

    legacy = put_in(durable, [:spec_result, :errors], ["Metrics have invalid fields"])
    _pid = start_campaign(context, legacy)
    snapshot = Campaign.snapshot()

    assert "metrics[0].direction: must be one of minimize, maximize" in snapshot.spec_errors
    refute "Metrics have invalid fields" in snapshot.spec_errors
  end

  test "requires reconfirmation when legacy state changed the Harness after confirmation",
       context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "legacy-confirmed-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "legacy-confirmed-harness")
             )

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    assert {:ok, durable} = Store.load(context.campaign.id)
    GenServer.stop(pid)

    legacy = %{durable | status: :drafting_spec}
    _pid = start_campaign(context, legacy)
    snapshot = Campaign.snapshot()

    assert snapshot.status == :awaiting_confirmation
    assert snapshot.required_operations == []
    assert snapshot.last_error =~ "请重新确认"
  end

  test "persists a post-merge Baseline rollback as the next Spec revision", context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "rollback-spec-1",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "rollback-harness-1")
             )

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    {setup_sha, best_sha} = merge_setup(context.stage_workspace)

    assert {:ok, _} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "rollback-merge-1",
               "base_sha" => context.stage_workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    assert {:ok, %{status: "drafting_spec", revision: 2}} =
             Campaign.mcp_call(@token, "reopen_baseline_definition", %{
               "idempotency_key" => "rollback-definition-1",
               "reason" => "冻结的 Reference 无法生成有效 Baseline",
               "requested_changes" => "改用正确的 Reference 并重建 Harness"
             })

    assert Campaign.snapshot().spec["revision"] == 2

    assert [[1, "confirmed"], [2, "draft"]] =
             Repo.query!(
               "SELECT revision, status FROM spec_revisions WHERE campaign_id = ? ORDER BY revision",
               [context.campaign.id]
             ).rows

    assert {:ok, durable} = Store.load(context.campaign.id)
    assert durable.spec_result.spec["revision"] == 2
    assert durable.setup_base_sha == best_sha
    GenServer.stop(pid)

    campaign = Persistence.current_campaign()

    assert {:ok, revision_workspace} =
             Pika.CampaignWorkspace.prepare(context.workspace, campaign, 2)

    _pid = start_campaign(context, durable, revision_workspace)
    restored = Campaign.snapshot()
    assert restored.status == :drafting_spec
    assert restored.spec["revision"] == 2

    assert restored.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    assert :sys.get_state(Campaign).workspace.setup_worktree ==
             Path.join(context.workspace.root, "setup/2")
  end

  test "materializes definitions only after a draft Spec becomes valid", context do
    _pid = start_campaign(context)

    invalid_spec =
      AlignmentFixtures.spec()
      |> update_in(["benchmark"], &Map.delete(&1, "pair_count"))

    assert {:ok, %{ready: false}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "incomplete-spec",
               "spec" => invalid_spec
             })

    assert [[0, 0]] = definition_counts(context.campaign.id)

    valid_spec =
      put_in(invalid_spec, ["benchmark", "pair_count"], AlignmentFixtures.pair_count())

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "completed-spec",
               "spec" => valid_spec
             })

    assert [[1, 1]] = definition_counts(context.campaign.id)
  end

  defp start_campaign(context, durable \\ nil, stage_workspace \\ nil) do
    stage_workspace = stage_workspace || context.stage_workspace

    {:ok, pid} =
      Campaign.start_link(
        workspace: stage_workspace,
        campaign_id: context.campaign.id,
        persistence: Store,
        durable_state: durable,
        backend: :codex_app_server,
        start_backend: false,
        resolve_references: false,
        references: context.references,
        mcp_url: "http://127.0.0.1:1/mcp",
        mcp_token: @token,
        skill: context.skill
      )

    pid
  end

  defp merge_setup(workspace) do
    setup_sha = Campaign.snapshot().prepared_setup_sha
    assert Git.run!(workspace.setup_worktree, ["rev-parse", "HEAD"]) == setup_sha
    assert Git.clean?(workspace.setup_worktree)
    Git.run!(workspace.repo, ["merge", "--squash", setup_sha])
    Git.run!(workspace.repo, ["commit", "-m", "Alignment setup"])
    {setup_sha, Git.run!(workspace.repo, ["rev-parse", "HEAD"])}
  end

  defp confirm_reviewed_spec do
    if is_nil(Campaign.snapshot().implementation_review_evidence) do
      root = Campaign.snapshot().workspace.root
      key = "persistence-review-#{System.unique_integer([:positive])}"
      {:ok, _} = AlignmentFixtures.submit_implementation_review(@token, %{root: root}, key)
    end

    {:ok, implementation_review} = Campaign.implementation_review()
    evidence = Campaign.snapshot().implementation_review_evidence

    Campaign.confirm_spec(
      implementation_review.target_snapshot.digest,
      evidence.development_sha,
      evidence.digest
    )
  end

  defp insert_second_revision(campaign_id) do
    now = System.system_time(:microsecond)
    spec_id = Ecto.UUID.generate()
    case_id = Ecto.UUID.generate()
    metric_id = Ecto.UUID.generate()
    best_id = Ecto.UUID.generate()
    sha = String.duplicate("b", 40)

    Repo.query!(
      """
      INSERT INTO spec_revisions(
        id, campaign_id, revision, status, spec_json, protected_paths_json,
        reference_snapshot_json, skill_snapshot_json, confirmed_at, inserted_at, updated_at
      ) VALUES (?, ?, 2, 'confirmed', '{}', '[]', '[]', '{}', ?, ?, ?)
      """,
      [spec_id, campaign_id, now, now, now]
    )

    Repo.query!(
      """
      INSERT INTO benchmark_cases(
        id, spec_revision_id, ordinal, name, kind, shape_json, dtype_json, layout_json
      ) VALUES (?, ?, 1, 'target_case', 'target', '{}', '\"float16\"', '\"contiguous\"')
      """,
      [case_id, spec_id]
    )

    Repo.query!(
      """
      INSERT INTO metric_definitions(
        id, spec_revision_id, name, unit, direction, role, min_improvement_ratio, parser_json
      ) VALUES (?, ?, 'latency_us', 'us', 'minimize', 'target', 0.01, '{}')
      """,
      [metric_id, spec_id]
    )

    Repo.query!(
      """
      INSERT INTO best_revisions(
        id, campaign_id, sequence, sha, cause, spec_revision_id, summary, inserted_at
      ) VALUES (?, ?, 2, ?, 'baseline', ?, 'revision 2 baseline', ?)
      """,
      [best_id, campaign_id, sha, spec_id, now]
    )

    Repo.query!(
      """
      INSERT INTO best_metrics(
        best_revision_id, benchmark_case_id, metric_definition_id, measured_sha,
        value, baseline_value, improvement_ratio, mad, noise_tolerance,
        pair_count, valid_pair_count, source, measured_at
      ) VALUES (?, ?, ?, ?, 9.0, 9.0, 0.0, 0.001, 0.005, 7, 7, 'baseline', ?)
      """,
      [best_id, case_id, metric_id, sha, now]
    )
  end

  defp definition_counts(campaign_id) do
    Repo.query!(
      """
      SELECT
        (SELECT COUNT(*) FROM benchmark_cases
         WHERE spec_revision_id = campaigns.current_spec_revision_id),
        (SELECT COUNT(*) FROM metric_definitions
         WHERE spec_revision_id = campaigns.current_spec_revision_id)
      FROM campaigns WHERE id = ?
      """,
      [campaign_id]
    ).rows
  end

  defp eventually(fun, attempts \\ 50)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.() do
      true
    else
      Process.sleep(20)
      eventually(fun, attempts - 1)
    end
  end

  defp eventually(_fun, 0), do: false

  defp stop_campaign do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
  end
end
