defmodule Pika.Baseline do
  @moduledoc false

  def evaluate(
        samples_path,
        correctness_path,
        profiler_path,
        spec,
        measured_sha,
        skill_sha,
        opts \\ []
      ) do
    on_progress = Keyword.get(opts, :on_progress, fn _progress -> :ok end)
    expected_samples = Keyword.get(opts, :expected_samples)

    max_concurrency =
      opts
      |> Keyword.get(:max_concurrency, default_concurrency())
      |> normalize_concurrency()

    with {:ok, samples} <-
           evaluate_jsonl(
             samples_path,
             spec,
             measured_sha,
             on_progress,
             expected_samples,
             max_concurrency
           ),
         :ok <- report_phase(on_progress, :validating_correctness),
         :ok <- validate_correctness(correctness_path, spec, measured_sha),
         :ok <- report_phase(on_progress, :validating_profiler),
         {:ok, profiler} <- validate_profiler(profiler_path, spec, measured_sha, skill_sha) do
      report_phase(on_progress, :completed)

      {:ok,
       %{
         measured_sha: measured_sha,
         metrics: samples.metrics,
         profiler: profiler,
         samples_artifact: %{sha256: samples.sha256, size: samples.size}
       }}
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

  # The file is canonical and each Case/Metric group has exactly expected_pairs
  # consecutive lines. Hashing stays on the ordered reader while CPU-heavy JSON
  # decoding and statistics run in a bounded worker pool.
  defp evaluate_jsonl(path, spec, measured_sha, on_progress, expected_samples, max_concurrency) do
    expected_pairs = get_in(spec, ["benchmark", "pair_count"])
    min_valid = get_in(spec, ["benchmark", "min_valid_pairs"])

    expected_groups =
      for case_ <- Map.fetch!(spec, "benchmark_cases"),
          definition <- Map.fetch!(spec, "metrics") do
        {case_["id"], definition["id"], definition}
      end
      |> List.to_tuple()

    total_groups = tuple_size(expected_groups)
    total_records = total_groups * expected_pairs
    total_bytes = sample_size(path)
    hash_ref = make_ref()
    owner = self()
    started_at = System.monotonic_time(:millisecond)

    initial = %{
      expected_groups: expected_groups,
      total_groups: total_groups,
      total_records: total_records,
      total_bytes: total_bytes,
      expected_pairs: expected_pairs,
      expected_samples: expected_samples,
      completed_groups: 0,
      processed_records: 0,
      processed_bytes: 0,
      metrics: [],
      started_at: started_at,
      last_progress_at: started_at,
      on_progress: on_progress,
      max_concurrency: max_concurrency
    }

    report_parallel_progress(initial, true)

    results =
      path
      |> File.stream!([:read_ahead], :line)
      |> Stream.chunk_every(expected_pairs)
      |> Stream.with_index()
      |> hash_groups(owner, hash_ref)
      |> Task.async_stream(
        fn {lines, group_index, group_bytes} ->
          evaluate_jsonl_group(
            lines,
            group_index,
            group_bytes,
            expected_groups,
            expected_pairs,
            min_valid,
            measured_sha
          )
        end,
        ordered: true,
        max_concurrency: max_concurrency,
        timeout: :infinity
      )
      |> Enum.reduce_while({:ok, initial}, &reduce_parallel_group/2)

    finish_parallel_jsonl(results, hash_ref)
  rescue
    File.Error -> {:error, :samples_not_found}
  end

  defp hash_groups(groups, owner, hash_ref) do
    Stream.transform(
      groups,
      fn -> {:crypto.hash_init(:sha256), 0} end,
      fn {lines, group_index}, {context, size} ->
        group_bytes = :erlang.iolist_size(lines)
        next_context = :crypto.hash_update(context, lines)
        {[{lines, group_index, group_bytes}], {next_context, size + group_bytes}}
      end,
      fn {context, size} ->
        sha256 = context |> :crypto.hash_final() |> Base.encode16(case: :lower)
        send(owner, {:baseline_samples_hash, hash_ref, sha256, size})
      end
    )
  end

  defp evaluate_jsonl_group(
         _lines,
         group_index,
         _group_bytes,
         expected_groups,
         _expected_pairs,
         _min_valid,
         _measured_sha
       )
       when group_index >= tuple_size(expected_groups),
       do: {:error, :unexpected_case_metric_samples}

  defp evaluate_jsonl_group(
         lines,
         group_index,
         group_bytes,
         expected_groups,
         expected_pairs,
         min_valid,
         measured_sha
       ) do
    {case_id, metric_id, definition} = elem(expected_groups, group_index)

    if length(lines) != expected_pairs do
      {:error, {:wrong_pair_count, case_id, metric_id}}
    else
      initial = %{values: [], deltas: [], valid_count: 0, previous_order: nil}

      lines
      |> Enum.with_index()
      |> Enum.reduce_while({:ok, initial}, fn {line, pair_index}, {:ok, acc} ->
        record_number = group_index * expected_pairs + pair_index + 1

        with {:ok, record} <- decode_record(line, record_number),
             :ok <-
               validate_group_record(
                 record,
                 record_number,
                 pair_index,
                 case_id,
                 metric_id,
                 measured_sha,
                 acc.previous_order
               ) do
          {:cont, {:ok, accumulate_group_record(acc, record, definition)}}
        else
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)
      |> finish_parallel_group(
        case_id,
        metric_id,
        definition,
        expected_pairs,
        min_valid,
        group_bytes
      )
    end
  end

  defp decode_record(line, record_number) do
    case Jason.decode(line) do
      {:ok, record} -> {:ok, record}
      _error -> {:error, {:invalid_samples_jsonl, record_number}}
    end
  end

  defp validate_group_record(
         record,
         record_number,
         pair_index,
         case_id,
         metric_id,
         measured_sha,
         previous_order
       ) do
    cond do
      record["schema_version"] != 1 or record["measured_sha"] != measured_sha or
        record["order"] not in ["ab", "ba"] or not is_integer(record["pair_index"]) ->
        {:error, {:invalid_sample_record, record_number}}

      record["case_id"] != case_id or record["metric_id"] != metric_id ->
        {:error,
         {:unexpected_case_metric_order, record_number, case_id, metric_id, record["case_id"],
          record["metric_id"]}}

      record["pair_index"] != pair_index ->
        {:error, {:invalid_pair_indexes, case_id, metric_id}}

      pair_index > 0 and record["order"] == previous_order ->
        {:error, {:non_alternating_pair_orders, case_id, metric_id}}

      true ->
        :ok
    end
  end

  defp accumulate_group_record(acc, record, definition) do
    valid? =
      record["valid"] == true and finite_positive?(record["a"]) and
        finite_positive?(record["b"])

    if valid? do
      %{
        acc
        | values: [record["b"], record["a"] | acc.values],
          deltas: [delta(record, definition["direction"]) | acc.deltas],
          valid_count: acc.valid_count + 1,
          previous_order: record["order"]
      }
    else
      %{acc | previous_order: record["order"]}
    end
  end

  defp finish_parallel_group({:error, reason}, _case, _metric, _definition, _pairs, _min, _bytes),
    do: {:error, reason}

  defp finish_parallel_group(
         {:ok, acc},
         case_id,
         metric_id,
         definition,
         expected_pairs,
         min_valid,
         group_bytes
       ) do
    if acc.valid_count < min_valid do
      {:error, {:insufficient_valid_pairs, case_id, metric_id, acc.valid_count}}
    else
      center = median(acc.deltas)
      mad = acc.deltas |> Enum.map(&abs(&1 - center)) |> median()

      {:ok,
       %{
         case_id: case_id,
         metric_id: metric_id,
         unit: definition["unit"],
         direction: definition["direction"],
         value: median(acc.values),
         pair_delta_median: center,
         mad: mad,
         noise_tolerance: max(0.005, 3.0 * 1.4826 * mad),
         pair_count: expected_pairs,
         valid_pair_count: acc.valid_count
       }, group_bytes}
    end
  end

  defp reduce_parallel_group({:ok, {:ok, metric, group_bytes}}, {:ok, state}) do
    state = %{
      state
      | completed_groups: state.completed_groups + 1,
        processed_records: state.processed_records + state.expected_pairs,
        processed_bytes: state.processed_bytes + group_bytes,
        metrics: [metric | state.metrics]
    }

    {:cont, {:ok, report_parallel_progress(state, false)}}
  end

  defp reduce_parallel_group({:ok, {:error, reason}}, {:ok, _state}),
    do: {:halt, {:error, reason}}

  defp reduce_parallel_group({:exit, reason}, {:ok, _state}),
    do: {:halt, {:error, {:baseline_worker_exited, reason}}}

  defp finish_parallel_jsonl({:error, reason}, _hash_ref), do: {:error, reason}

  defp finish_parallel_jsonl({:ok, %{processed_records: 0}}, _hash_ref),
    do: {:error, :empty_samples}

  defp finish_parallel_jsonl({:ok, state}, hash_ref) do
    if state.completed_groups == state.total_groups do
      report_parallel_progress(state, true)

      receive do
        {:baseline_samples_hash, ^hash_ref, sha256, size} ->
          with true <- size == state.processed_bytes,
               :ok <- validate_expected_sample_size(state.expected_samples, size),
               :ok <- validate_expected_sample_sha(state.expected_samples, sha256) do
            {:ok, %{metrics: Enum.reverse(state.metrics), sha256: sha256, size: size}}
          else
            false -> {:error, :samples_hash_size_mismatch}
            {:error, reason} -> {:error, reason}
          end
      after
        5_000 -> {:error, :samples_hash_not_available}
      end
    else
      {:error, :missing_case_metric_samples}
    end
  end

  defp validate_expected_sample_size(nil, _actual), do: :ok

  defp validate_expected_sample_size(expected, actual) do
    case expected[:size] || expected["size"] do
      nil -> :ok
      ^actual -> :ok
      size -> {:error, {:samples_size_mismatch, size, actual}}
    end
  end

  defp validate_expected_sample_sha(nil, _actual), do: :ok

  defp validate_expected_sample_sha(expected, actual) do
    case expected[:sha256] || expected["sha256"] do
      nil -> :ok
      ^actual -> :ok
      sha256 -> {:error, {:samples_sha256_mismatch, sha256, actual}}
    end
  end

  defp report_parallel_progress(state, force?) do
    now = System.monotonic_time(:millisecond)

    if force? or now - state.last_progress_at >= 500 do
      {case_id, metric_id} = current_parallel_group(state)
      elapsed_seconds = max((now - state.started_at) / 1_000, 0.001)
      records_per_second = state.processed_records / elapsed_seconds

      eta_seconds =
        if records_per_second > 0,
          do: (state.total_records - state.processed_records) / records_per_second,
          else: nil

      report_progress(state.on_progress, %{
        phase: :reading_samples,
        processed_records: state.processed_records,
        total_records: state.total_records,
        processed_bytes: state.processed_bytes,
        total_bytes: state.total_bytes,
        completed_groups: state.completed_groups,
        total_groups: state.total_groups,
        case_id: case_id,
        metric_id: metric_id,
        max_concurrency: state.max_concurrency,
        records_per_second: records_per_second,
        eta_seconds: eta_seconds
      })

      %{state | last_progress_at: now}
    else
      state
    end
  end

  defp current_parallel_group(%{completed_groups: index, total_groups: total})
       when index >= total,
       do: {nil, nil}

  defp current_parallel_group(state) do
    {case_id, metric_id, _definition} =
      elem(state.expected_groups, state.completed_groups)

    {case_id, metric_id}
  end

  defp report_phase(on_progress, phase) do
    report_progress(on_progress, %{phase: phase})
    :ok
  end

  defp report_progress(on_progress, progress) do
    on_progress.(progress)
    :ok
  end

  defp sample_size(path) do
    case File.stat(path) do
      {:ok, stat} -> stat.size
      _error -> nil
    end
  end

  defp default_concurrency do
    Application.get_env(
      :pika,
      :baseline_validation_concurrency,
      min(System.schedulers_online(), 16)
    )
  end

  defp normalize_concurrency(value) when is_integer(value) and value > 0,
    do: min(value, System.schedulers_online())

  defp normalize_concurrency(_value), do: min(System.schedulers_online(), 16)

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
