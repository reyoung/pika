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
               [to_string(attempt.id)]
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

    result = %{
      "schema_version" => 1,
      "role" => "iteration",
      "work_id" => to_string(attempt.id),
      "outcome" => "rejected",
      "summary" => "direction did not improve",
      "files" => files,
      "details" => %{
        "attempt_id" => attempt.id,
        "iteration_round" => attempt.current_iteration_round,
        "base_sha" => attempt.base_sha,
        "candidate_sha" => nil,
        "sampling_revision" => 0,
        "hypothesis" => "increase stages",
        "changes" => [],
        "risks" => [],
        "failure_reason" => "no improvement"
      }
    }

    V2BaselineFixtures.write_json(paths.root, "iteration-result.json", result)
  end

  defp commit_candidate(repo, message) do
    File.write!(Path.join(repo, "candidate.txt"), message <> "\n")
    Git.run!(repo, ["add", "candidate.txt"])
    Git.run!(repo, ["commit", "-m", message])
    Git.run!(repo, ["rev-parse", "HEAD"])
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
