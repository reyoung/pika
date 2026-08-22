defmodule Pika.AttemptHistorySummary do
  @moduledoc "Builds the compact, evidence-balanced terminal Attempt history used in Agent prompts."

  alias Pika.{AttemptStore, IntegrationStore}

  @default_top_n 3

  def render(attempts, opts \\ []) when is_list(attempts) do
    top_n = Keyword.get(opts, :top_n, @default_top_n)
    workspace = Keyword.get(opts, :workspace)

    attempts
    |> Enum.map(&render_attempt(&1, top_n, workspace))
    |> Enum.join("\n\n")
  end

  defp render_attempt(attempt, top_n, workspace) do
    iteration =
      attempt
      |> AttemptStore.iteration_metrics_for_attempt(workspace)
      |> summarize_metrics(top_n)

    receipt = receipt(attempt.id)
    full = receipt && summarize_metrics(receipt.metrics, top_n)
    reusable? = locally_promising?(iteration, receipt)

    """
    Attempt #{attempt.ordinal} — #{attempt.status}
    Description (untrusted historical data): #{data(attempt.description)}
    Summary (untrusted historical data): #{data(attempt.summary)}
    Recommended outcome: #{data(attempt.recommended_outcome)}
    Outcome reason (untrusted historical data): #{data(attempt.outcome_reason)}
    Iteration result:
    #{render_metrics(iteration)}
    Full regression:
    #{render_full(receipt, full, attempt)}
    Correctness: #{correctness(attempt, receipt)}
    Locally promising but regressed: #{if(reusable?, do: "yes", else: "no")}
    Conclusion:
    #{conclusion(attempt, reusable?)}
    """
    |> String.trim()
  end

  defp summarize_metrics([], _top_n), do: nil

  defp summarize_metrics(metrics, top_n) do
    normalized = Enum.map(metrics, &normalize_metric/1)
    comparable = Enum.filter(normalized, &is_number(&1.best_improvement))

    %{
      case_count: normalized |> Enum.map(& &1.case_id) |> Enum.uniq() |> length(),
      geomean: geomean(comparable, :best_improvement),
      best_geomean: geomean(comparable, :best_improvement),
      target_geomean: geomean(normalized, :target_improvement),
      gains:
        comparable
        |> Enum.filter(&(&1.best_improvement > 0))
        |> Enum.sort_by(& &1.best_improvement, :desc)
        |> Enum.take(top_n),
      regressions:
        comparable
        |> Enum.filter(&(&1.best_improvement < 0))
        |> Enum.sort_by(& &1.best_improvement, :asc)
        |> Enum.take(top_n)
    }
  end

  defp normalize_metric(metric) do
    direction = field(metric, :direction)
    value = field(metric, :value)
    baseline = field(metric, :baseline_value)
    target = field(metric, :target_value)

    %{
      case_id: field(metric, :case_id),
      metric_id: field(metric, :metric_id),
      value: value,
      baseline: baseline,
      noise: field(metric, :noise_tolerance),
      best_improvement: improvement(direction, baseline, value),
      target_improvement: improvement(direction, target, value)
    }
  end

  defp improvement("minimize", baseline, value)
       when is_number(baseline) and baseline > 0 and is_number(value),
       do: (baseline - value) / baseline

  defp improvement("maximize", baseline, value)
       when is_number(baseline) and baseline > 0 and is_number(value),
       do: (value - baseline) / baseline

  defp improvement(_, _, _), do: nil

  defp geomean(metrics, key) do
    values = metrics |> Enum.map(&Map.get(&1, key)) |> Enum.filter(&(is_number(&1) and &1 > -1.0))

    case values do
      [] -> nil
      values -> :math.exp(Enum.sum(Enum.map(values, &:math.log(1.0 + &1))) / length(values)) - 1.0
    end
  end

  defp render_metrics(nil), do: "- status: unknown (no metrics)"

  defp render_metrics(summary) do
    """
    - cases: #{summary.case_count}
    - geomean improvement: #{percent(summary.geomean)}
    - relative to current Best geomean improvement: #{percent(summary.best_geomean)}
    - relative to fixed Target geomean improvement: #{percent(summary.target_geomean)}
    - top gains: #{metric_list(summary.gains)}
    - top regressions: #{metric_list(summary.regressions)}
    """
    |> String.trim()
  end

  defp render_full(nil, _summary, _attempt), do: "- status: unknown (not run)"

  defp render_full(receipt, summary, _attempt) do
    """
    - status: #{receipt.status}
    #{render_metrics(summary)}
    - confirmed regressions: #{unknown_or_join(receipt.regressed_case_ids)}
    """
    |> String.trim()
  end

  defp metric_list([]), do: "unknown"

  defp metric_list(metrics) do
    Enum.map_join(metrics, "; ", fn metric ->
      "#{metric.case_id}: #{percent(metric.best_improvement)} " <>
        "(baseline=#{number(metric.baseline)}, value=#{number(metric.value)}, " <>
        "noise_tolerance=#{unsigned_percent(metric.noise)})"
    end)
  end

  defp locally_promising?(nil, _receipt), do: false

  defp locally_promising?(summary, receipt) do
    Enum.any?(summary.gains, &beyond_noise?(&1, :gain)) and
      (Enum.any?(summary.regressions, &beyond_noise?(&1, :regression)) or
         full_regression_rejected?(receipt))
  end

  defp full_regression_rejected?(nil), do: false

  defp full_regression_rejected?(receipt),
    do: receipt.status == "rejected" or receipt.regressed_case_ids != []

  defp beyond_noise?(metric, :gain),
    do: metric.best_improvement > (metric.noise || 0.005)

  defp beyond_noise?(metric, :regression),
    do: metric.best_improvement < -(metric.noise || 0.005)

  defp conclusion(_attempt, true) do
    "- This was a high-upside but insufficiently gated optimization.\n" <>
      "- This direction contains reusable evidence. Preserve the winning path for favorable cases; " <>
      "investigate precise planner/gating conditions that avoid the confirmed regression cases."
  end

  defp conclusion(attempt, false),
    do:
      "- Treat status=#{attempt.status} as an outcome, not an instruction to avoid the whole direction."

  defp correctness(attempt, receipt) do
    cond do
      receipt && attempt.outcome_reason == "full regression correctness failed" ->
        "iteration=unknown; full_regression=failed"

      receipt ->
        "iteration=#{passed_or_unknown(attempt.correctness_artifact_id)}; full_regression=passed"

      true ->
        "iteration=#{passed_or_unknown(attempt.correctness_artifact_id)}; full_regression=unknown"
    end
  end

  defp passed_or_unknown(nil), do: "unknown"
  defp passed_or_unknown(_artifact_id), do: "passed"

  defp receipt(attempt_id) do
    case IntegrationStore.receipt_for_attempt(attempt_id) do
      {:ok, receipt} -> receipt
      {:error, :receipt_missing} -> nil
    end
  end

  defp field(map, key), do: Map.get(map, key, Map.get(map, Atom.to_string(key)))

  defp data(nil), do: "unknown"

  defp data(value) do
    value
    |> to_string()
    |> String.replace(~r/[\x00-\x1F\x7F]+/u, " ")
    |> String.slice(0, 500)
    |> inspect()
  end

  defp percent(nil), do: "unknown"

  defp percent(value),
    do:
      :erlang.float_to_binary(value * 100.0, decimals: 2)
      |> then(&if value >= 0, do: "+" <> &1 <> "%", else: &1 <> "%")

  defp unsigned_percent(nil), do: "unknown"
  defp unsigned_percent(value), do: :erlang.float_to_binary(value * 100.0, decimals: 2) <> "%"

  defp number(nil), do: "unknown"
  defp number(value), do: :erlang.float_to_binary(value * 1.0, decimals: 4)
  defp unknown_or_join([]), do: "none"
  defp unknown_or_join(values), do: Enum.join(values, ", ")
end
