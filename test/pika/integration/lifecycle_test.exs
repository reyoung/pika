Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Integration.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Attempt.Lifecycle, as: AttemptLifecycle
  alias Pika.Attempt.{Scheduler, Workspace}
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Integration.Lifecycle, as: IntegrationLifecycle
  alias Pika.Integration.{Decision, PromptInput}
  alias Pika.Optimization.{Config, Measurement, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.{Git, Repo}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-integration-workflow-#{System.unique_integer([:positive])}"
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
    %{baseline: baseline, config: config, workspace: workspace}
  end

  test "Accept verifies the intent and atomically advances Best and authoritative Metrics", %{
    baseline: baseline,
    config: config,
    workspace: workspace
  } do
    %{attempt: attempt, paths: paths} = ready_attempt(config, workspace)
    benchmark = benchmark([0, 1], :improved)
    write_validation(paths, baseline, attempt, benchmark, "accepted")

    assert [%{role_id: "integration", work_id: work_id}] = IntegrationLifecycle.project_work()
    assert work_id == to_string(attempt.id)
    assert {:ok, prompt} = PromptInput.render(attempt.id)
    assert prompt =~ "./verify_cases.sh --case-id 0,1"
    assert prompt =~ "./benchmark_cases.sh --case-id 0,1"

    assert {:ok, prepared} =
             IntegrationLifecycle.prepare_best_update(
               attempt.id,
               paths.root,
               "integration-validation.json"
             )

    assert prepared.run.status == "best_update_prepared"
    assert prepared.intent.state == "pending"
    assert prepared.intent.expected_best_sha == attempt.base_sha
    assert prepared.intent.candidate_sha == attempt.candidate_sha

    assert {:ok, repeated} =
             IntegrationLifecycle.prepare_best_update(
               attempt.id,
               paths.root,
               "integration-validation.json"
             )

    assert repeated.intent.id == prepared.intent.id

    best_after = squash_candidate(baseline.repo, attempt)
    write_accepted_result(paths, attempt, prepared.intent.id, best_after)

    assert {:ok, accepted} =
             IntegrationLifecycle.finish(
               attempt.id,
               paths.root,
               "integration-result.json",
               config
             )

    assert accepted.status == "accepted"
    assert accepted.candidate_sha == best_after
    assert Git.run!(baseline.repo, ["rev-parse", "pika/best"]) == best_after

    assert [[1, ^best_after, attempt_id]] =
             Repo.query!(
               "SELECT sequence, sha, source_attempt_id FROM best_revisions ORDER BY sequence DESC LIMIT 1"
             ).rows

    assert attempt_id == attempt.id
    assert [[^best_after]] = Repo.query!("SELECT best_sha FROM optimizations").rows

    assert [[4]] =
             Repo.query!(
               "SELECT COUNT(*) FROM best_metrics WHERE best_revision_id = (SELECT MAX(id) FROM best_revisions)"
             ).rows

    assert [["accepted", validation_id, result_id, "accepted"]] =
             Repo.query!(
               "SELECT status, validation_artifact_id, result_artifact_id, outcome FROM integration_runs WHERE attempt_id = ?",
               [attempt.id]
             ).rows

    assert is_binary(validation_id)
    assert is_binary(result_id)
    refute validation_id == result_id

    assert [["verified"]] =
             Repo.query!("SELECT state FROM operation_intents WHERE id = ?", [prepared.intent.id]).rows

    summaries = read_jsonl(Path.join(paths.root, "summary.jsonl"))
    assert Enum.map(summaries, & &1["outcome"]) == ["ready_for_integration", "accepted"]
  end

  test "Pika hard gates override an Agent Accept before any Best mutation intent", %{
    baseline: baseline,
    config: config,
    workspace: workspace
  } do
    %{attempt: attempt, paths: paths} = ready_attempt(config, workspace)

    write_validation(
      paths,
      baseline,
      attempt,
      benchmark([0, 1], :critical_regression),
      "accepted"
    )

    assert {:error, {:integration_hard_gate_rejected, decision}} =
             IntegrationLifecycle.prepare_best_update(
               attempt.id,
               paths.root,
               "integration-validation.json"
             )

    assert decision.outcome == :rejected
    assert decision.reason =~ "critical Case"
    assert {:ok, persisted} = AttemptLifecycle.fetch_attempt(attempt.id)
    assert persisted.status == "ready_for_integration"
    assert Repo.query!("SELECT COUNT(*) FROM integration_runs").rows == [[0]]
    assert Repo.query!("SELECT COUNT(*) FROM operation_intents").rows == [[0]]
    assert Git.run!(baseline.repo, ["rev-parse", "pika/best"]) == attempt.base_sha
  end

  test "an accepted validation monotonically feeds a noisy regressed Case into future Sampling",
       %{
         baseline: baseline,
         config: config,
         workspace: workspace
       } do
    _sampling_revision_id = create_single_case_sampling_revision()
    %{attempt: attempt, paths: paths} = ready_attempt(config, workspace, [0])

    judgements = [
      %{
        "case_id" => 1,
        "metric_id" => "latency_us",
        "classification" => "noise",
        "reason" => "the paired distribution is still within the fast-kernel noise envelope"
      }
    ]

    write_validation(
      paths,
      baseline,
      attempt,
      benchmark([0, 1], :ordinary_regression),
      "accepted",
      judgements,
      [1]
    )

    assert {:ok, prepared} =
             IntegrationLifecycle.prepare_best_update(
               attempt.id,
               paths.root,
               "integration-validation.json"
             )

    best_after = squash_candidate(baseline.repo, attempt)
    write_accepted_result(paths, attempt, prepared.intent.id, best_after)

    assert {:ok, %{status: "accepted"}} =
             IntegrationLifecycle.finish(
               attempt.id,
               paths.root,
               "integration-result.json",
               config
             )

    assert [[2, latest_sampling_id]] =
             Repo.query!(
               "SELECT sequence, id FROM sampling_revisions ORDER BY sequence DESC LIMIT 1"
             ).rows

    assert Repo.query!(
             "SELECT case_id FROM sampling_revision_cases WHERE sampling_revision_id = ? ORDER BY case_id",
             [latest_sampling_id]
           ).rows == [[0], [1]]
  end

  test "Reject atomically releases the queue and monotonically adds Sampling Feedback", %{
    config: config,
    workspace: workspace
  } do
    sampling_revision_id = create_single_case_sampling_revision()
    %{attempt: attempt, paths: paths} = ready_attempt(config, workspace, [0])
    assert attempt.sampling_revision_id == sampling_revision_id

    result = %{
      "schema_version" => 1,
      "role" => "integration",
      "work_id" => to_string(attempt.id),
      "attempt_id" => attempt.id,
      "outcome" => "rejected",
      "summary" => "full validation found a large-case regression",
      "details" => %{
        "reason" => "case 1 latency regressed beyond noise",
        "regressed_case_ids" => [1],
        "sampling_feedback" => [
          %{"case_id" => 1, "reason" => "cover the observed integration regression"}
        ]
      }
    }

    V2BaselineFixtures.write_json(paths.root, "integration-result.json", result)

    assert {:ok, rejected} =
             IntegrationLifecycle.finish(
               attempt.id,
               paths.root,
               "integration-result.json",
               config
             )

    assert rejected.status == "rejected"
    assert rejected.failure_reason == "case 1 latency regressed beyond noise"
    assert {:none, nil} = Scheduler.next_queue_action()

    assert [[2, latest_sampling_id]] =
             Repo.query!(
               "SELECT sequence, id FROM sampling_revisions ORDER BY sequence DESC LIMIT 1"
             ).rows

    assert Repo.query!(
             "SELECT case_id FROM sampling_revision_cases WHERE sampling_revision_id = ? ORDER BY case_id",
             [latest_sampling_id]
           ).rows == [[0], [1]]

    assert [["rejected", nil, result_id, "rejected"]] =
             Repo.query!(
               "SELECT status, validation_artifact_id, result_artifact_id, outcome FROM integration_runs WHERE attempt_id = ?",
               [attempt.id]
             ).rows

    assert is_binary(result_id)
  end

  test "cannot Reject after a pending intent has already mutated Best", %{
    baseline: baseline,
    config: config,
    workspace: workspace
  } do
    %{attempt: attempt, paths: paths} = ready_attempt(config, workspace)
    write_validation(paths, baseline, attempt, benchmark([0, 1], :improved), "accepted")

    assert {:ok, prepared} =
             IntegrationLifecycle.prepare_best_update(
               attempt.id,
               paths.root,
               "integration-validation.json"
             )

    best_after = squash_candidate(baseline.repo, attempt)

    rejected = %{
      "schema_version" => 1,
      "role" => "integration",
      "work_id" => to_string(attempt.id),
      "attempt_id" => attempt.id,
      "outcome" => "rejected",
      "summary" => "cannot safely accept",
      "details" => %{
        "reason" => "late concern",
        "regressed_case_ids" => [],
        "sampling_feedback" => []
      }
    }

    V2BaselineFixtures.write_json(paths.root, "integration-result.json", rejected)

    assert {:error, {:best_mutated_pending_intent, expected, ^best_after}} =
             IntegrationLifecycle.finish(
               attempt.id,
               paths.root,
               "integration-result.json",
               config
             )

    assert expected == attempt.base_sha
    assert {:ok, %{status: "integrating"}} = AttemptLifecycle.fetch_attempt(attempt.id)

    assert [["pending"]] =
             Repo.query!("SELECT state FROM operation_intents WHERE id = ?", [prepared.intent.id]).rows
  end

  defp ready_attempt(config, workspace, sampled_case_ids \\ [0, 1]) do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    candidate_sha = commit_candidate(paths.repo)
    attempt = Map.put(attempt, :candidate_sha, candidate_sha)
    write_iteration_result(paths, attempt, sampled_case_ids)

    assert {:ok, ready} =
             AttemptLifecycle.finish(attempt.id, paths.root, "iteration-result.json")

    %{attempt: ready, paths: paths}
  end

  defp write_iteration_result(paths, attempt, case_ids) do
    verify = verify(case_ids)
    samples = benchmark(case_ids, :improved)

    patch =
      Git.run!(paths.repo, ["diff", "--binary", "#{attempt.base_sha}..#{attempt.candidate_sha}"])

    File.write!(Path.join(paths.root, "sampled-verify.json"), Jason.encode!(verify))
    V2BaselineFixtures.write_jsonl(paths.root, "sampled-benchmark.jsonl", samples)
    File.write!(Path.join(paths.root, "candidate.patch"), patch)

    result = %{
      "schema_version" => 1,
      "role" => "iteration",
      "work_id" => to_string(attempt.id),
      "outcome" => "ready_for_integration",
      "summary" => "sampled cases improved",
      "files" => %{
        "verify" => identity(paths.root, "sampled-verify.json"),
        "benchmark" => identity(paths.root, "sampled-benchmark.jsonl"),
        "patch" => identity(paths.root, "candidate.patch")
      },
      "details" => %{
        "attempt_id" => attempt.id,
        "iteration_round" => attempt.current_iteration_round,
        "base_sha" => attempt.base_sha,
        "candidate_sha" => attempt.candidate_sha,
        "sampling_revision" => sampling_sequence(attempt.sampling_revision_id),
        "hypothesis" => "vectorize the kernel",
        "changes" => ["added candidate implementation"],
        "risks" => [],
        "failure_reason" => nil
      }
    }

    V2BaselineFixtures.write_json(paths.root, "iteration-result.json", result)
  end

  defp write_validation(
         paths,
         baseline,
         attempt,
         samples,
         recommendation,
         judgements \\ [],
         feedback_case_ids \\ []
       ) do
    verify = verify([0, 1])
    File.write!(Path.join(paths.root, "integration-verify.json"), Jason.encode!(verify))
    V2BaselineFixtures.write_jsonl(paths.root, "integration-benchmark.jsonl", samples)

    cases = V2BaselineFixtures.read_json(baseline.root, "cases.json")["cases"]
    metrics = V2BaselineFixtures.read_json(baseline.root, "metrics.json")["metrics"]

    measurement =
      V2BaselineFixtures.read_json(baseline.root, "baseline-definition.json")["measurement"]

    assert {:ok, statistics} = Measurement.evaluate(samples, cases, metrics, measurement)

    assert {:ok, decision} =
             Decision.evaluate(statistics, best_metrics(), cases, metrics, judgements)

    validation = %{
      "schema_version" => 1,
      "role" => "integration",
      "work_id" => to_string(attempt.id),
      "attempt_id" => attempt.id,
      "base_sha" => attempt.base_sha,
      "candidate_sha" => attempt.candidate_sha,
      "sampling_revision" => sampling_sequence(attempt.sampling_revision_id),
      "recommended_outcome" => recommendation,
      "files" => %{
        "verify" => identity(paths.root, "integration-verify.json"),
        "benchmark" => identity(paths.root, "integration-benchmark.jsonl")
      },
      "judgements" => judgements,
      "weighted_aggregates" =>
        Enum.map(decision.weighted_aggregates, fn aggregate ->
          %{
            "metric_id" => aggregate.metric_id,
            "regression_ratio" => aggregate.regression_ratio
          }
        end),
      "sampling_feedback_case_ids" => feedback_case_ids,
      "summary" => "full-set validation complete"
    }

    V2BaselineFixtures.write_json(paths.root, "integration-validation.json", validation)
  end

  defp write_accepted_result(paths, attempt, intent_id, best_after) do
    result = %{
      "schema_version" => 1,
      "role" => "integration",
      "work_id" => to_string(attempt.id),
      "attempt_id" => attempt.id,
      "outcome" => "accepted",
      "summary" => "candidate accepted after full validation",
      "details" => %{
        "intent_id" => intent_id,
        "best_before_sha" => attempt.base_sha,
        "best_after_sha" => best_after,
        "squash_sha" => best_after,
        "trailers" => trailers(attempt)
      }
    }

    V2BaselineFixtures.write_json(paths.root, "integration-result.json", result)
  end

  defp squash_candidate(repo, attempt) do
    Git.run!(repo, ["checkout", "pika/best"])
    Git.run!(repo, ["cherry-pick", "--no-commit", attempt.candidate_sha])

    message =
      """
      integrate Attempt #{attempt.id}

      Pika-Attempt: #{attempt.id}
      Pika-Baseline-Revision: 0
      Pika-Sampling-Revision: #{sampling_sequence(attempt.sampling_revision_id)}
      """
      |> String.trim()

    Git.run!(repo, ["commit", "-m", message])
    Git.run!(repo, ["rev-parse", "HEAD"])
  end

  defp trailers(attempt) do
    %{
      "Pika-Attempt" => to_string(attempt.id),
      "Pika-Baseline-Revision" => "0",
      "Pika-Sampling-Revision" => to_string(sampling_sequence(attempt.sampling_revision_id))
    }
  end

  defp create_single_case_sampling_revision do
    [[baseline_revision_id]] =
      Repo.query!("SELECT id FROM baseline_revisions WHERE revision = 0").rows

    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO sampling_revisions(optimization_id, baseline_revision_id, sequence, cause, created_at) VALUES ('optimization', ?, 1, 'test_focus', ?)",
      [baseline_revision_id, now]
    )

    [[sampling_revision_id]] = Repo.query!("SELECT last_insert_rowid()").rows

    Repo.query!(
      "INSERT INTO sampling_revision_cases(sampling_revision_id, case_id, reason) VALUES (?, 0, 'critical')",
      [sampling_revision_id]
    )

    sampling_revision_id
  end

  defp best_metrics do
    Repo.query!("""
    SELECT case_id, metric_id, normalized_ratio, noise_tolerance
    FROM best_metrics
    WHERE best_revision_id = (SELECT id FROM best_revisions ORDER BY sequence DESC LIMIT 1)
    """).rows
    |> Enum.map(fn [case_id, metric_id, ratio, noise] ->
      %{
        case_id: case_id,
        metric_id: metric_id,
        normalized_ratio: ratio,
        noise_tolerance: noise
      }
    end)
  end

  defp commit_candidate(repo) do
    File.write!(Path.join(repo, "candidate.txt"), "optimized\n")
    Git.run!(repo, ["add", "candidate.txt"])
    Git.run!(repo, ["commit", "-m", "optimize candidate"])
    Git.run!(repo, ["rev-parse", "HEAD"])
  end

  defp verify(case_ids) do
    %{
      "schema_version" => 1,
      "requested_case_ids" => case_ids,
      "cases" =>
        Enum.map(case_ids, fn case_id ->
          %{
            "case_id" => case_id,
            "target" => %{"passed" => true},
            "candidate" => %{"passed" => true},
            "comparison" => %{"passed" => true},
            "error" => nil
          }
        end)
    }
  end

  defp benchmark(case_ids, outcome) do
    for case_id <- case_ids,
        metric_id <- ["latency_us", "bandwidth_gbps"],
        pair_index <- 0..2 do
      target = if metric_id == "latency_us", do: 10.0 + case_id, else: 100.0 + case_id

      candidate =
        case {outcome, case_id, metric_id} do
          {:critical_regression, 0, "latency_us"} -> target + 3.0
          {:ordinary_regression, 1, "latency_us"} -> target + 2.1
          {_, _, "latency_us"} -> target - 1.0
          _ -> target + 15.0
        end

      %{
        "schema_version" => 1,
        "case_id" => case_id,
        "metric_id" => metric_id,
        "pair_index" => pair_index,
        "order" => if(rem(pair_index, 2) == 0, do: "target_candidate", else: "candidate_target"),
        "target" => target + pair_index * 0.01,
        "candidate" => candidate + pair_index * 0.01,
        "valid" => true,
        "error" => nil
      }
    end
  end

  defp identity(root, path), do: V2BaselineFixtures.file_identity(root, path)

  defp sampling_sequence(id) do
    [[sequence]] = Repo.query!("SELECT sequence FROM sampling_revisions WHERE id = ?", [id]).rows
    sequence
  end

  defp read_jsonl(path), do: path |> File.stream!() |> Enum.map(&Jason.decode!/1)

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
        regression_feedback_cases: 2
    """
  end
end
