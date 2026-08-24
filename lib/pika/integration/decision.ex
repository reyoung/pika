defmodule Pika.Integration.Decision do
  @moduledoc "Applies v2 hard gates and Agent ordinary-Case judgements to Full Case Metrics."

  @type result :: %{
          outcome: :accepted | :rejected,
          reason: String.t(),
          metrics: [map()],
          regressed_case_ids: [non_neg_integer()],
          weighted_aggregates: [map()]
        }

  @spec evaluate([map()], [map()], [map()], [map()], [map()]) ::
          {:ok, result()} | {:error, term()}
  def evaluate(candidate_statistics, best_metrics, cases, metrics, judgements) do
    best_by_key = Map.new(best_metrics, &{{&1.case_id, &1.metric_id}, &1})
    case_by_id = Map.new(cases, &{&1["id"], &1})
    metric_by_id = Map.new(metrics, &{&1["id"], &1})
    judgement_by_key = Map.new(judgements, &{{&1["case_id"], &1["metric_id"]}, &1})

    with :ok <- require_best_coverage(candidate_statistics, best_by_key),
         evaluated <-
           Enum.map(candidate_statistics, fn statistic ->
             compare(
               statistic,
               Map.fetch!(best_by_key, {statistic.case_id, statistic.metric_id}),
               Map.fetch!(case_by_id, statistic.case_id),
               Map.fetch!(metric_by_id, statistic.metric_id),
               judgement_by_key[{statistic.case_id, statistic.metric_id}]
             )
           end),
         :ok <- require_judgements(evaluated),
         aggregates <- weighted_aggregates(evaluated, cases, metrics) do
      {:ok, decide(evaluated, aggregates)}
    end
  end

  defp compare(candidate, best, case_, metric, judgement) do
    improvement =
      case metric["direction"] do
        "maximize" -> (candidate.normalized_ratio - best.normalized_ratio) / best.normalized_ratio
        _minimize -> (best.normalized_ratio - candidate.normalized_ratio) / best.normalized_ratio
      end

    tolerance = max(candidate.noise_tolerance, best.noise_tolerance)

    %{
      case_id: candidate.case_id,
      metric_id: candidate.metric_id,
      role: metric["role"],
      critical: case_["critical"],
      weight: case_["weight"],
      best_relative_improvement: improvement,
      noise_tolerance: tolerance,
      significant_improvement: improvement > tolerance,
      significant_regression: improvement < -tolerance,
      judgement: judgement,
      candidate: candidate,
      best: best
    }
  end

  defp require_best_coverage(candidate_statistics, best_by_key) do
    missing =
      candidate_statistics
      |> Enum.map(&{&1.case_id, &1.metric_id})
      |> Enum.reject(&Map.has_key?(best_by_key, &1))

    if missing == [], do: :ok, else: {:error, {:best_metric_coverage_missing, missing}}
  end

  defp require_judgements(evaluated) do
    missing =
      evaluated
      |> Enum.filter(&(&1.significant_regression and not &1.critical and is_nil(&1.judgement)))
      |> Enum.map(&{&1.case_id, &1.metric_id})

    if missing == [], do: :ok, else: {:error, {:regression_judgements_missing, missing}}
  end

  defp weighted_aggregates(evaluated, cases, metrics) do
    total_weight = Enum.reduce(cases, 0.0, &(&1["weight"] + &2))

    metrics
    |> Enum.filter(&(&1["role"] == "primary"))
    |> Enum.map(fn metric ->
      values = Enum.filter(evaluated, &(&1.metric_id == metric["id"]))

      regression_ratio =
        Enum.reduce(values, 0.0, fn value, sum ->
          sum + value.weight * -value.best_relative_improvement
        end) / total_weight

      %{metric_id: metric["id"], regression_ratio: regression_ratio}
    end)
  end

  defp decide(evaluated, aggregates) do
    critical_regression = Enum.find(evaluated, &(&1.critical and &1.significant_regression))

    agent_regression =
      Enum.find(evaluated, fn metric ->
        metric.significant_regression and not metric.critical and
          get_in(metric, [:judgement, "classification"]) == "regression"
      end)

    aggregate_regression = Enum.find(aggregates, &(&1.regression_ratio >= 0.01))
    improvement? = Enum.any?(evaluated, &(&1.role == "primary" and &1.significant_improvement))

    {outcome, reason} =
      cond do
        critical_regression ->
          {:rejected,
           "critical Case #{critical_regression.case_id}/#{critical_regression.metric_id} regressed"}

        agent_regression ->
          {:rejected, "Integration Agent confirmed an ordinary Case regression"}

        aggregate_regression ->
          {:rejected,
           "weighted aggregate regression for #{aggregate_regression.metric_id} is >= 1%"}

        not improvement? ->
          {:rejected, "no primary Case improved beyond noise"}

        true ->
          {:accepted, "full-set hard gates and Agent judgements passed"}
      end

    %{
      outcome: outcome,
      reason: reason,
      metrics: evaluated,
      regressed_case_ids:
        evaluated
        |> Enum.filter(& &1.significant_regression)
        |> Enum.map(& &1.case_id)
        |> Enum.uniq()
        |> Enum.sort(),
      weighted_aggregates: aggregates
    }
  end
end
