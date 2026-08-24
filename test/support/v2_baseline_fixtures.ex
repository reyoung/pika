defmodule Pika.Test.V2BaselineFixtures do
  @moduledoc false

  alias Pika.Git
  alias Pika.Optimization.Measurement

  def create_work_root(workspace, revision \\ 0) do
    root = Path.join([workspace, "baseline", "revisions", Integer.to_string(revision)])
    repo = Path.join(root, "repo")
    File.mkdir_p!(repo)

    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])

    write_executable(repo, "verify_cases.sh")
    write_executable(repo, "benchmark_cases.sh")
    File.write!(Path.join(repo, "kernel.py"), "def run():\n    return 1\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "baseline development"])
    development_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    write_definition_files(root, development_sha)
    %{root: root, repo: repo, development_sha: development_sha}
  end

  def write_definition_files(root, development_sha) do
    File.mkdir_p!(Path.join(root, "target"))
    target_contents = "def run():\n    return 1\n"
    File.write!(Path.join(root, "target/target.py"), target_contents)

    write_json(root, "target/manifest.json", %{
      "schema_version" => 1,
      "source_kind" => "generated_bundle",
      "source_description" => "test target",
      "source_commit_sha" => nil,
      "entrypoint" => "target.py:run",
      "files" => [
        %{
          "path" => "target.py",
          "sha256" => sha256(target_contents),
          "size" => byte_size(target_contents)
        }
      ]
    })

    write_json(root, "cases.json", %{
      "schema_version" => 1,
      "cases" => [
        %{
          "id" => 0,
          "name" => "small",
          "description" => "small case",
          "inputs" => %{"m" => 16},
          "weight" => 1.0,
          "critical" => true
        },
        %{
          "id" => 1,
          "name" => "large",
          "description" => "large case",
          "inputs" => %{"m" => 64},
          "weight" => 2.0,
          "critical" => false
        }
      ]
    })

    write_json(root, "metrics.json", %{
      "schema_version" => 1,
      "metrics" => [
        %{
          "id" => "latency_us",
          "unit" => "us",
          "direction" => "minimize",
          "role" => "primary"
        },
        %{
          "id" => "bandwidth_gbps",
          "unit" => "GB/s",
          "direction" => "maximize",
          "role" => "informational"
        }
      ]
    })

    write_json(root, "smoke-verify.json", %{
      "schema_version" => 1,
      "requested_case_ids" => [0],
      "cases" => [
        %{
          "case_id" => 0,
          "target" => %{"passed" => true},
          "candidate" => %{"passed" => true},
          "comparison" => %{"passed" => true},
          "error" => nil
        }
      ]
    })

    write_jsonl(root, "smoke-benchmark.jsonl", [
      benchmark_record("latency_us", 10.0, 12.0),
      benchmark_record("bandwidth_gbps", 100.0, 90.0)
    ])

    write_json(root, "baseline-definition.json", %{
      "schema_version" => 1,
      "summary" => "test baseline",
      "optimization_target" => %{
        "manifest_path" => "target/manifest.json",
        "entrypoint" => "target.py:run"
      },
      "development_baseline" => %{
        "commit_sha" => development_sha,
        "entrypoint" => "kernel.py:run"
      },
      "correctness" => %{"mode" => "target_equivalence"},
      "verify" => %{"script" => "verify_cases.sh", "output_schema_version" => 1},
      "benchmark" => %{"script" => "benchmark_cases.sh", "output_schema_version" => 1},
      "cases_path" => "cases.json",
      "metrics_path" => "metrics.json",
      "measurement" => %{"warmup" => 2, "pair_count" => 3, "min_valid_pairs" => 2},
      "smoke_verify_path" => "smoke-verify.json",
      "smoke_benchmark_path" => "smoke-benchmark.jsonl"
    })
  end

  def write_json(root, path, value), do: File.write!(Path.join(root, path), Jason.encode!(value))

  def read_json(root, path), do: root |> Path.join(path) |> File.read!() |> Jason.decode!()

  def write_jsonl(root, path, values) do
    File.write!(Path.join(root, path), Enum.map_join(values, "", &(Jason.encode!(&1) <> "\n")))
  end

  def write_verification_result(work, baseline_id, outcome \\ :accepted) do
    definition = read_json(work.root, "baseline-definition.json")
    cases = read_json(work.root, "cases.json")["cases"]
    metrics = read_json(work.root, "metrics.json")["metrics"]

    case outcome do
      :accepted ->
        verify = %{
          "schema_version" => 1,
          "requested_case_ids" => Enum.map(cases, & &1["id"]),
          "cases" =>
            Enum.map(cases, fn case_ ->
              %{
                "case_id" => case_["id"],
                "target" => %{"passed" => true},
                "candidate" => %{"passed" => true},
                "comparison" => %{"passed" => true},
                "error" => nil
              }
            end)
        }

        benchmark = full_benchmark(cases, metrics, definition["measurement"]["pair_count"])

        {:ok, statistics} =
          Measurement.evaluate(benchmark, cases, metrics, definition["measurement"])

        write_json(work.root, "full-verify.json", verify)
        write_jsonl(work.root, "full-benchmark.jsonl", benchmark)

        result = %{
          "schema_version" => 1,
          "role" => "baseline_verify",
          "work_id" => to_string(baseline_id),
          "outcome" => "accepted",
          "summary" => "baseline accepted",
          "files" => %{
            "verify" => file_identity(work.root, "full-verify.json"),
            "benchmark" => file_identity(work.root, "full-benchmark.jsonl")
          },
          "details" => %{
            "baseline_revision" => 0,
            "definition_sha256" => file_identity(work.root, "baseline-definition.json")["sha256"],
            "development_sha" => work.development_sha,
            "initial_iteration_case_ids" => [0, 1],
            "case_selection_reasons" => [
              %{"case_id" => 0, "reason" => "critical"},
              %{"case_id" => 1, "reason" => "representative"}
            ],
            "case_metrics" => Enum.map(statistics, &reported_metric/1),
            "judgement" => %{"reasonable" => true, "reason" => "stable full run"}
          }
        }

        write_json(work.root, "baseline-verification-result.json", result)
        result

      :rejected ->
        result = %{
          "schema_version" => 1,
          "role" => "baseline_verify",
          "work_id" => to_string(baseline_id),
          "outcome" => "definition_rejected",
          "summary" => "benchmark failed",
          "files" => %{},
          "details" => %{
            "baseline_revision" => 0,
            "definition_sha256" => file_identity(work.root, "baseline-definition.json")["sha256"],
            "development_sha" => work.development_sha,
            "failure_kind" => "benchmark",
            "reason" => "unstable measurements",
            "requested_changes" => ["fix benchmark synchronization"]
          }
        }

        write_json(work.root, "baseline-verification-result.json", result)
        result
    end
  end

  def file_identity(root, path) do
    contents = File.read!(Path.join(root, path))
    %{"path" => path, "sha256" => sha256(contents)}
  end

  defp full_benchmark(cases, metrics, pair_count) do
    for case_ <- cases, metric <- metrics, pair_index <- 0..(pair_count - 1) do
      target =
        if metric["direction"] == "minimize", do: 10.0 + case_["id"], else: 100.0 + case_["id"]

      candidate = if metric["direction"] == "minimize", do: target + 2.0, else: target + 10.0

      %{
        "schema_version" => 1,
        "case_id" => case_["id"],
        "metric_id" => metric["id"],
        "pair_index" => pair_index,
        "order" => if(rem(pair_index, 2) == 0, do: "target_candidate", else: "candidate_target"),
        "target" => target + pair_index * 0.01,
        "candidate" => candidate + pair_index * 0.01,
        "valid" => true,
        "error" => nil
      }
    end
  end

  defp reported_metric(statistic) do
    %{
      "case_id" => statistic.case_id,
      "metric_id" => statistic.metric_id,
      "unit" => statistic.unit,
      "target_value" => statistic.target_value,
      "development_value" => statistic.development_value,
      "relative_difference" => statistic.relative_difference,
      "noise_tolerance" => statistic.noise_tolerance,
      "valid_pair_count" => statistic.valid_pair_count
    }
  end

  defp benchmark_record(metric_id, target, candidate) do
    %{
      "schema_version" => 1,
      "case_id" => 0,
      "metric_id" => metric_id,
      "pair_index" => 0,
      "order" => "target_candidate",
      "target" => target,
      "candidate" => candidate,
      "valid" => true,
      "error" => nil
    }
  end

  defp write_executable(root, path) do
    absolute = Path.join(root, path)
    File.write!(absolute, "#!/usr/bin/env bash\nexit 0\n")
    File.chmod!(absolute, 0o755)
  end

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
