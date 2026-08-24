Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Baseline.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Baseline.Lifecycle
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

  test "rejected Verification preserves its Result for the next Alignment", %{work: work} do
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
