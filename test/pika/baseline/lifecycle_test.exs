Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Baseline.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ContextBundle, PromptBuilder, Work}
  alias Pika.Baseline.{Lifecycle, Workspace}
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.{Git, Repo}
  alias Pika.Test.V2BaselineFixtures

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-baseline-lifecycle-#{System.unique_integer([:positive])}"
      )

    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    work = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(work.repo, workspace))
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
             Persistence.initialize_or_recover(config, work.development_sha)

    on_exit(fn -> File.rm_rf!(root) end)
    %{config: config, root: root, work: work, workspace: workspace}
  end

  test "projects Alignment, submits a Definition, and waits for user Review", %{work: work} do
    assert {:ok, draft} = Lifecycle.ensure_draft(work.root)
    assert draft.revision == 0
    assert draft.status == "drafting"

    assert [%{role_id: "baseline_alignment", work_id: work_id}] = Lifecycle.project_work()
    assert work_id == to_string(draft.id)

    assert {:ok, submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert submitted.status == "awaiting_review"
    assert byte_size(submitted.definition_sha256) == 64
    assert byte_size(submitted.dependencies_sha256) == 64
    assert submitted.development_sha == work.development_sha
    assert Lifecycle.project_work() == []
    assert Persistence.current().status == "awaiting_baseline_review"

    assert [[2]] = Repo.query!("SELECT COUNT(*) FROM benchmark_cases").rows
    assert [[2]] = Repo.query!("SELECT COUNT(*) FROM metric_definitions").rows
    assert [[6]] = Repo.query!("SELECT COUNT(*) FROM artifacts").rows
  end

  test "Approve freezes a Target Snapshot and projects Baseline Verify", %{work: work} do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert {:ok, approved} =
             Lifecycle.review(0, :approve, work.root, "baseline-definition.json")

    assert approved.status == "verifying"
    assert is_binary(approved.target_snapshot_id)
    assert Persistence.current().status == "verifying_baseline"

    assert [%{role_id: "baseline_verify", work_id: work_id}] = Lifecycle.project_work()
    assert work_id == to_string(approved.id)

    assert [[1]] =
             Repo.query!("SELECT COUNT(*) FROM baseline_reviews WHERE decision = 'approved'").rows

    assert [[1]] = Repo.query!("SELECT COUNT(*) FROM target_snapshots").rows
  end

  test "Review rejects any file identity change after submission", %{work: work} do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    cases = V2BaselineFixtures.read_json(work.root, "cases.json")
    cases = put_in(cases, ["cases", Access.at(0), "description"], "changed after review")
    V2BaselineFixtures.write_json(work.root, "cases.json", cases)

    assert {:error, {:baseline_review_identity_changed, _identity}} =
             Lifecycle.review(0, :approve, work.root, "baseline-definition.json")
  end

  test "Request Changes supersedes the Revision and allows a new draft", %{
    work: work,
    workspace: workspace
  } do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert {:error, :review_feedback_required} =
             Lifecycle.review(0, :request_changes, work.root, "baseline-definition.json", "")

    assert {:ok, superseded} =
             Lifecycle.review(
               0,
               :request_changes,
               work.root,
               "baseline-definition.json",
               "补齐线上 shape"
             )

    assert superseded.status == "superseded"
    assert superseded.review_feedback == "补齐线上 shape"

    next_root = Path.join([workspace, "baseline", "revisions", "1"])
    File.mkdir_p!(next_root)
    assert {:ok, next} = Lifecycle.ensure_draft(next_root)
    assert next.revision == 1
    assert next.status == "drafting"
  end

  test "accepted Verification creates Best Revision 0 and Sampling Revision 0", %{work: work} do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert {:ok, approved} = Lifecycle.review(0, :approve, work.root, "baseline-definition.json")
    V2BaselineFixtures.write_verification_result(work, approved.id, :accepted)

    assert {:ok, accepted} =
             Lifecycle.finish_verification(
               0,
               work.root,
               "baseline-verification-result.json"
             )

    assert accepted.status == "accepted"
    assert Persistence.current().status == "optimizing"
    assert Persistence.current().best_sha == work.development_sha
    assert Git.run!(work.repo, ["rev-parse", "pika/best"]) == work.development_sha
    assert [[1]] = Repo.query!("SELECT COUNT(*) FROM best_revisions WHERE sequence = 0").rows
    assert [[4]] = Repo.query!("SELECT COUNT(*) FROM best_metrics").rows
    assert [[1]] = Repo.query!("SELECT COUNT(*) FROM sampling_revisions WHERE sequence = 0").rows
    assert [[2]] = Repo.query!("SELECT COUNT(*) FROM sampling_revision_cases").rows
    assert Lifecycle.project_work() == []
  end

  test "rejected Verification preserves its Result for the next Alignment", %{
    config: config,
    work: work,
    workspace: workspace
  } do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert {:ok, approved} = Lifecycle.review(0, :approve, work.root, "baseline-definition.json")
    V2BaselineFixtures.write_verification_result(work, approved.id, :rejected)

    assert {:ok, rejected} =
             Lifecycle.finish_verification(
               0,
               work.root,
               "baseline-verification-result.json"
             )

    assert rejected.status == "superseded"
    assert rejected.terminal_reason == "unstable measurements"
    assert Persistence.current().status == "aligning_baseline"

    assert [[1]] =
             Repo.query!(
               "SELECT COUNT(*) FROM artifacts WHERE kind = 'baseline_verification_result'"
             ).rows

    next_root = Path.join([workspace, "baseline", "revisions", "000001"])
    File.mkdir_p!(next_root)
    assert {:ok, next} = Lifecycle.ensure_draft(next_root)
    session_id = Ecto.UUID.generate()

    assert {:ok, bundle} =
             ContextBundle.build(
               config,
               session_id,
               "baseline_alignment",
               :baseline_revision,
               to_string(next.id)
             )

    next_work = %Work{
      role_id: "baseline_alignment",
      kind: :baseline_revision,
      id: to_string(next.id)
    }

    [[failure_relative_path]] =
      Repo.query!(
        "SELECT relative_path FROM artifacts WHERE kind = 'baseline_verification_result'"
      ).rows

    assert File.regular?(Path.join(workspace, failure_relative_path))
    assert File.regular?(bundle.context_file)

    assert {:ok, prompt} = PromptBuilder.build(config, next_work, bundle)
    assert prompt.system =~ "开始工作前必须读取以下信息"
    assert prompt.system =~ "## previous verification 0"
    assert prompt.system =~ "baseline-verification-result.json"
    assert prompt.system =~ "上一轮 Baseline Verify 的拒绝反馈如下"
    assert prompt.system =~ "unstable measurements"
    assert prompt.system =~ "fix benchmark synchronization"

    assert {:start_turn, activation} = prompt.activation
    assert activation =~ "上一轮 Baseline Verify 已拒绝 Definition"
    assert activation =~ "requested_changes 已注入 System Prompt"

    recovery_directory = Path.join(workspace, "empty-alignment-recovery")
    File.mkdir_p!(recovery_directory)
    messages_file = Path.join(recovery_directory, "messages.jsonl")
    state_file = Path.join(recovery_directory, "recovery.json")
    File.write!(messages_file, "")
    File.write!(state_file, "{}")

    recoveries = [
      %{
        directory: recovery_directory,
        messages_file: messages_file,
        state_file: state_file
      }
    ]

    assert {:ok, recovered_prompt} = PromptBuilder.build(config, next_work, bundle, recoveries)
    assert {:start_turn, recovered_activation} = recovered_prompt.activation
    assert recovered_activation =~ "上一轮 Baseline Verify 已拒绝 Definition"
  end

  test "accepted Verification rejects Agent metrics that do not match raw Pairs", %{work: work} do
    assert {:ok, _draft} = Lifecycle.ensure_draft(work.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, work.root, "baseline-definition.json")

    assert {:ok, approved} = Lifecycle.review(0, :approve, work.root, "baseline-definition.json")
    result = V2BaselineFixtures.write_verification_result(work, approved.id, :accepted)
    result = put_in(result, ["details", "case_metrics", Access.at(0), "target_value"], 999.0)
    V2BaselineFixtures.write_json(work.root, "baseline-verification-result.json", result)

    assert {:error, {:reported_case_metric_mismatch, 0, "latency_us"}} =
             Lifecycle.finish_verification(
               0,
               work.root,
               "baseline-verification-result.json"
             )

    assert Lifecycle.latest_revision().status == "verifying"
  end

  test "a reviewed descendant Development advances Best during Baseline realignment", %{
    config: config,
    work: work
  } do
    assert {:ok, first} = accept_baseline(work, 0)
    assert first.status == "accepted"

    now = System.system_time(:microsecond)

    Repo.query!(
      "UPDATE baseline_revisions SET status = 'superseded', terminal_reason = 'harness migration', updated_at = ? WHERE id = ?",
      [now, first.id]
    )

    Repo.query!(
      "UPDATE optimizations SET status = 'aligning_baseline', stop_reason = 'harness migration', updated_at = ? WHERE id = 'optimization'",
      [now]
    )

    assert {:ok, %{revision: draft, paths: paths}} = Workspace.ensure_current(config)
    assert draft.revision == 1

    File.write!(Path.join(paths.repo, "harness-migration.txt"), "runtime manifest support\n")
    Git.run!(paths.repo, ["add", "harness-migration.txt"])
    Git.run!(paths.repo, ["commit", "-m", "migrate baseline harness"])
    intermediate_sha = Git.run!(paths.repo, ["rev-parse", "HEAD"])
    Git.run!(work.repo, ["update-ref", "refs/heads/pika/best", intermediate_sha])

    File.write!(Path.join(paths.repo, "harness-review.txt"), "reviewed identity\n")
    Git.run!(paths.repo, ["add", "harness-review.txt"])
    Git.run!(paths.repo, ["commit", "-m", "record reviewed harness identity"])
    descendant_sha = Git.run!(paths.repo, ["rev-parse", "HEAD"])
    V2BaselineFixtures.write_definition_files(paths.root, descendant_sha)

    assert {:ok, accepted} =
             accept_baseline(%{root: paths.root, development_sha: descendant_sha}, 1)

    assert accepted.status == "accepted"
    assert Persistence.current().best_sha == descendant_sha
    assert Git.run!(work.repo, ["rev-parse", "pika/best"]) == descendant_sha

    assert [[1, ^descendant_sha, "baseline"]] =
             Repo.query!(
               "SELECT sequence, sha, source_kind FROM best_revisions ORDER BY sequence DESC LIMIT 1"
             ).rows
  end

  test "reconsiders complete evidence rejected only by the retired exact-Best gate", %{
    config: config,
    work: work
  } do
    assert {:ok, first} = accept_baseline(work, 0)
    old_best = work.development_sha
    now = System.system_time(:microsecond)

    Repo.query!(
      "UPDATE baseline_revisions SET status = 'superseded', terminal_reason = 'harness migration', updated_at = ? WHERE id = ?",
      [now, first.id]
    )

    Repo.query!(
      "UPDATE optimizations SET status = 'aligning_baseline', stop_reason = 'harness migration', updated_at = ? WHERE id = 'optimization'",
      [now]
    )

    assert {:ok, %{revision: draft, paths: paths}} = Workspace.ensure_current(config)
    assert draft.revision == 1
    File.write!(Path.join(paths.repo, "candidate-env.txt"), "manifest from environment\n")
    Git.run!(paths.repo, ["add", "candidate-env.txt"])
    Git.run!(paths.repo, ["commit", "-m", "support candidate manifest env"])
    candidate_sha = Git.run!(paths.repo, ["rev-parse", "HEAD"])
    V2BaselineFixtures.write_definition_files(paths.root, candidate_sha)

    assert {:ok, submitted} =
             Lifecycle.submit_definition(1, paths.root, "baseline-definition.json")

    assert {:ok, approved} =
             Lifecycle.review(1, :approve, paths.root, "baseline-definition.json")

    accepted =
      V2BaselineFixtures.write_verification_result(
        %{root: paths.root, development_sha: candidate_sha},
        approved.id,
        :accepted
      )

    accepted = put_in(accepted, ["details", "baseline_revision"], 1)
    V2BaselineFixtures.write_json(paths.root, "baseline-verification-result.json", accepted)
    verify_stat = File.stat!(Path.join(paths.root, "full-verify.json"))

    rejected = %{
      "schema_version" => 1,
      "role" => "baseline_verify",
      "work_id" => to_string(submitted.id),
      "outcome" => "definition_rejected",
      "summary" => "complete evidence blocked by old identity gate",
      "files" => %{},
      "details" => %{
        "baseline_revision" => 1,
        "definition_sha256" => submitted.definition_sha256,
        "development_sha" => candidate_sha,
        "failure_kind" => "definition",
        "reason" =>
          "Full evidence is valid, but accepted Best #{old_best} differs from Development #{candidate_sha}.",
        "requested_changes" => ["Align accepted Best with Development identity."]
      }
    }

    V2BaselineFixtures.write_json(paths.root, "baseline-verification-result.json", rejected)

    assert {:ok, %{status: "superseded"}} =
             Lifecycle.finish_verification(1, paths.root, "baseline-verification-result.json")

    assert {:ok, %{revision: successor, paths: successor_paths}} =
             Workspace.ensure_current(config)

    assert successor.revision == 2
    assert File.dir?(successor_paths.root)

    assert {:ok, accepted_revision} = Lifecycle.reconsider_verified_revision(1, paths.root)
    assert accepted_revision.status == "accepted"
    assert Persistence.current().status == "optimizing"
    assert Persistence.current().best_sha == candidate_sha
    assert Git.run!(work.repo, ["rev-parse", "pika/best"]) == candidate_sha
    assert File.stat!(Path.join(paths.root, "full-verify.json")) == verify_stat

    assert File.regular?(Path.join(paths.root, "baseline-verification-reconsideration.json"))

    assert [["superseded", "replaced_by_reconsidered_baseline_v1"]] =
             Repo.query!(
               "SELECT status, terminal_reason FROM baseline_revisions WHERE revision = 2"
             ).rows

    assert [[2]] =
             Repo.query!(
               "SELECT COUNT(*) FROM baseline_verifications WHERE baseline_revision_id = ?",
               [accepted_revision.id]
             ).rows

    assert [[1]] =
             Repo.query!(
               "SELECT COUNT(*) FROM sampling_revisions WHERE baseline_revision_id = ?",
               [accepted_revision.id]
             ).rows

    assert Lifecycle.project_work() == []
    assert {:ok, ^accepted_revision} = Lifecycle.reconsider_verified_revision(1, paths.root)
  end

  defp accept_baseline(work, revision) do
    with {:ok, _draft} <- Lifecycle.ensure_draft(work.root),
         {:ok, _submitted} <-
           Lifecycle.submit_definition(revision, work.root, "baseline-definition.json"),
         {:ok, approved} <-
           Lifecycle.review(revision, :approve, work.root, "baseline-definition.json") do
      result = V2BaselineFixtures.write_verification_result(work, approved.id, :accepted)
      result = put_in(result, ["details", "baseline_revision"], revision)
      V2BaselineFixtures.write_json(work.root, "baseline-verification-result.json", result)

      Lifecycle.finish_verification(
        revision,
        work.root,
        "baseline-verification-result.json"
      )
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
