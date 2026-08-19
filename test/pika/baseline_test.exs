defmodule Pika.BaselineTest do
  use ExUnit.Case, async: true

  alias Pika.Baseline
  alias Pika.Test.AlignmentFixtures

  test "recomputes median, signed pair deltas, MAD and noise floor" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("a", 40)

    records =
      for index <- 0..29 do
        %{
          "schema_version" => 1,
          "measured_sha" => sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "ab", else: "ba"),
          "a" => 10.0,
          "b" => 10.01,
          "valid" => true
        }
      end

    assert {:ok, [metric]} = Baseline.evaluate_records(records, spec, sha)
    assert_in_delta metric.value, 10.005, 1.0e-9
    assert_in_delta metric.pair_delta_median, -0.001, 1.0e-9
    assert metric.noise_tolerance == 0.005
    assert metric.valid_pair_count == 30
  end

  test "requires exact pair indexes and at least 24 valid pairs" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("b", 40)

    records =
      for index <- 0..29 do
        %{
          "schema_version" => 1,
          "measured_sha" => sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "ab", else: "ba"),
          "a" => 10.0,
          "b" => 10.0,
          "valid" => index < 23
        }
      end

    assert {:error, {:insufficient_valid_pairs, "target_case", "latency_us", 23}} =
             Baseline.evaluate_records(records, spec, sha)
  end

  test "rejects Pair records that are not ordered alternately" do
    spec = AlignmentFixtures.spec()
    sha = String.duplicate("c", 40)

    records =
      for index <- 0..29 do
        %{
          "schema_version" => 1,
          "measured_sha" => sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => "ab",
          "a" => 10.0,
          "b" => 10.0,
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
          "schema_version" => 1,
          "measured_sha" => sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "ab", else: "ba"),
          "a" => 10.0,
          "b" => 9.8,
          "valid" => index < 6
        }
      end

    assert {:ok, [%{pair_count: 8, valid_pair_count: 6}]} =
             Baseline.evaluate_records(records, spec, sha)
  end
end
