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
      benchmark: %{"pair_count" => 7, "min_valid_pairs" => 5},
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

  test "recomputes the user-sized formal result from raw alternating records", context do
    write_pairs(context.samples, context.context, 7, fn index ->
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
    assert metric.pair_count == 7
    assert metric.valid_pair_count == 7
    assert_in_delta metric.improvement_ratio, 0.02, 1.0e-12
    assert_in_delta metric.value / metric.baseline_value, 0.98, 1.0e-9
    assert metric.noise_tolerance >= 0.005
  end

  test "rejects the wrong pair count, non-alternating order, and failed correctness", context do
    write_pairs(context.samples, context.context, 6, fn _index -> {10.0, 9.8, true} end)

    assert {:error, {:wrong_pair_count, "target_case", "latency_us", 6, 7}} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )

    write_pairs(
      context.samples,
      context.context,
      7,
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

  test "requires the user-specified minimum valid pairs", context do
    write_pairs(context.samples, context.context, 7, fn index ->
      {10.0, 9.8, index < 4}
    end)

    assert {:error, {:insufficient_valid_pairs, "target_case", "latency_us", 4}} =
             Measurement.evaluate_iteration(
               context.samples,
               context.correctness,
               context.context
             )
  end

  test "uses the Campaign formal pair protocol without a global pair constant", context do
    configured = %{
      context.context
      | benchmark: %{"pair_count" => 8, "min_valid_pairs" => 6}
    }

    write_pairs(context.samples, configured, 8, fn index -> {10.0, 9.8, index < 6} end)

    assert {:ok, [%{pair_count: 8, valid_pair_count: 6}]} =
             Measurement.evaluate_iteration(context.samples, context.correctness, configured)

    write_pairs(context.samples, configured, 7, fn _index -> {10.0, 9.8, true} end)

    assert {:error, {:wrong_pair_count, "target_case", "latency_us", 7, 8}} =
             Measurement.evaluate_iteration(context.samples, context.correctness, configured)
  end

  test "integration escalates an invalid screen to the user-sized formal result", context do
    full = Path.join(context.root, "full.jsonl")

    write_pairs(context.samples, context.context, 5, fn index ->
      {10.0, 9.8, index < 3}
    end)

    write_pairs(full, context.context, 7, fn _index -> {10.0, 9.8, true} end)

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

    assert [%{source: "integration_full", pair_count: 7, valid_pair_count: 7}] =
             result.metrics
  end

  test "integration escalation uses the Campaign formal pair protocol", context do
    full = Path.join(context.root, "configured-full.jsonl")

    configured = %{
      context.context
      | benchmark: %{"pair_count" => 8, "min_valid_pairs" => 6}
    }

    write_pairs(context.samples, configured, 5, fn index -> {10.0, 9.8, index < 3} end)
    write_pairs(full, configured, 8, fn index -> {10.0, 9.8, index < 6} end)

    assert {:ok, result} =
             Measurement.evaluate_integration(
               context.samples,
               full,
               context.correctness,
               configured,
               %{}
             )

    assert [%{source: "integration_full", pair_count: 8, valid_pair_count: 6}] =
             result.metrics
  end

  test "an informational metric is still rejected after confirmed regression", context do
    full = Path.join(context.root, "full.jsonl")

    informational = %{
      context.context
      | metrics: put_in(context.context.metrics, [Access.at(0), "role"], "informational")
    }

    write_pairs(context.samples, informational, 5, fn _index -> {10.0, 10.2, true} end)
    write_pairs(full, informational, 7, fn _index -> {10.0, 10.2, true} end)

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
