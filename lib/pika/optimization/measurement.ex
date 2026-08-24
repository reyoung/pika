defmodule Pika.Optimization.Measurement do
  @moduledoc "Recomputes authoritative per-Case Metrics from Target/Candidate Pair JSONL."

  @noise_floor 0.005
  @mad_scale 3.0 * 1.4826

  @type statistic :: %{
          case_id: non_neg_integer(),
          metric_id: String.t(),
          unit: String.t(),
          direction: String.t(),
          target_value: float(),
          development_value: float(),
          normalized_ratio: float(),
          relative_difference: float(),
          mad: float(),
          noise_tolerance: float(),
          valid_pair_count: pos_integer()
        }

  @spec evaluate([map()], [map()], [map()], map()) :: {:ok, [statistic()]} | {:error, term()}
  def evaluate(records, cases, metrics, measurement)
      when is_list(records) and is_list(cases) and is_list(metrics) and is_map(measurement) do
    pair_count = measurement["pair_count"]
    min_valid_pairs = measurement["min_valid_pairs"]
    case_ids = Enum.map(cases, & &1["id"])
    metric_by_id = Map.new(metrics, &{&1["id"], &1})
    expected_keys = for case_id <- case_ids, metric <- metrics, do: {case_id, metric["id"]}
    groups = Enum.group_by(records, &{&1["case_id"], &1["metric_id"]})

    with :ok <- validate_key_coverage(Map.keys(groups), expected_keys),
         {:ok, statistics} <-
           evaluate_groups(expected_keys, groups, metric_by_id, pair_count, min_valid_pairs) do
      {:ok, statistics}
    end
  end

  def evaluate(_records, _cases, _metrics, _measurement), do: {:error, :invalid_measurement_input}

  defp validate_key_coverage(actual, expected) do
    actual = MapSet.new(actual)
    expected = MapSet.new(expected)

    cond do
      not MapSet.subset?(expected, actual) ->
        {:error, {:missing_case_metrics, MapSet.difference(expected, actual) |> Enum.sort()}}

      not MapSet.subset?(actual, expected) ->
        {:error, {:unexpected_case_metrics, MapSet.difference(actual, expected) |> Enum.sort()}}

      true ->
        :ok
    end
  end

  defp evaluate_groups(expected_keys, groups, metric_by_id, pair_count, min_valid_pairs) do
    Enum.reduce_while(expected_keys, {:ok, []}, fn {case_id, metric_id} = key, {:ok, results} ->
      metric = Map.fetch!(metric_by_id, metric_id)

      case evaluate_group(groups[key], case_id, metric, pair_count, min_valid_pairs) do
        {:ok, result} -> {:cont, {:ok, [result | results]}}
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
    |> case do
      {:ok, results} -> {:ok, Enum.reverse(results)}
      error -> error
    end
  end

  defp evaluate_group(records, case_id, metric, pair_count, min_valid_pairs) do
    metric_id = metric["id"]
    records = Enum.sort_by(records, & &1["pair_index"])

    with :ok <- validate_pair_indexes(records, pair_count, case_id, metric_id),
         :ok <- validate_alternation(records, case_id, metric_id),
         valid <- Enum.filter(records, &valid_pair?/1),
         true <-
           length(valid) >= min_valid_pairs ||
             {:error,
              {:insufficient_valid_pairs, case_id, metric_id, length(valid), min_valid_pairs}} do
      target_values = Enum.map(valid, &(&1["target"] * 1.0))
      candidate_values = Enum.map(valid, &(&1["candidate"] * 1.0))
      ratios = Enum.map(valid, &(&1["candidate"] / &1["target"]))
      target = median(target_values)
      candidate = median(candidate_values)
      ratio = median(ratios)
      mad = median(Enum.map(ratios, &abs(&1 - ratio)))
      relative_mad = if ratio == 0.0, do: 0.0, else: mad / abs(ratio)

      relative_difference =
        case metric["direction"] do
          "maximize" -> (candidate - target) / target
          _minimize -> (target - candidate) / target
        end

      {:ok,
       %{
         case_id: case_id,
         metric_id: metric_id,
         unit: metric["unit"],
         direction: metric["direction"],
         target_value: target,
         development_value: candidate,
         normalized_ratio: ratio,
         relative_difference: relative_difference,
         mad: mad,
         noise_tolerance: max(@noise_floor, @mad_scale * relative_mad),
         valid_pair_count: length(valid)
       }}
    else
      {:error, reason} -> {:error, reason}
    end
  end

  defp validate_pair_indexes(records, pair_count, case_id, metric_id) do
    actual = Enum.map(records, & &1["pair_index"])
    expected = if pair_count > 0, do: Enum.to_list(0..(pair_count - 1)), else: []

    if actual == expected,
      do: :ok,
      else: {:error, {:invalid_pair_indexes, case_id, metric_id, actual, expected}}
  end

  defp validate_alternation([], _case_id, _metric_id), do: :ok

  defp validate_alternation([first | _] = records, case_id, metric_id) do
    first_order = first["order"]
    second_order = opposite(first_order)

    valid? =
      second_order != nil and
        Enum.all?(records, fn record ->
          expected = if rem(record["pair_index"], 2) == 0, do: first_order, else: second_order
          record["order"] == expected
        end)

    if valid?, do: :ok, else: {:error, {:invalid_pair_order, case_id, metric_id}}
  end

  defp opposite("target_candidate"), do: "candidate_target"
  defp opposite("candidate_target"), do: "target_candidate"
  defp opposite(_order), do: nil

  defp valid_pair?(record) do
    record["valid"] == true and positive_finite?(record["target"]) and
      positive_finite?(record["candidate"])
  end

  defp positive_finite?(value) when is_integer(value), do: value > 0

  defp positive_finite?(value) when is_float(value), do: value > 0 and value == value

  defp positive_finite?(_value), do: false

  defp median(values) do
    sorted = Enum.sort(values)
    count = length(sorted)
    middle = div(count, 2)

    if rem(count, 2) == 1,
      do: Enum.at(sorted, middle),
      else: (Enum.at(sorted, middle - 1) + Enum.at(sorted, middle)) / 2.0
  end
end
