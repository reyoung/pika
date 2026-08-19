defmodule Pika.Measurement do
  @moduledoc false

  @iteration_pairs 30
  @iteration_min_valid 24
  @screen_pairs 5
  @screen_min_valid 4

  def evaluate_iteration(samples_path, correctness_path, context) do
    with {:ok, records} <- read_jsonl(samples_path),
         :ok <- validate_correctness(correctness_path, context.case_ids, context.candidate_sha),
         {:ok, metrics} <-
           evaluate_records(records, context,
             expected_pairs: @iteration_pairs,
             min_valid: @iteration_min_valid,
             source: "iteration",
             allow_insufficient: false
           ) do
      {:ok, metrics}
    end
  end

  def evaluate_integration(screening_path, full_path, correctness_path, context, best_metrics) do
    with {:ok, records} <- read_jsonl(screening_path),
         :ok <- validate_correctness(correctness_path, context.case_ids, context.candidate_sha),
         {:ok, screening} <-
           evaluate_records(records, context,
             expected_pairs: @screen_pairs,
             min_valid: @screen_min_valid,
             source: "integration_screen",
             allow_insufficient: true
           ),
         escalated <- escalation_keys(screening, best_metrics),
         {:ok, full} <- evaluate_escalations(full_path, escalated, context),
         {:ok, final, regressions} <-
           combine_integration(screening, full, escalated, best_metrics) do
      {:ok, %{metrics: final, escalated: escalated, regressions: regressions}}
    end
  end

  def evaluate_records(records, context, opts) when is_list(records) and is_map(context) do
    expected_pairs = Keyword.fetch!(opts, :expected_pairs)
    min_valid = Keyword.fetch!(opts, :min_valid)
    source = Keyword.fetch!(opts, :source)
    allow_insufficient = Keyword.get(opts, :allow_insufficient, false)
    metric_defs = Map.new(context.metrics, &{&1["id"], &1})

    expected_keys =
      Keyword.get_lazy(opts, :expected_keys, fn ->
        for case_id <- context.case_ids, metric <- context.metrics, do: {case_id, metric["id"]}
      end)

    grouped = Enum.group_by(records, &{&1["case_id"], &1["metric_id"]})

    with :ok <- validate_record_shapes(records, context),
         :ok <- validate_expected_keys(grouped, expected_keys) do
      expected_keys
      |> Enum.map(fn {_case_id, metric_id} = key ->
        evaluate_group(
          key,
          Map.fetch!(grouped, key),
          Map.fetch!(metric_defs, metric_id),
          expected_pairs,
          min_valid,
          source,
          allow_insufficient
        )
      end)
      |> collect()
    end
  end

  defp evaluate_escalations(_path, [], _context), do: {:ok, []}
  defp evaluate_escalations(nil, keys, _context), do: {:error, {:missing_full_escalation, keys}}

  defp evaluate_escalations(path, keys, context) do
    with {:ok, records} <- read_jsonl(path) do
      expected = MapSet.new(keys)
      records = Enum.filter(records, &MapSet.member?(expected, {&1["case_id"], &1["metric_id"]}))

      case evaluate_records(records, context,
             expected_pairs: @iteration_pairs,
             min_valid: @iteration_min_valid,
             source: "integration_full",
             allow_insufficient: false,
             expected_keys: keys
           ) do
        {:ok, metrics} ->
          actual = MapSet.new(Enum.map(metrics, &{&1.case_id, &1.metric_id}))

          if actual == expected,
            do: {:ok, metrics},
            else: {:error, {:invalid_full_escalation_coverage, MapSet.to_list(actual)}}

        error ->
          error
      end
    end
  end

  defp combine_integration(screening, full, escalated, best_metrics) do
    full_by_key = Map.new(full, &{{&1.case_id, &1.metric_id}, &1})
    escalated = MapSet.new(escalated)

    final =
      Enum.map(screening, fn metric ->
        key = {metric.case_id, metric.metric_id}
        if MapSet.member?(escalated, key), do: Map.fetch!(full_by_key, key), else: metric
      end)

    regressions =
      Enum.filter(final, fn metric ->
        metric.insufficient? or confirmed_regression?(metric, best_metrics)
      end)
      |> Enum.map(&{&1.case_id, &1.metric_id})

    {:ok, final, regressions}
  end

  defp escalation_keys(screening, best_metrics) do
    screening
    |> Enum.filter(&(&1.insufficient? or confirmed_regression?(&1, best_metrics)))
    |> Enum.map(&{&1.case_id, &1.metric_id})
  end

  defp confirmed_regression?(metric, best_metrics) do
    tolerance =
      case Map.fetch(best_metrics, {metric.case_id, metric.metric_id}) do
        {:ok, best} -> best.noise_tolerance || best[:noise_tolerance] || 0.005
        :error -> metric.noise_tolerance
      end

    is_nil(metric.improvement_ratio) or metric.improvement_ratio < -tolerance
  end

  defp evaluate_group(
         {case_id, metric_id},
         records,
         definition,
         expected_pairs,
         min_valid,
         source,
         allow_insufficient
       ) do
    indexes = Enum.map(records, & &1["pair_index"])

    cond do
      length(records) != expected_pairs ->
        {:error, {:wrong_pair_count, case_id, metric_id, length(records), expected_pairs}}

      Enum.sort(indexes) != Enum.to_list(0..(expected_pairs - 1)) ->
        {:error, {:invalid_pair_indexes, case_id, metric_id}}

      not alternating_orders?(records) ->
        {:error, {:non_alternating_pair_orders, case_id, metric_id}}

      true ->
        valid = Enum.filter(records, &valid_pair?/1)

        cond do
          length(valid) < min_valid and not allow_insufficient ->
            {:error, {:insufficient_valid_pairs, case_id, metric_id, length(valid)}}

          valid == [] ->
            {:ok,
             metric_result(
               case_id,
               metric_id,
               definition,
               source,
               expected_pairs,
               0,
               nil,
               nil,
               nil,
               nil
             )}

          true ->
            improvements = Enum.map(valid, &improvement(&1, definition["direction"]))
            center = median(improvements)
            mad = improvements |> Enum.map(&abs(&1 - center)) |> median()

            {:ok,
             metric_result(
               case_id,
               metric_id,
               definition,
               source,
               expected_pairs,
               length(valid),
               median(Enum.map(valid, & &1["candidate"])),
               median(Enum.map(valid, & &1["baseline"])),
               center,
               mad
             )}
        end
    end
  end

  defp metric_result(
         case_id,
         metric_id,
         definition,
         source,
         pair_count,
         valid_count,
         value,
         baseline_value,
         improvement_ratio,
         mad
       ) do
    %{
      case_id: case_id,
      metric_id: metric_id,
      unit: definition["unit"],
      direction: definition["direction"],
      role: definition["role"],
      value: value,
      baseline_value: baseline_value,
      improvement_ratio: improvement_ratio,
      mad: mad,
      noise_tolerance: if(is_nil(mad), do: 0.005, else: max(0.005, 3.0 * 1.4826 * mad)),
      pair_count: pair_count,
      valid_pair_count: valid_count,
      insufficient?:
        valid_count <
          if(pair_count == @screen_pairs, do: @screen_min_valid, else: @iteration_min_valid),
      source: source
    }
  end

  defp validate_record_shapes(records, context) do
    valid? =
      Enum.all?(records, fn record ->
        record["schema_version"] == 1 and record["base_sha"] == context.base_sha and
          record["candidate_sha"] == context.candidate_sha and
          record["order"] in ["bc", "cb"] and is_integer(record["pair_index"])
      end)

    if valid?, do: :ok, else: {:error, :invalid_sample_record}
  end

  defp validate_expected_keys(grouped, expected_keys) do
    if Enum.sort(Map.keys(grouped)) == Enum.sort(expected_keys),
      do: :ok,
      else: {:error, {:invalid_case_metric_coverage, Enum.sort(Map.keys(grouped))}}
  end

  defp validate_correctness(path, expected_case_ids, candidate_sha) do
    with {:ok, body} <- File.read(path),
         {:ok, report} <- Jason.decode(body),
         true <- report["candidate_sha"] == candidate_sha,
         cases when is_list(cases) <- report["cases"],
         true <- Enum.sort(Enum.map(cases, & &1["case_id"])) == Enum.sort(expected_case_ids),
         true <- Enum.all?(cases, &(&1["passed"] == true)) do
      :ok
    else
      _ -> {:error, :correctness_failed}
    end
  end

  defp alternating_orders?(records) do
    records
    |> Enum.sort_by(& &1["pair_index"])
    |> Enum.with_index()
    |> Enum.all?(fn {record, index} ->
      record["order"] == if(rem(index, 2) == 0, do: "bc", else: "cb")
    end)
  end

  defp valid_pair?(record) do
    record["valid"] == true and finite_positive?(record["baseline"]) and
      finite_positive?(record["candidate"])
  end

  defp improvement(record, "minimize"),
    do: (record["baseline"] - record["candidate"]) / record["baseline"]

  defp improvement(record, "maximize"),
    do: (record["candidate"] - record["baseline"]) / record["baseline"]

  defp read_jsonl(path) do
    path
    |> File.stream!(:line, [])
    |> Enum.reduce_while({:ok, []}, fn line, {:ok, acc} ->
      case Jason.decode(line) do
        {:ok, record} -> {:cont, {:ok, [record | acc]}}
        _ -> {:halt, {:error, :invalid_samples_jsonl}}
      end
    end)
    |> case do
      {:ok, []} -> {:error, :empty_samples}
      {:ok, records} -> {:ok, Enum.reverse(records)}
      error -> error
    end
  rescue
    File.Error -> {:error, :samples_not_found}
  end

  defp collect(results) do
    case Enum.find(results, &match?({:error, _}, &1)) do
      nil -> {:ok, Enum.map(results, fn {:ok, result} -> result end)}
      error -> error
    end
  end

  defp median(values) do
    sorted = Enum.sort(values)
    count = length(sorted)
    middle = div(count, 2)

    if rem(count, 2) == 1,
      do: Enum.at(sorted, middle) * 1.0,
      else: (Enum.at(sorted, middle - 1) + Enum.at(sorted, middle)) / 2.0
  end

  defp finite_positive?(value) when is_number(value), do: value > 0 and value == value
  defp finite_positive?(_value), do: false
end
