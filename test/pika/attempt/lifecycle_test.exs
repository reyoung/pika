Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Attempt.LifecycleTest do
  use ExUnit.Case, async: false

  alias Pika.Attempt.{Lifecycle, Scheduler, Workspace}
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.{Git, Repo}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-attempt-lifecycle-#{System.unique_integer([:positive])}"
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

  test "ready result validates sampled evidence, Git Patch, Metrics, and journals", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    candidate_sha = commit_candidate(paths.repo, "optimize kernel")
    write_ready_result(paths, attempt, candidate_sha)

    assert {:ok, ready} = Lifecycle.finish(attempt.id, paths.root, "iteration-result.json")
    assert ready.status == "ready_for_integration"
    assert ready.candidate_sha == candidate_sha

    assert [[4]] =
             Repo.query!("SELECT COUNT(*) FROM attempt_metrics WHERE attempt_id = ?", [attempt.id]).rows

    summaries =
      paths.root |> Path.join("summary.jsonl") |> File.stream!() |> Enum.map(&Jason.decode!/1)

    attempt_id = attempt.id

    assert [%{"attempt_id" => ^attempt_id, "outcome" => "ready_for_integration"}] =
             Enum.map(summaries, &Map.take(&1, ["attempt_id", "outcome"]))

    assert {:integrate, queued} = Scheduler.next_queue_action()
    assert queued.id == attempt.id
  end

  test "rejected result terminates the Attempt without formal evidence", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    write_rejected_result(paths, attempt)

    assert {:ok, rejected} = Lifecycle.finish(attempt.id, paths.root, "iteration-result.json")
    assert rejected.status == "rejected"
    assert rejected.failure_reason == "no improvement"

    assert [[0]] =
             Repo.query!("SELECT COUNT(*) FROM attempt_metrics WHERE attempt_id = ?", [attempt.id]).rows

    [summary] =
      paths.root |> Path.join("summary.jsonl") |> File.stream!() |> Enum.map(&Jason.decode!/1)

    assert summary["status"] == "rejected"
    assert File.read!(Path.join(paths.root, "message.jsonl")) == ""
  end

  test "rejected result retains complete sampled Benchmark metrics", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    write_rejected_result(paths, attempt, benchmark: true)

    assert {:ok, rejected} = Lifecycle.finish(attempt.id, paths.root, "iteration-result.json")
    assert rejected.status == "rejected"

    assert [[4]] =
             Repo.query!("SELECT COUNT(*) FROM attempt_metrics WHERE attempt_id = ?", [attempt.id]).rows

    assert [[1]] =
             Repo.query!(
               "SELECT COUNT(*) FROM artifacts WHERE owner_type = 'attempt' AND owner_id = ? AND kind = 'iteration_benchmark'",
               ["#{attempt.id}:round:1"]
             ).rows
  end

  test "stale refresh writes immutable artifacts in an independent Round workspace", %{
    baseline: baseline,
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    first_paths = Workspace.paths(workspace, attempt.id)
    candidate_sha = commit_candidate(first_paths.repo, "round one candidate")
    write_ready_result(first_paths, attempt, candidate_sha)

    assert {:ok, ready} = Lifecycle.finish(attempt.id, first_paths.root, "iteration-result.json")
    first_result = File.read!(Path.join(first_paths.root, "iteration-result.json"))
    advance_best(baseline.repo, candidate_sha, attempt.id)

    assert {:refresh, refreshed} = Scheduler.next_queue_action()
    second_paths = Workspace.round_paths(workspace, attempt.id, 2)
    assert second_paths.root != first_paths.root
    assert File.dir?(second_paths.repo)
    assert Git.run!(second_paths.repo, ["rev-parse", "HEAD"]) == ready.candidate_sha

    write_rejected_result(second_paths, refreshed)

    assert {:error, {:iteration_round_workspace_mismatch, _details}} =
             Lifecycle.finish(attempt.id, first_paths.root, "iteration-result.json")

    assert {:error, {:iteration_result_path_mismatch, "iteration-result-round2.json"}} =
             Lifecycle.finish(attempt.id, second_paths.root, "iteration-result-round2.json")

    assert {:ok, rejected} =
             Lifecycle.finish(attempt.id, second_paths.root, "iteration-result.json")

    assert rejected.status == "rejected"
    assert File.read!(Path.join(first_paths.root, "iteration-result.json")) == first_result

    assert [
             [1, first_relative, "completed"],
             [2, second_relative, "rejected"]
           ] =
             Repo.query!(
               "SELECT round, work_relative_path, status FROM iteration_rounds WHERE attempt_id = ? ORDER BY round",
               [attempt.id]
             ).rows

    assert first_relative == first_paths.relative_root
    assert second_relative == second_paths.relative_root

    assert [[2]] =
             Repo.query!(
               "SELECT COUNT(*) FROM artifacts WHERE owner_type = 'attempt' AND owner_id IN (?, ?) AND kind = 'iteration_result'",
               ["#{attempt.id}:round:1", "#{attempt.id}:round:2"]
             ).rows
  end

  test "structural Harness rejection supersedes Baseline and stops new Attempts", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [first]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, first.id)

    write_rejected_result(paths, first,
      failure_code: "baseline_harness_missing_candidate_env",
      failure_reason: "baseline_harness_missing_candidate_env: adapter ignores runtime manifest"
    )

    assert {:ok, rejected} = Lifecycle.finish(first.id, paths.root, "iteration-result.json")
    assert rejected.status == "rejected"
    assert Persistence.current().status == "aligning_baseline"
    assert Persistence.current().stop_reason == "baseline_harness_missing_candidate_env"
    assert BaselineLifecycle.latest_revision().status == "superseded"

    assert {:error, {:optimization_not_spawning, "aligning_baseline"}} =
             Scheduler.spawn_available(config)

    assert [["baseline_realignment_requested"]] =
             Repo.query!(
               "SELECT event_type FROM domain_events WHERE event_type = 'baseline_realignment_requested'"
             ).rows
  end

  test "ready result cannot change protected Harness paths", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    File.write!(Path.join(paths.repo, "verify_cases.sh"), "#!/usr/bin/env bash\necho changed\n")
    Git.run!(paths.repo, ["add", "verify_cases.sh"])
    Git.run!(paths.repo, ["commit", "-m", "change protected harness"])
    candidate_sha = Git.run!(paths.repo, ["rev-parse", "HEAD"])
    write_ready_result(paths, attempt, candidate_sha)

    assert {:error, :protected_path_changed} =
             Lifecycle.finish(attempt.id, paths.root, "iteration-result.json")

    assert {:ok, persisted} = Lifecycle.fetch_attempt(attempt.id)
    assert persisted.status == "iterating"
  end

  test "ready result cannot force-add a Reference Project path", %{
    config: config,
    workspace: workspace
  } do
    assert {:ok, [attempt]} = Scheduler.spawn_available(config)
    paths = Workspace.paths(workspace, attempt.id)
    File.mkdir_p!(Path.join(paths.repo, "ref"))
    File.ln_s!(System.tmp_dir!(), Path.join(paths.repo, "ref/forced-reference"))
    Git.run!(paths.repo, ["add", "-f", "ref/forced-reference"])
    Git.run!(paths.repo, ["commit", "-m", "force-add reference link"])
    candidate_sha = Git.run!(paths.repo, ["rev-parse", "HEAD"])
    write_ready_result(paths, attempt, candidate_sha)

    assert {:error, :protected_path_changed} =
             Lifecycle.finish(attempt.id, paths.root, "iteration-result.json")
  end

  defp write_ready_result(paths, attempt, candidate_sha) do
    verify = full_verify()
    benchmark = full_benchmark()
    patch = Git.run!(paths.repo, ["diff", "--binary", "#{attempt.base_sha}..#{candidate_sha}"])
    File.write!(Path.join(paths.root, "verify.json"), Jason.encode!(verify))
    V2BaselineFixtures.write_jsonl(paths.root, "benchmark.jsonl", benchmark)
    File.write!(Path.join(paths.root, "candidate.patch"), patch)

    result = %{
      "schema_version" => 1,
      "role" => "iteration",
      "work_id" => to_string(attempt.id),
      "outcome" => "ready_for_integration",
      "summary" => "candidate improves sampled cases",
      "files" => %{
        "verify" => identity(paths.root, "verify.json"),
        "benchmark" => identity(paths.root, "benchmark.jsonl"),
        "patch" => identity(paths.root, "candidate.patch")
      },
      "details" => %{
        "attempt_id" => attempt.id,
        "iteration_round" => attempt.current_iteration_round,
        "base_sha" => attempt.base_sha,
        "candidate_sha" => candidate_sha,
        "sampling_revision" => 0,
        "hypothesis" => "vectorize",
        "changes" => ["changed kernel"],
        "risks" => [],
        "failure_reason" => nil
      }
    }

    V2BaselineFixtures.write_json(paths.root, "iteration-result.json", result)
  end

  defp write_rejected_result(paths, attempt, opts \\ []) do
    files =
      if opts[:benchmark] do
        V2BaselineFixtures.write_jsonl(paths.root, "benchmark.jsonl", full_benchmark())
        %{"benchmark" => identity(paths.root, "benchmark.jsonl")}
      else
        %{}
      end

    details = %{
      "attempt_id" => attempt.id,
      "iteration_round" => attempt.current_iteration_round,
      "base_sha" => attempt.base_sha,
      "candidate_sha" => nil,
      "sampling_revision" => 0,
      "hypothesis" => "increase stages",
      "changes" => [],
      "risks" => [],
      "failure_reason" => opts[:failure_reason] || "no improvement"
    }

    details =
      if opts[:failure_code],
        do: Map.put(details, "failure_code", opts[:failure_code]),
        else: details

    result = %{
      "schema_version" => 1,
      "role" => "iteration",
      "work_id" => to_string(attempt.id),
      "outcome" => "rejected",
      "summary" => "direction did not improve",
      "files" => files,
      "details" => details
    }

    V2BaselineFixtures.write_json(paths.root, "iteration-result.json", result)
  end

  defp commit_candidate(repo, message) do
    File.write!(Path.join(repo, "candidate.txt"), message <> "\n")
    Git.run!(repo, ["add", "candidate.txt"])
    Git.run!(repo, ["commit", "-m", message])
    Git.run!(repo, ["rev-parse", "HEAD"])
  end

  defp advance_best(repo, sha, attempt_id) do
    Git.run!(repo, ["branch", "-f", "pika/best", sha])
    now = System.system_time(:microsecond)

    [[baseline_revision_id]] =
      Repo.query!("SELECT id FROM baseline_revisions WHERE status = 'accepted'").rows

    Repo.query!(
      """
      INSERT INTO best_revisions(
        optimization_id, sequence, sha, source_kind, source_attempt_id,
        baseline_revision_id, summary, created_at
      ) VALUES ('optimization', 1, ?, 'attempt', ?, ?, 'advanced', ?)
      """,
      [sha, attempt_id, baseline_revision_id, now]
    )

    Repo.query!("UPDATE optimizations SET best_sha = ? WHERE id = 'optimization'", [sha])
  end

  defp full_verify do
    %{
      "schema_version" => 1,
      "requested_case_ids" => [0, 1],
      "cases" =>
        Enum.map([0, 1], fn case_id ->
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

  defp full_benchmark do
    for case_id <- [0, 1], metric_id <- ["latency_us", "bandwidth_gbps"], pair_index <- 0..2 do
      target = if metric_id == "latency_us", do: 10.0 + case_id, else: 100.0 + case_id
      candidate = if metric_id == "latency_us", do: target - 1.0, else: target + 5.0

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

  defp identity(root, path) do
    contents = File.read!(Path.join(root, path))
    %{"path" => path, "sha256" => :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)}
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
