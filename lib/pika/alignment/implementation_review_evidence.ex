defmodule Pika.Alignment.ImplementationReviewEvidence do
  @moduledoc false

  @schema_version 2
  @evidence_fields ~w(
    schema_version spec_revision target_snapshot_id target_snapshot_digest development_sha
    harness_digest case_id case_name command environment exit_code correctness metrics
    output_artifact output_sha256 output_size summary digest
  )a

  def validate(spec, harness, target_snapshot, development_sha, args, artifact) do
    args = stringify_keys(args)

    with true <- args["schema_version"] == @schema_version || {:error, :invalid_schema_version},
         :ok <- validate_bindings(spec, harness, target_snapshot, development_sha, args),
         {:ok, case_definition} <- find_case(spec, args["case_id"]),
         {:ok, correctness} <- validate_correctness(args["correctness"]),
         {:ok, metrics} <- validate_metrics(spec, args["metrics"]),
         {:ok, command} <- nonempty_string(args, "command"),
         {:ok, environment} <- nonempty_string(args, "environment"),
         {:ok, summary} <- nonempty_string(args, "summary"),
         true <- args["exit_code"] == 0 || {:error, :implementation_review_run_failed},
         :ok <- validate_artifact(artifact, args["output_artifact"]) do
      evidence = %{
        schema_version: @schema_version,
        spec_revision: spec["revision"],
        target_snapshot_id: target_snapshot.id,
        target_snapshot_digest: target_snapshot.digest,
        development_sha: development_sha,
        harness_digest: harness.digest,
        case_id: case_definition["id"],
        case_name: case_definition["name"],
        command: command,
        environment: environment,
        exit_code: 0,
        correctness: correctness,
        metrics: metrics,
        output_artifact: artifact.relative_path,
        output_sha256: artifact.sha256,
        output_size: artifact.size,
        summary: summary,
        submitted_at: DateTime.utc_now()
      }

      {:ok, Map.put(evidence, :digest, digest(evidence))}
    else
      false -> {:error, :invalid_implementation_review_evidence}
      {:error, _reason} = error -> error
    end
  end

  def verify(spec, harness, target_snapshot, development_sha, evidence, artifact)
      when is_map(evidence) do
    with true <- Enum.all?(@evidence_fields, &Map.has_key?(evidence, &1)),
         true <- evidence.schema_version == @schema_version,
         true <- evidence.spec_revision == spec["revision"],
         true <- evidence.target_snapshot_id == target_snapshot.id,
         true <- evidence.target_snapshot_digest == target_snapshot.digest,
         true <- evidence.development_sha == development_sha,
         true <- evidence.harness_digest == harness.digest,
         {:ok, case_definition} <- find_case(spec, evidence.case_id),
         true <- evidence.case_name == case_definition["name"],
         {:ok, correctness} <- validate_correctness(evidence.correctness),
         true <- correctness == evidence.correctness,
         {:ok, metrics} <- validate_metrics(spec, evidence.metrics),
         true <- metrics == evidence.metrics,
         true <- evidence.exit_code == 0,
         :ok <- validate_artifact(artifact, evidence.output_artifact),
         true <- evidence.output_sha256 == artifact.sha256,
         true <- evidence.output_size == artifact.size,
         true <- digest(evidence) == evidence.digest do
      :ok
    else
      false -> {:error, :implementation_review_evidence_changed}
      {:error, _reason} = error -> error
    end
  end

  def verify(_spec, _harness, _target, _development_sha, _evidence, _artifact),
    do: {:error, :implementation_review_evidence_missing}

  defp validate_bindings(spec, harness, target_snapshot, development_sha, args) do
    cond do
      args["spec_revision"] != spec["revision"] ->
        {:error, :spec_revision_mismatch}

      args["target_snapshot_id"] != target_snapshot.id ->
        {:error, :target_snapshot_mismatch}

      args["development_sha"] != development_sha ->
        {:error, :development_sha_mismatch}

      args["harness_digest"] != harness.digest ->
        {:error, :harness_digest_mismatch}

      true ->
        :ok
    end
  end

  defp validate_correctness(correctness) when is_map(correctness) do
    correctness = stringify_keys(correctness)

    if correctness["target_passed"] == true and correctness["development_passed"] == true do
      {:ok, %{target_passed: true, development_passed: true}}
    else
      {:error, :correctness_failed}
    end
  end

  defp validate_correctness(_correctness), do: {:error, :correctness_failed}

  defp find_case(spec, case_id) when is_binary(case_id) do
    case Enum.find(spec["benchmark_cases"] || [], &(&1["id"] == case_id)) do
      nil -> {:error, {:unknown_benchmark_case, case_id}}
      case_definition -> {:ok, case_definition}
    end
  end

  defp find_case(_spec, case_id), do: {:error, {:unknown_benchmark_case, case_id}}

  defp validate_metrics(spec, metrics) when is_list(metrics) and metrics != [] do
    definitions = Map.new(spec["metrics"] || [], &{&1["id"], &1})

    metrics
    |> Enum.reduce_while({:ok, [], MapSet.new()}, fn
      metric, {:ok, acc, seen} when is_map(metric) ->
        metric = stringify_keys(metric)
        metric_id = metric["metric_id"]
        definition = definitions[metric_id]

        cond do
          is_nil(definition) ->
            {:halt, {:error, {:unknown_metric, metric_id}}}

          MapSet.member?(seen, metric_id) ->
            {:halt, {:error, {:duplicate_metric, metric_id}}}

          not finite_positive?(metric["target_value"]) ->
            {:halt, {:error, {:invalid_target_metric_value, metric_id}}}

          not finite_positive?(metric["development_value"]) ->
            {:halt, {:error, {:invalid_development_metric_value, metric_id}}}

          metric["unit"] != definition["unit"] ->
            {:halt,
             {:error, {:metric_unit_mismatch, metric_id, metric["unit"], definition["unit"]}}}

          not (is_integer(metric["sample_count"]) and metric["sample_count"] > 0) ->
            {:halt, {:error, {:invalid_metric_sample_count, metric_id}}}

          true ->
            normalized = %{
              metric_id: metric_id,
              name: definition["name"],
              target_value: metric["target_value"],
              development_value: metric["development_value"],
              unit: definition["unit"],
              sample_count: metric["sample_count"],
              direction: definition["direction"],
              role: definition["role"]
            }

            {:cont, {:ok, [normalized | acc], MapSet.put(seen, metric_id)}}
        end

      _metric, _acc ->
        {:halt, {:error, :invalid_performance_metric}}
    end)
    |> case do
      {:ok, normalized, _seen} -> {:ok, Enum.reverse(normalized)}
      {:error, _reason} = error -> error
    end
  end

  defp validate_metrics(_spec, _metrics), do: {:error, :performance_metric_required}

  defp validate_artifact(
         %{kind: "implementation_review_evidence", relative_path: relative_path},
         relative_path
       ),
       do: :ok

  defp validate_artifact(nil, relative_path),
    do: {:error, {:artifact_not_registered, relative_path}}

  defp validate_artifact(_artifact, _relative_path),
    do: {:error, :invalid_implementation_review_artifact}

  defp nonempty_string(args, key) do
    case args[key] do
      value when is_binary(value) ->
        case String.trim(value) do
          "" -> {:error, {:empty_field, key}}
          trimmed -> {:ok, trimmed}
        end

      _ ->
        {:error, {:empty_field, key}}
    end
  end

  defp digest(evidence) do
    metrics =
      Enum.map(evidence.metrics, fn metric ->
        [
          metric.metric_id,
          metric.name,
          metric.target_value,
          metric.development_value,
          metric.unit,
          metric.sample_count,
          metric.direction,
          metric.role
        ]
      end)

    [
      evidence.schema_version,
      evidence.spec_revision,
      evidence.target_snapshot_id,
      evidence.target_snapshot_digest,
      evidence.development_sha,
      evidence.harness_digest,
      evidence.case_id,
      evidence.case_name,
      evidence.command,
      evidence.environment,
      evidence.exit_code,
      evidence.correctness,
      metrics,
      evidence.output_artifact,
      evidence.output_sha256,
      evidence.output_size,
      evidence.summary
    ]
    |> Jason.encode!()
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  defp finite_positive?(value) when is_number(value), do: value > 0 and value == value
  defp finite_positive?(_value), do: false

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
