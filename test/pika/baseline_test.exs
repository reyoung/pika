defmodule Pika.BaselineTest do
  use ExUnit.Case, async: true

  alias Pika.Baseline
  alias Pika.Test.AlignmentFixtures

  test "recomputes median, signed pair deltas, MAD and noise floor" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("a", 40)
    pair_count = spec["benchmark"]["pair_count"]

    records =
      for index <- 0..(pair_count - 1) do
        %{
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
          "target" => 10.0,
          "candidate" => 10.01,
          "valid" => true
        }
      end

    assert {:ok, [metric]} = Baseline.evaluate_records(records, spec, sha)
    assert_in_delta metric.value, 10.01, 1.0e-9
    assert_in_delta metric.target_value, 10.0, 1.0e-9
    assert_in_delta metric.pair_delta_median, -0.001, 1.0e-9
    assert metric.noise_tolerance == 0.005
    assert metric.valid_pair_count == pair_count
  end

  test "requires exact pair indexes and the user-specified validity floor" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("b", 40)
    pair_count = spec["benchmark"]["pair_count"]

    records =
      for index <- 0..(pair_count - 1) do
        %{
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
          "target" => 10.0,
          "candidate" => 10.0,
          "valid" => index < 4
        }
      end

    assert {:error, {:insufficient_valid_pairs, "target_case", "latency_us", 4}} =
             Baseline.evaluate_records(records, spec, sha)
  end

  test "rejects Pair records that are not ordered alternately" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("c", 40)
    pair_count = spec["benchmark"]["pair_count"]

    records =
      for index <- 0..(pair_count - 1) do
        %{
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => "tc",
          "target" => 10.0,
          "candidate" => 10.0,
          "valid" => true
        }
      end

    assert {:error, {:non_alternating_pair_orders, "target_case", "latency_us"}} =
             Baseline.evaluate_records(records, spec, sha)
  end

  test "uses the formal pair count declared by the Campaign Spec" do
    spec =
      AlignmentFixtures.spec()
      |> put_in(["benchmark", "pair_count"], 8)
      |> put_in(["benchmark", "min_valid_pairs"], 6)

    sha = String.duplicate("d", 40)

    records =
      for index <- 0..7 do
        %{
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
          "target" => 10.0,
          "candidate" => 9.8,
          "valid" => index < 6
        }
      end

    assert {:ok, [%{pair_count: 8, valid_pair_count: 6}]} =
             Baseline.evaluate_records(records, spec, sha)
  end

  test "streams a Baseline artifact and reports validation phases" do
    root = AlignmentFixtures.temp_dir("pika-baseline-stream")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("e", 40)
    skill_sha = String.duplicate("f", 40)

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, sha, skill_sha)

    owner = self()

    assert {:ok,
            %{
              metrics: [%{case_id: "target_case", metric_id: "latency_us"}],
              samples_artifact: sample_artifact
            }} =
             Baseline.evaluate(
               Path.join(root, samples),
               Path.join(root, correctness),
               Path.join(root, profiler),
               AlignmentFixtures.spec(),
               sha,
               skill_sha,
               target_snapshot_id: "target-fixture",
               on_progress: &send(owner, {:progress, &1})
             )

    samples_body = File.read!(Path.join(root, samples))

    assert sample_artifact.sha256 ==
             :crypto.hash(:sha256, samples_body) |> Base.encode16(case: :lower)

    assert sample_artifact.size == byte_size(samples_body)

    assert_received {:progress,
                     %{phase: :reading_samples, processed_records: 0, total_records: 7}}

    assert_received {:progress,
                     %{phase: :reading_samples, processed_records: 7, completed_groups: 1}}

    assert_received {:progress, %{phase: :validating_correctness}}
    assert_received {:progress, %{phase: :validating_profiler}}
    assert_received {:progress, %{phase: :completed}}
  end

  test "validates canonical Case/Metric groups concurrently without changing result order" do
    root = AlignmentFixtures.temp_dir("pika-baseline-parallel")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("9", 40)
    skill_sha = String.duplicate("8", 40)

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, sha, skill_sha)

    cases =
      for index <- 1..4 do
        %{
          "id" => if(index == 1, do: "target_case", else: "case_#{index}"),
          "name" => "Case #{index}",
          "kind" => "target",
          "shape" => %{"n" => index * 1_024},
          "dtype" => "float16",
          "layout" => "contiguous",
          "frequency_weight" => 0.25
        }
      end

    metrics = [
      hd(AlignmentFixtures.spec()["metrics"]),
      %{
        "id" => "throughput",
        "name" => "Throughput",
        "unit" => "items/s",
        "direction" => "maximize",
        "role" => "guard",
        "min_improvement_ratio" => 0.01
      }
    ]

    spec =
      AlignmentFixtures.spec()
      |> Map.put("benchmark_cases", cases)
      |> Map.put("metrics", metrics)

    records =
      for case_ <- cases, metric <- metrics, index <- 0..(AlignmentFixtures.pair_count() - 1) do
        %{
          "case_id" => case_["id"],
          "metric_id" => metric["id"],
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
          "target" => 10.0 + index / 1_000,
          "candidate" => 10.01 + index / 1_000,
          "valid" => true
        }
      end

    samples_path = Path.join(root, samples)
    File.write!(samples_path, Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n")

    File.write!(
      Path.join(root, correctness),
      Jason.encode!(%{
        "schema_version" => 2,
        "target_snapshot_id" => "target-fixture",
        "candidate_sha" => sha,
        "cases" =>
          Enum.map(
            cases,
            &%{
              "case_id" => &1["id"],
              "target_passed" => true,
              "candidate_passed" => true
            }
          )
      })
    )

    owner = self()

    assert {:ok, %{metrics: results}} =
             Baseline.evaluate(
               samples_path,
               Path.join(root, correctness),
               Path.join(root, profiler),
               spec,
               sha,
               skill_sha,
               target_snapshot_id: "target-fixture",
               max_concurrency: 4,
               on_progress: &send(owner, {:parallel_progress, &1})
             )

    assert Enum.map(results, &{&1.case_id, &1.metric_id}) ==
             for(case_ <- cases, metric <- metrics, do: {case_["id"], metric["id"]})

    expected_concurrency = min(4, System.schedulers_online())

    assert_received {:parallel_progress,
                     %{
                       phase: :reading_samples,
                       completed_groups: 8,
                       processed_records: 56,
                       max_concurrency: ^expected_concurrency
                     }}
  end

  test "checks a previously registered sample digest during the parsing pass" do
    root = AlignmentFixtures.temp_dir("pika-baseline-digest")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("3", 40)
    skill_sha = String.duplicate("4", 40)

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, sha, skill_sha)

    assert {:error, {:samples_sha256_mismatch, "wrong", _actual}} =
             Baseline.evaluate(
               Path.join(root, samples),
               Path.join(root, correctness),
               Path.join(root, profiler),
               AlignmentFixtures.spec(),
               sha,
               skill_sha,
               target_snapshot_id: "target-fixture",
               expected_samples: %{
                 sha256: "wrong",
                 size: File.stat!(Path.join(root, samples)).size
               }
             )
  end

  test "rejects JSONL that is not in canonical pair order" do
    root = AlignmentFixtures.temp_dir("pika-baseline-order")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("1", 40)
    skill_sha = String.duplicate("2", 40)

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, sha, skill_sha)

    samples_path = Path.join(root, samples)

    samples_path
    |> File.read!()
    |> String.split("\n", trim: true)
    |> Enum.reverse()
    |> then(&File.write!(samples_path, Enum.join(&1, "\n") <> "\n"))

    assert {:error, {:invalid_pair_indexes, "target_case", "latency_us"}} =
             Baseline.evaluate(
               samples_path,
               Path.join(root, correctness),
               Path.join(root, profiler),
               AlignmentFixtures.spec(),
               sha,
               skill_sha,
               target_snapshot_id: "target-fixture"
             )
  end
end
