defmodule Pika.Optimization.MeasurementTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.Measurement

  @cases [
    %{"id" => 0, "weight" => 1.0, "critical" => true},
    %{"id" => 1, "weight" => 2.0, "critical" => false}
  ]
  @metrics [
    %{"id" => "latency_us", "unit" => "us", "direction" => "minimize", "role" => "primary"},
    %{
      "id" => "bandwidth_gbps",
      "unit" => "GB/s",
      "direction" => "maximize",
      "role" => "informational"
    }
  ]
  @protocol %{"pair_count" => 3, "min_valid_pairs" => 2}

  test "recomputes ordered per-Case Metrics and empirical noise" do
    records = full_records()

    assert {:ok, statistics} = Measurement.evaluate(records, @cases, @metrics, @protocol)

    assert Enum.map(statistics, &{&1.case_id, &1.metric_id}) == [
             {0, "latency_us"},
             {0, "bandwidth_gbps"},
             {1, "latency_us"},
             {1, "bandwidth_gbps"}
           ]

    latency = Enum.find(statistics, &(&1.case_id == 0 and &1.metric_id == "latency_us"))
    assert_in_delta latency.target_value, 10.1, 1.0e-9
    assert_in_delta latency.development_value, 12.1, 1.0e-9
    assert_in_delta latency.relative_difference, (10.1 - 12.1) / 10.1, 1.0e-9
    assert latency.valid_pair_count == 3
    assert latency.noise_tolerance >= 0.005

    bandwidth =
      Enum.find(statistics, &(&1.case_id == 0 and &1.metric_id == "bandwidth_gbps"))

    assert_in_delta bandwidth.relative_difference, (111.0 - 101.0) / 101.0, 1.0e-9
  end

  test "rejects missing Case/Metric coverage" do
    records =
      Enum.reject(full_records(), &(&1["case_id"] == 1 and &1["metric_id"] == "latency_us"))

    assert {:error, {:missing_case_metrics, [{1, "latency_us"}]}} =
             Measurement.evaluate(records, @cases, @metrics, @protocol)
  end

  test "rejects noncanonical Pair indexes and ordering" do
    records = full_records()

    records =
      Enum.reject(
        records,
        &(&1["case_id"] == 0 and &1["metric_id"] == "latency_us" and &1["pair_index"] == 1)
      )

    assert {:error, {:invalid_pair_indexes, 0, "latency_us", [0, 2], [0, 1, 2]}} =
             Measurement.evaluate(records, @cases, @metrics, @protocol)

    records =
      full_records()
      |> Enum.map(fn record ->
        if record["case_id"] == 0 and record["metric_id"] == "latency_us" and
             record["pair_index"] == 1,
           do: %{record | "order" => "target_candidate"},
           else: record
      end)

    assert {:error, {:invalid_pair_order, 0, "latency_us"}} =
             Measurement.evaluate(records, @cases, @metrics, @protocol)
  end

  test "requires the configured number of valid Pairs" do
    records =
      full_records()
      |> Enum.map(fn record ->
        if record["case_id"] == 0 and record["metric_id"] == "latency_us" and
             record["pair_index"] in [1, 2],
           do: %{record | "valid" => false},
           else: record
      end)

    assert {:error, {:insufficient_valid_pairs, 0, "latency_us", 1, 2}} =
             Measurement.evaluate(records, @cases, @metrics, @protocol)
  end

  defp full_records do
    for case_id <- [0, 1],
        metric_id <- ["latency_us", "bandwidth_gbps"],
        pair_index <- 0..2 do
      values = values(metric_id, case_id, pair_index)

      %{
        "case_id" => case_id,
        "metric_id" => metric_id,
        "pair_index" => pair_index,
        "order" => if(rem(pair_index, 2) == 0, do: "target_candidate", else: "candidate_target"),
        "target" => values.target,
        "candidate" => values.candidate,
        "valid" => true,
        "error" => nil
      }
    end
  end

  defp values("latency_us", case_id, pair_index),
    do: %{target: 10.0 + case_id + pair_index * 0.1, candidate: 12.0 + case_id + pair_index * 0.1}

  defp values("bandwidth_gbps", case_id, pair_index),
    do: %{target: 100.0 + case_id + pair_index, candidate: 110.0 + case_id + pair_index}
end
