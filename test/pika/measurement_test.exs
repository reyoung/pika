defmodule Pika.MeasurementTest do
  use ExUnit.Case, async: true

  alias Pika.Measurement
  alias Pika.Test.AlignmentFixtures

  setup do
    root = AlignmentFixtures.temp_dir("pika-measurement")
    base_sha = String.duplicate("a", 40)
    candidate_sha = String.duplicate("b", 40)
    samples = Path.join(root, "pairs.jsonl")
    correctness = Path.join(root, "correctness.json")

    context = %{
      base_sha: base_sha,
      candidate_sha: candidate_sha,
      case_ids: ["target_case"],
      metrics: [
        %{
          "id" => "latency_us",
          "unit" => "us",
          "direction" => "minimize",
          "role" => "target"
        }
      ]
    }

    File.write!(
      correctness,
      Jason.encode!(%{
        "candidate_sha" => candidate_sha,
        "cases" => [%{"case_id" => "target_case", "passed" => true}]
      })
    )

    %{root: root, samples: samples, correctness: correctness, context: context}
  end

  test "recomputes the formal 30-pair result from raw alternating records", context do
    write_pairs(context.samples, context.context, 30, fn index ->
      baseline = 10.0 + index / 10_000
      {baseline, baseline * 0.98, true}
    end)

    assert {:ok, [metric]} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )

    assert metric.case_id == "target_case"
    assert metric.metric_id == "latency_us"
    assert metric.pair_count == 30
    assert metric.valid_pair_count == 30
    assert_in_delta metric.improvement_ratio, 0.02, 1.0e-12
    assert_in_delta metric.value / metric.baseline_value, 0.98, 1.0e-9
    assert metric.noise_tolerance >= 0.005
  end

  test "rejects the wrong pair count, non-alternating order, and failed correctness", context do
    write_pairs(context.samples, context.context, 29, fn _index -> {10.0, 9.8, true} end)

    assert {:error, {:wrong_pair_count, "target_case", "latency_us", 29, 30}} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )

    write_pairs(
      context.samples,
      context.context,
      30,
      fn _index -> {10.0, 9.8, true} end,
      fn _index -> "bc" end
    )

    assert {:error, {:non_alternating_pair_orders, "target_case", "latency_us"}} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )

    File.write!(
      context.correctness,
      Jason.encode!(%{
        "candidate_sha" => context.context.candidate_sha,
        "cases" => [%{"case_id" => "target_case", "passed" => false}]
      })
    )

    assert {:error, :correctness_failed} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )
  end

  test "requires at least 24 valid pairs", context do
    write_pairs(context.samples, context.context, 30, fn index ->
      {10.0, 9.8, index < 23}
    end)

    assert {:error, {:insufficient_valid_pairs, "target_case", "latency_us", 23}} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )
  end

  test "integration escalates an invalid screen to one independent 30-pair result", context do
    full = Path.join(context.root, "full.jsonl")

    write_pairs(context.samples, context.context, 5, fn index ->
      {10.0, 9.8, index < 3}
    end)

    write_pairs(full, context.context, 30, fn _index -> {10.0, 9.8, true} end)

    assert {:ok, result} =
             Measurement.evaluate_integration(
               context.samples,
               full,
               context.correctness,
               context.context,
               %{}
             )

    assert result.escalated == [{"target_case", "latency_us"}]
    assert result.regressions == []

    assert [%{source: "integration_full", pair_count: 30, valid_pair_count: 30}] =
             result.metrics
  end

  test "an informational metric is still rejected after confirmed regression", context do
    full = Path.join(context.root, "full.jsonl")

    informational = %{
      context.context
      | metrics: put_in(context.context.metrics, [Access.at(0), "role"], "informational")
    }

    write_pairs(context.samples, informational, 5, fn _index -> {10.0, 10.2, true} end)
    write_pairs(full, informational, 30, fn _index -> {10.0, 10.2, true} end)

    assert {:ok, result} =
             Measurement.evaluate_integration(
               context.samples,
               full,
               context.correctness,
               informational,
               %{}
             )

    assert result.escalated == [{"target_case", "latency_us"}]
    assert result.regressions == [{"target_case", "latency_us"}]
    assert [%{role: "informational", source: "integration_full"}] = result.metrics
  end

  defp write_pairs(path, context, count, values, order \\ nil) do
    records =
      for index <- 0..(count - 1) do
        {baseline, candidate, valid} = values.(index)

        %{
          "schema_version" => 1,
          "base_sha" => context.base_sha,
          "candidate_sha" => context.candidate_sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" =>
            if(order, do: order.(index), else: if(rem(index, 2) == 0, do: "bc", else: "cb")),
          "baseline" => baseline,
          "candidate" => candidate,
          "valid" => valid
        }
      end

    File.write!(path, Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n")
  end
end
