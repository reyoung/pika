defmodule Pika.Integration.DecisionTest do
  use ExUnit.Case, async: true

  alias Pika.Integration.Decision

  @cases [
    %{"id" => 0, "weight" => 1.0, "critical" => true},
    %{"id" => 1, "weight" => 3.0, "critical" => false}
  ]
  @metrics [
    %{"id" => "latency_us", "direction" => "minimize", "role" => "primary"}
  ]

  test "accepts a full-set improvement within aggregate and noise gates" do
    candidate = [stat(0, 0.90), stat(1, 0.99)]

    assert {:ok, decision} = Decision.evaluate(candidate, best(), @cases, @metrics, [])
    assert decision.outcome == :accepted
    assert decision.regressed_case_ids == []
    assert hd(decision.weighted_aggregates).regression_ratio < 0.01
  end

  test "rejects a critical regression and missing primary improvement" do
    candidate = [stat(0, 1.02), stat(1, 1.00)]

    assert {:ok, decision} = Decision.evaluate(candidate, best(), @cases, @metrics, [])
    assert decision.outcome == :rejected
    assert decision.reason =~ "critical Case"

    candidate = [stat(0, 1.00), stat(1, 1.00)]
    assert {:ok, decision} = Decision.evaluate(candidate, best(), @cases, @metrics, [])
    assert decision.reason == "no primary Case improved beyond noise"
  end

  test "requires an Agent judgement for an ordinary significant regression" do
    candidate = [stat(0, 0.90), stat(1, 1.02)]

    assert {:error, {:regression_judgements_missing, [{1, "latency_us"}]}} =
             Decision.evaluate(candidate, best(), @cases, @metrics, [])

    noise = [%{"case_id" => 1, "metric_id" => "latency_us", "classification" => "noise"}]
    assert {:ok, accepted} = Decision.evaluate(candidate, best(), @cases, @metrics, noise)
    assert accepted.outcome == :accepted

    regression = [
      %{"case_id" => 1, "metric_id" => "latency_us", "classification" => "regression"}
    ]

    assert {:ok, rejected} = Decision.evaluate(candidate, best(), @cases, @metrics, regression)
    assert rejected.outcome == :rejected
    assert rejected.reason =~ "Agent confirmed"
  end

  test "rejects a weighted aggregate regression of one percent or more" do
    candidate = [stat(0, 0.99), stat(1, 1.02)]
    judgements = [%{"case_id" => 1, "metric_id" => "latency_us", "classification" => "noise"}]

    assert {:ok, decision} = Decision.evaluate(candidate, best(), @cases, @metrics, judgements)
    assert decision.outcome == :rejected
    assert decision.reason =~ ">= 1%"
  end

  test "rejects any guard Metric regression beyond noise without an Agent judgement" do
    metrics =
      @metrics ++ [%{"id" => "accuracy", "direction" => "maximize", "role" => "guard"}]

    candidate = [
      stat(0, 0.90),
      stat(1, 0.99),
      stat(0, 1.00, "accuracy"),
      stat(1, 0.98, "accuracy")
    ]

    best = best() ++ [best_stat(0, "accuracy"), best_stat(1, "accuracy")]

    assert {:ok, decision} = Decision.evaluate(candidate, best, @cases, metrics, [])
    assert decision.outcome == :rejected
    assert decision.reason == "guard Metric accuracy regressed on Case 1"
    assert Enum.map(decision.weighted_aggregates, & &1.metric_id) == ["latency_us"]
  end

  test "accepts a guard Metric change within noise when a primary Metric improves" do
    metrics =
      @metrics ++ [%{"id" => "accuracy", "direction" => "maximize", "role" => "guard"}]

    candidate = [
      stat(0, 0.90),
      stat(1, 0.99),
      stat(0, 0.997, "accuracy"),
      stat(1, 1.00, "accuracy")
    ]

    best = best() ++ [best_stat(0, "accuracy"), best_stat(1, "accuracy")]

    assert {:ok, decision} = Decision.evaluate(candidate, best, @cases, metrics, [])
    assert decision.outcome == :accepted
  end

  test "does not count a guard Metric improvement as the required primary improvement" do
    metrics =
      @metrics ++ [%{"id" => "accuracy", "direction" => "maximize", "role" => "guard"}]

    candidate = [
      stat(0, 1.00),
      stat(1, 1.00),
      stat(0, 1.02, "accuracy"),
      stat(1, 1.02, "accuracy")
    ]

    best = best() ++ [best_stat(0, "accuracy"), best_stat(1, "accuracy")]

    assert {:ok, decision} = Decision.evaluate(candidate, best, @cases, metrics, [])
    assert decision.outcome == :rejected
    assert decision.reason == "no primary Case improved beyond noise"
  end

  defp best do
    [best_stat(0), best_stat(1)]
  end

  defp best_stat(case_id, metric_id \\ "latency_us") do
    %{case_id: case_id, metric_id: metric_id, normalized_ratio: 1.0, noise_tolerance: 0.005}
  end

  defp stat(case_id, ratio, metric_id \\ "latency_us") do
    %{
      case_id: case_id,
      metric_id: metric_id,
      normalized_ratio: ratio,
      noise_tolerance: 0.005
    }
  end
end
