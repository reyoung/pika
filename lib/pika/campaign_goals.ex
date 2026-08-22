defmodule Pika.CampaignGoals do
  @moduledoc false

  def evaluate(spec, metrics) when is_map(spec) and is_list(metrics) do
    stopping = spec["stopping"] || %{}
    goals = stopping["metric_goals"] || []
    results = Enum.map(goals, &evaluate_goal(&1, spec, metrics))

    reached? =
      case {stopping["mode"] || "all_goals", results} do
        {_mode, []} -> false
        {"any_goal", results} -> Enum.any?(results, & &1.reached?)
        {"all_goals", results} -> Enum.all?(results, & &1.reached?)
        {_unknown, _results} -> false
      end

    %{reached?: reached?, goals: results}
  end

  defp evaluate_goal(goal, spec, metrics) do
    metric_id = goal["metric_id"]
    expected_cases = Enum.map(spec["benchmark_cases"] || [], & &1["id"])
    weights = Map.new(spec["benchmark_cases"] || [], &{&1["id"], &1["frequency_weight"] || 1.0})

    observed =
      Enum.filter(metrics, fn metric ->
        field(metric, "metric_id") == metric_id and field(metric, "case_id") in expected_cases
      end)

    complete? =
      expected_cases != [] and
        MapSet.new(Enum.map(observed, &field(&1, "case_id"))) == MapSet.new(expected_cases)

    value =
      if complete? and geometric_mean?(goal["aggregation"]),
        do: aggregate_improvement(observed, weights),
        else: nil

    threshold = goal["min_improvement_ratio"]

    %{
      metric_id: metric_id,
      aggregation: goal["aggregation"],
      value: value,
      threshold: threshold,
      complete?: complete?,
      reached?: complete? and is_number(value) and is_number(threshold) and value >= threshold
    }
  end

  defp aggregate_improvement(metrics, weights) do
    aggregate =
      Enum.reduce_while(metrics, {0.0, 0.0}, fn metric, {log_sum, weight_sum} ->
        improvement = field(metric, "target_relative_improvement")
        direction = field(metric, "direction")
        weight = Map.get(weights, field(metric, "case_id"), 1.0)

        if is_number(improvement) and is_number(weight) and weight > 0 do
          factor = if direction == "maximize", do: 1.0 + improvement, else: 1.0 - improvement

          if factor > 0,
            do: {:cont, {log_sum + weight * :math.log(factor), weight_sum + weight}},
            else: {:halt, :invalid}
        else
          {:halt, :invalid}
        end
      end)

    case aggregate do
      {weighted_log_sum, weight_sum} ->
        factor = :math.exp(weighted_log_sum / weight_sum)
        direction = metrics |> hd() |> field("direction")
        if direction == "maximize", do: factor - 1.0, else: 1.0 - factor

      :invalid ->
        nil
    end
  end

  defp geometric_mean?(value) when is_binary(value),
    do: String.starts_with?(value, "geometric_mean")

  defp geometric_mean?(_value), do: false

  defp field(map, key), do: map[key] || map[String.to_existing_atom(key)]
end
