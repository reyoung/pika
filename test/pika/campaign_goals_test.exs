defmodule Pika.CampaignGoalsTest do
  use ExUnit.Case, async: true

  alias Pika.CampaignGoals

  test "Campaign goal uses cumulative Target geomean independently of Attempt acceptance" do
    spec = %{
      "benchmark_cases" => [
        %{"id" => "a", "frequency_weight" => 1},
        %{"id" => "b", "frequency_weight" => 3}
      ],
      "stopping" => %{
        "mode" => "all_goals",
        "metric_goals" => [
          %{
            "metric_id" => "latency_us",
            "aggregation" => "geometric_mean_over_cases",
            "min_improvement_ratio" => 0.30
          }
        ]
      }
    }

    metrics = [
      metric("a", 0.20, 0.02),
      metric("b", 0.40, 0.01)
    ]

    result = CampaignGoals.evaluate(spec, metrics)
    assert result.reached?
    assert [%{complete?: true, value: value, threshold: 0.30}] = result.goals
    assert value > 0.30
  end

  test "goal is not reached without complete Full Case Set coverage" do
    spec = %{
      "benchmark_cases" => [%{"id" => "a"}, %{"id" => "b"}],
      "stopping" => %{
        "mode" => "all_goals",
        "metric_goals" => [
          %{
            "metric_id" => "latency_us",
            "aggregation" => "geometric_mean_over_cases",
            "min_improvement_ratio" => 0.30
          }
        ]
      }
    }

    assert %{reached?: false, goals: [%{complete?: false}]} =
             CampaignGoals.evaluate(spec, [metric("a", 0.50, 0.02)])
  end

  defp metric(case_id, target_improvement, best_improvement) do
    %{
      "case_id" => case_id,
      "metric_id" => "latency_us",
      "direction" => "minimize",
      "target_relative_improvement" => target_improvement,
      "best_relative_improvement" => best_improvement
    }
  end
end
