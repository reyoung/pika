defmodule Pika.Baseline do
  @moduledoc false

  def evaluate(samples_path, correctness_path, profiler_path, spec, measured_sha, skill_sha) do
    with {:ok, records} <- read_jsonl(samples_path),
         {:ok, metrics} <- evaluate_records(records, spec, measured_sha),
         :ok <- validate_correctness(correctness_path, spec, measured_sha),
         {:ok, profiler} <- validate_profiler(profiler_path, spec, measured_sha, skill_sha) do
      {:ok, %{measured_sha: measured_sha, metrics: metrics, profiler: profiler}}
    end
  end

  def evaluate_records(records, spec, measured_sha) do
    cases = Map.fetch!(spec, "benchmark_cases")
    metric_defs = Map.fetch!(spec, "metrics")
    expected_pairs = get_in(spec, ["benchmark", "pair_count"])
    min_valid = get_in(spec, ["benchmark", "min_valid_pairs"])
    expected_keys = for case_ <- cases, metric <- metric_defs, do: {case_["id"], metric["id"]}
    grouped = Enum.group_by(records, &{&1["case_id"], &1["metric_id"]})

    with :ok <- validate_record_shapes(records, measured_sha),
         :ok <- validate_expected_keys(grouped, expected_keys) do
      results =
        Enum.map(expected_keys, fn {_case_id, metric_id} = key ->
          definition = Enum.find(metric_defs, &(&1["id"] == metric_id))
          evaluate_group(key, Map.fetch!(grouped, key), definition, expected_pairs, min_valid)
        end)

      case Enum.find(results, &match?({:error, _}, &1)) do
        nil -> {:ok, Enum.map(results, fn {:ok, result} -> result end)}
        error -> error
      end
    else
      {:error, reason} -> {:error, reason}
    end
  end

  defp validate_expected_keys(grouped, expected_keys) do
    if Enum.sort(Map.keys(grouped)) == Enum.sort(expected_keys),
      do: :ok,
      else: {:error, :missing_case_metric_samples}
  end

  defp evaluate_group({case_id, metric_id}, records, definition, expected_pairs, min_valid) do
    indexes = Enum.map(records, & &1["pair_index"])

    cond do
      length(records) != expected_pairs ->
        {:error, {:wrong_pair_count, case_id, metric_id}}

      Enum.sort(indexes) != Enum.to_list(0..(expected_pairs - 1)) ->
        {:error, {:invalid_pair_indexes, case_id, metric_id}}

      not alternating_orders?(records) ->
        {:error, {:non_alternating_pair_orders, case_id, metric_id}}

      true ->
        valid =
          Enum.filter(
            records,
            &(&1["valid"] == true and finite_positive?(&1["a"]) and finite_positive?(&1["b"]))
          )

        if length(valid) < min_valid do
          {:error, {:insufficient_valid_pairs, case_id, metric_id, length(valid)}}
        else
          values = Enum.flat_map(valid, &[&1["a"], &1["b"]])
          deltas = Enum.map(valid, &delta(&1, definition["direction"]))
          center = median(deltas)
          mad = deltas |> Enum.map(&abs(&1 - center)) |> median()

          {:ok,
           %{
             case_id: case_id,
             metric_id: metric_id,
             unit: definition["unit"],
             direction: definition["direction"],
             value: median(values),
             pair_delta_median: center,
             mad: mad,
             noise_tolerance: max(0.005, 3.0 * 1.4826 * mad),
             pair_count: expected_pairs,
             valid_pair_count: length(valid)
           }}
        end
    end
  end

  defp validate_record_shapes(records, measured_sha) do
    valid? =
      Enum.all?(records, fn record ->
        record["schema_version"] == 1 and record["measured_sha"] == measured_sha and
          record["order"] in ["ab", "ba"] and is_integer(record["pair_index"])
      end)

    if valid?, do: :ok, else: {:error, :invalid_sample_record}
  end

  defp alternating_orders?(records) do
    records
    |> Enum.sort_by(& &1["pair_index"])
    |> Enum.map(& &1["order"])
    |> Enum.chunk_every(2, 1, :discard)
    |> Enum.all?(fn [left, right] -> left != right end)
  end

  defp validate_correctness(path, spec, measured_sha) do
    with {:ok, body} <- File.read(path),
         {:ok, report} <- Jason.decode(body),
         true <- report["measured_sha"] == measured_sha,
         cases when is_list(cases) <- report["cases"],
         expected <- Enum.map(spec["benchmark_cases"], & &1["id"]),
         true <- Enum.sort(Enum.map(cases, & &1["case_id"])) == Enum.sort(expected),
         true <- Enum.all?(cases, &(&1["passed"] == true)) do
      :ok
    else
      _ -> {:error, :correctness_failed}
    end
  end

  defp validate_profiler(path, spec, measured_sha, skill_sha) do
    target_ids = for case_ <- spec["benchmark_cases"], case_["kind"] == "target", do: case_["id"]
    Pika.Profiler.validate_manifest(path, target_ids, measured_sha, skill_sha)
  end

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
      {:ok, records} when records != [] -> {:ok, Enum.reverse(records)}
      {:ok, []} -> {:error, :empty_samples}
      error -> error
    end
  rescue
    File.Error -> {:error, :samples_not_found}
  end

  defp delta(record, "minimize"), do: (record["a"] - record["b"]) / record["a"]
  defp delta(record, "maximize"), do: (record["b"] - record["a"]) / record["a"]

  defp median(values) do
    sorted = Enum.sort(values)
    count = length(sorted)
    middle = div(count, 2)

    if rem(count, 2) == 1 do
      Enum.at(sorted, middle) * 1.0
    else
      (Enum.at(sorted, middle - 1) + Enum.at(sorted, middle)) / 2.0
    end
  end

  defp finite_positive?(value) when is_number(value), do: value > 0 and value == value
  defp finite_positive?(_), do: false
end
