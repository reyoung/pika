defmodule Pika.Measurement do
  @moduledoc false

  @screen_pairs 5
  @screen_min_valid 4

  def evaluate_iteration(samples_path, correctness_path, context) do
    {pair_count, min_valid_pairs} = formal_protocol(context)

    with {:ok, records} <- read_jsonl(samples_path),
         :ok <- validate_correctness(correctness_path, context),
         {:ok, metrics} <-
           evaluate_records(records, context,
             expected_pairs: pair_count,
             min_valid: min_valid_pairs,
             source: "iteration",
             allow_insufficient: false,
             best_metrics: Map.get(context, :best_metrics, %{})
           ) do
      {:ok, metrics}
    end
  end

  def evaluate_integration(screening_path, full_path, correctness_path, context, best_metrics) do
    best_metrics =
      Map.put(
        best_metrics,
        :target_case_ids,
        Map.get(context, :target_case_ids, context.case_ids)
      )

    context = Map.put(context, :best_metrics, best_metrics)

    with {:ok, records} <- read_jsonl(screening_path),
         :ok <- validate_correctness(correctness_path, context),
         {:ok, screening} <-
           evaluate_records(records, context,
             expected_pairs: @screen_pairs,
             min_valid: @screen_min_valid,
             source: "integration_screen",
             allow_insufficient: true,
             best_metrics: best_metrics
           ),
         escalated <- escalation_keys(screening, best_metrics),
         {:ok, full} <- evaluate_escalations(full_path, escalated, context),
         {:ok, final, regressions, target_improvement?} <-
           combine_integration(screening, full, escalated, best_metrics) do
      {:ok,
       %{
         metrics: final,
         escalated: escalated,
         regressions: regressions,
         target_improvement?: target_improvement?
       }}
    end
  end

  def evaluate_fast_rejection(samples_path, correctness_path, context, best_metrics) do
    {pair_count, min_valid_pairs} = formal_protocol(context)

    with {:ok, correctness} <- validate_fast_rejection_correctness(correctness_path, context),
         {:ok, records} <- read_jsonl(samples_path),
         observed_cases when observed_cases != [] <-
           records |> Enum.map(& &1["case_id"]) |> Enum.uniq(),
         observed_keys <- records |> Enum.map(&{&1["case_id"], &1["metric_id"]}) |> Enum.uniq(),
         partial_context <- %{context | case_ids: observed_cases},
         :ok <- validate_fast_rejection_coverage(correctness, partial_context),
         {:ok, metrics} <-
           evaluate_records(records, partial_context,
             expected_pairs: pair_count,
             min_valid: min_valid_pairs,
             source: "integration_fast_reject",
             allow_insufficient: false,
             best_metrics: best_metrics,
             expected_keys: observed_keys
           ) do
      candidate_badcases =
        for case_ <- correctness["cases"], case_["candidate_passed"] != true, do: case_["case_id"]

      regressions =
        metrics
        |> Enum.filter(&confirmed_regression?(&1, best_metrics))
        |> Enum.map(&{&1.case_id, &1.metric_id})

      target_metric_ids =
        for metric <- context.metrics, metric["role"] == "target", do: metric["id"]

      required_target_keys =
        for case_id <- context.target_case_ids,
            metric_id <- target_metric_ids,
            do: {case_id, metric_id}

      target_coverage? =
        MapSet.subset?(MapSet.new(required_target_keys), MapSet.new(observed_keys))

      meaningful_improvement? =
        Enum.any?(metrics, fn metric ->
          metric.role == "target" and metric.case_id in context.target_case_ids and
            is_number(metric.target_relative_improvement) and
            metric.target_relative_improvement >=
              max(metric.min_improvement_ratio, metric.noise_tolerance)
        end)

      cond do
        candidate_badcases != [] ->
          {:ok,
           %{
             metrics: metrics,
             regressions: [],
             reason: "candidate correctness failed: #{Enum.join(candidate_badcases, ", ")}"
           }}

        regressions != [] ->
          {:ok, %{metrics: metrics, regressions: regressions, reason: "confirmed regression"}}

        target_coverage? and not meaningful_improvement? ->
          {:ok, %{metrics: metrics, regressions: [], reason: "no meaningful target improvement"}}

        true ->
          {:error, :fast_rejection_not_proven}
      end
    else
      [] -> {:error, :fast_rejection_samples_empty}
      {:error, _} = error -> error
    end
  end

  def evaluate_records(records, context, opts) when is_list(records) and is_map(context) do
    expected_pairs = Keyword.fetch!(opts, :expected_pairs)
    min_valid = Keyword.fetch!(opts, :min_valid)
    source = Keyword.fetch!(opts, :source)
    allow_insufficient = Keyword.get(opts, :allow_insufficient, false)
    best_metrics = Keyword.get(opts, :best_metrics, %{})
    metric_defs = Map.new(context.metrics, &{&1["id"], &1})

    expected_keys =
      Keyword.get_lazy(opts, :expected_keys, fn ->
        for case_id <- context.case_ids, metric <- context.metrics, do: {case_id, metric["id"]}
      end)

    grouped = Enum.group_by(records, &{&1["case_id"], &1["metric_id"]})

    with :ok <- validate_record_shapes(records, context),
         :ok <- validate_expected_keys(grouped, expected_keys) do
      result =
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

      case result do
        {:ok, metrics} -> {:ok, Enum.map(metrics, &compare_with_best(&1, best_metrics))}
        error -> error
      end
    end
  end

  defp evaluate_escalations(_path, [], _context), do: {:ok, []}
  defp evaluate_escalations(nil, keys, _context), do: {:error, {:missing_full_escalation, keys}}

  defp evaluate_escalations(path, keys, context) do
    {pair_count, min_valid_pairs} = formal_protocol(context)

    with {:ok, records} <- read_jsonl(path) do
      expected = MapSet.new(keys)
      records = Enum.filter(records, &MapSet.member?(expected, {&1["case_id"], &1["metric_id"]}))

      case evaluate_records(records, context,
             expected_pairs: pair_count,
             min_valid: min_valid_pairs,
             source: "integration_full",
             allow_insufficient: false,
             best_metrics: Map.get(context, :best_metrics, %{}),
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

    target_case_ids = MapSet.new(Map.get(best_metrics, :target_case_ids, []))

    target_improvement? =
      Enum.any?(final, fn metric ->
        metric.role == "target" and
          (MapSet.size(target_case_ids) == 0 or MapSet.member?(target_case_ids, metric.case_id)) and
          is_number(metric.target_relative_improvement) and
          metric.target_relative_improvement >=
            max(metric.min_improvement_ratio, metric.noise_tolerance)
      end)

    {:ok, final, regressions, target_improvement?}
  end

  defp escalation_keys(screening, best_metrics) do
    screening
    |> Enum.filter(
      &(&1.insufficient? or confirmed_regression?(&1, best_metrics) or
          target_goal_candidate?(&1, best_metrics))
    )
    |> Enum.map(&{&1.case_id, &1.metric_id})
  end

  defp target_goal_candidate?(metric, best_metrics) do
    target_case_ids = MapSet.new(Map.get(best_metrics, :target_case_ids, []))

    (MapSet.size(target_case_ids) == 0 or MapSet.member?(target_case_ids, metric.case_id)) and
      metric.role == "target" and is_number(metric.target_relative_improvement) and
      metric.target_relative_improvement >=
        max(metric.min_improvement_ratio, metric.noise_tolerance)
  end

  defp confirmed_regression?(metric, _best_metrics) do
    tolerance = max(metric.max_regression_ratio || 0.0, metric.noise_tolerance || 0.005)

    is_nil(metric.best_relative_improvement) or metric.best_relative_improvement < -tolerance
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
               min_valid,
               nil,
               nil,
               nil,
               nil
             )}

          true ->
            improvements = Enum.map(valid, &target_improvement(&1, definition["direction"]))
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
               min_valid,
               median(Enum.map(valid, & &1["candidate"])),
               median(Enum.map(valid, & &1["target"])),
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
         min_valid,
         value,
         target_value,
         target_relative_improvement,
         mad
       ) do
    %{
      case_id: case_id,
      metric_id: metric_id,
      unit: definition["unit"],
      direction: definition["direction"],
      role: definition["role"],
      min_improvement_ratio: definition["min_improvement_ratio"] || 0.01,
      max_regression_ratio: definition["max_regression_ratio"] || 0.0,
      value: value,
      target_value: target_value,
      target_relative_improvement: target_relative_improvement,
      best_relative_improvement: nil,
      baseline_value: nil,
      improvement_ratio: nil,
      mad: mad,
      noise_tolerance: if(is_nil(mad), do: 0.005, else: max(0.005, 3.0 * 1.4826 * mad)),
      pair_count: pair_count,
      valid_pair_count: valid_count,
      insufficient?: valid_count < min_valid,
      source: source
    }
  end

  defp compare_with_best(metric, best_metrics) do
    case Map.fetch(best_metrics, {metric.case_id, metric.metric_id}) do
      {:ok, best} ->
        best_value = best[:value] || best["value"]
        best_noise = best[:noise_tolerance] || best["noise_tolerance"] || 0.005

        best_relative =
          cond do
            not finite_positive?(metric.value) or not finite_positive?(best_value) ->
              nil

            metric.direction == "minimize" ->
              (best_value - metric.value) / best_value

            metric.direction == "maximize" ->
              (metric.value - best_value) / best_value
          end

        %{
          metric
          | baseline_value: best_value,
            improvement_ratio: best_relative,
            best_relative_improvement: best_relative,
            noise_tolerance: max(0.005, max(metric.noise_tolerance, best_noise))
        }

      :error ->
        metric
    end
  end

  defp formal_protocol(context) do
    benchmark = Map.fetch!(context, :benchmark)
    {Map.fetch!(benchmark, "pair_count"), Map.fetch!(benchmark, "min_valid_pairs")}
  end

  defp validate_record_shapes(records, _context) do
    valid? =
      Enum.all?(records, fn record ->
        record["order"] in ["tc", "ct"] and is_integer(record["pair_index"])
      end)

    if valid?, do: :ok, else: {:error, :invalid_sample_record}
  end

  defp validate_expected_keys(grouped, expected_keys) do
    if Enum.sort(Map.keys(grouped)) == Enum.sort(expected_keys),
      do: :ok,
      else: {:error, {:invalid_case_metric_coverage, Enum.sort(Map.keys(grouped))}}
  end

  defp validate_correctness(path, context) do
    with {:ok, body} <- File.read(path),
         {:ok, report} <- Jason.decode(body),
         true <- report["schema_version"] == 2,
         true <- report["target_snapshot_id"] == context.target_snapshot_id,
         true <- report["candidate_sha"] == context.candidate_sha,
         cases when is_list(cases) <- report["cases"],
         true <- Enum.sort(Enum.map(cases, & &1["case_id"])) == Enum.sort(context.case_ids),
         true <-
           Enum.all?(
             cases,
             &(&1["target_passed"] == true and &1["candidate_passed"] == true)
           ) do
      :ok
    else
      _ -> {:error, :correctness_failed}
    end
  end

  defp validate_fast_rejection_correctness(path, context) do
    with {:ok, body} <- File.read(path),
         {:ok, report} <- Jason.decode(body),
         true <- report["schema_version"] == 2,
         true <- report["target_snapshot_id"] == context.target_snapshot_id,
         true <- report["candidate_sha"] == context.candidate_sha,
         cases when is_list(cases) and cases != [] <- report["cases"],
         true <- Enum.all?(cases, &(&1["target_passed"] == true)) do
      {:ok, report}
    else
      _ -> {:error, :correctness_failed}
    end
  end

  defp validate_fast_rejection_coverage(report, context) do
    case_ids = Enum.map(report["cases"], & &1["case_id"])

    if Enum.sort(case_ids) == Enum.sort(context.case_ids),
      do: :ok,
      else: {:error, :correctness_failed}
  end

  defp alternating_orders?(records) do
    records
    |> Enum.sort_by(& &1["pair_index"])
    |> Enum.with_index()
    |> Enum.all?(fn {record, index} ->
      record["order"] == if(rem(index, 2) == 0, do: "tc", else: "ct")
    end)
  end

  defp valid_pair?(record) do
    record["valid"] == true and finite_positive?(record["target"]) and
      finite_positive?(record["candidate"])
  end

  defp target_improvement(record, "minimize"),
    do: (record["target"] - record["candidate"]) / record["target"]

  defp target_improvement(record, "maximize"),
    do: (record["candidate"] - record["target"]) / record["target"]

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
