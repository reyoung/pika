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

  test "persists and restores normalized Alignment through Baseline state", context do
    pid = start_campaign(context)
    harness_args = AlignmentFixtures.create_harness(context.stage_workspace.setup_worktree)

    upload = Path.join(context.workspace.root, "shape-input.jsonl")
    File.write!(upload, "{\"n\":1024}\n")

    assert {:ok, input_artifact} =
             ArtifactStore.copy_upload(context.workspace.root, upload, "shape.jsonl")

    assert :ok = Campaign.send_message("使用这个生产 Shape", [input_artifact])
    assert {:ok, composed} = Store.load(context.campaign.id)

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

    assert {:ok, %{digest: digest}} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "harness-1")
             )

    assert Campaign.snapshot().status == :awaiting_confirmation
    assert is_binary(digest)

    GenServer.stop(pid)
    assert {:ok, durable} = Store.load(context.campaign.id)
    assert durable.status == :awaiting_confirmation
    assert durable.harness.digest == digest
    assert {:ok, [frozen]} = Registry.references([], durable)
    assert frozen.sha == String.duplicate("a", 40)

    pid = start_campaign(context, durable)
    assert Campaign.snapshot().status == :awaiting_confirmation
    assert Campaign.snapshot().references |> hd() |> Map.fetch!(:sha) == frozen.sha

    assert :ok = Campaign.confirm_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    {setup_sha, best_sha} = merge_setup(context.stage_workspace)

    assert {:ok, %{best_sha: ^best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "merge-1",
               "base_sha" => context.stage_workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha
             })

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(
        context.stage_workspace,
        best_sha,
        context.skill.sha
      )

    Enum.each(
      [samples, correctness, profiler] ++ AlignmentFixtures.baseline_dependency_paths(),
      &register_artifact(&1, context.stage_workspace)
    )

    assert {:ok, %{status: "selecting_iteration_sample"}} =
             Campaign.mcp_call(@token, "submit_baseline", %{
               "idempotency_key" => "baseline-1",
               "measured_sha" => best_sha,
               "samples_artifact" => samples,
               "correctness_artifact" => correctness,
               "profiler_artifact" => profiler,
               "summary" => "persistent fixture baseline"
             })

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
             "spec_revisions" => 1
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

  defp start_campaign(context, durable \\ nil) do
    {:ok, pid} =
      Campaign.start_link(
        workspace: context.stage_workspace,
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
    Git.run!(workspace.setup_worktree, ["add", "."])
    Git.run!(workspace.setup_worktree, ["commit", "-m", "alignment setup"])
    setup_sha = Git.run!(workspace.setup_worktree, ["rev-parse", "HEAD"])
    Git.run!(workspace.repo, ["merge", "--squash", setup_sha])
    Git.run!(workspace.repo, ["commit", "-m", "Alignment setup"])
    {setup_sha, Git.run!(workspace.repo, ["rev-parse", "HEAD"])}
  end

  defp register_artifact(relative, workspace) do
    {:ok, artifact} = ArtifactStore.register(workspace.root, relative)

    assert {:ok, _} =
             Campaign.mcp_call(@token, "register_artifact", %{
               "idempotency_key" => "artifact-#{relative}",
               "kind" => artifact.kind,
               "relative_path" => relative,
               "sha256" => artifact.sha256,
               "size" => artifact.size,
               "mime" => artifact.mime,
               "metadata" => %{}
             })
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
      ) VALUES (?, ?, ?, ?, 9.0, 9.0, 0.0, 0.001, 0.005, 30, 30, 'baseline', ?)
      """,
      [best_id, case_id, metric_id, sha, now]
    )
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
