defmodule Pika.Baseline.Definition do
  @moduledoc "Validates the complete v2 Baseline Definition bundle through its file interface."

  import Bitwise

  alias Pika.Agent.RolePromptRegistry
  alias Pika.Optimization.FileContract

  @enforce_keys [
    :manifest,
    :manifest_receipt,
    :target_manifest,
    :cases,
    :metrics,
    :smoke_verify,
    :smoke_benchmark,
    :dependency_receipts,
    :dependencies_sha256
  ]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          manifest: map(),
          manifest_receipt: FileContract.receipt(),
          target_manifest: map(),
          cases: [map()],
          metrics: [map()],
          smoke_verify: map(),
          smoke_benchmark: [map()],
          dependency_receipts: %{String.t() => FileContract.receipt()},
          dependencies_sha256: String.t()
        }

  @spec validate(Path.t(), Path.t(), keyword()) :: {:ok, t()} | {:error, term()}
  def validate(work_root, manifest_path, opts \\ []) do
    repo_root = Keyword.get(opts, :repo_root, work_root)

    with {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, manifest, manifest_receipt} <-
           FileContract.validate_json(work_root, manifest_path, schemas.baseline_definition),
         {:ok, target_manifest, target_receipt} <-
           FileContract.validate_json(
             work_root,
             get_in(manifest, ["optimization_target", "manifest_path"]),
             schemas.target_manifest
           ),
         {:ok, cases_file, cases_receipt} <-
           FileContract.validate_json(work_root, manifest["cases_path"], schemas.cases),
         {:ok, metrics_file, metrics_receipt} <-
           FileContract.validate_json(work_root, manifest["metrics_path"], schemas.metrics),
         {:ok, smoke_verify, smoke_verify_receipt} <-
           FileContract.validate_json(
             work_root,
             manifest["smoke_verify_path"],
             schemas.verify_result
           ),
         {:ok, smoke_benchmark, smoke_benchmark_receipt} <-
           FileContract.validate_jsonl(
             work_root,
             manifest["smoke_benchmark_path"],
             schemas.benchmark_record
           ),
         :ok <- validate_scripts(repo_root),
         :ok <- validate_cases(cases_file["cases"]),
         :ok <- validate_metrics(metrics_file["metrics"]),
         :ok <- validate_measurement(manifest["measurement"]),
         :ok <- validate_stopping(manifest["stopping"]),
         :ok <- validate_target_coverage(work_root, manifest, target_manifest),
         :ok <- validate_target_files(work_root, manifest, target_manifest),
         :ok <-
           validate_smoke(
             smoke_verify,
             smoke_benchmark,
             cases_file["cases"],
             metrics_file["metrics"]
           ) do
      receipts = %{
        manifest_receipt.relative_path => manifest_receipt,
        get_in(manifest, ["optimization_target", "manifest_path"]) => target_receipt,
        manifest["cases_path"] => cases_receipt,
        manifest["metrics_path"] => metrics_receipt,
        manifest["smoke_verify_path"] => smoke_verify_receipt,
        manifest["smoke_benchmark_path"] => smoke_benchmark_receipt
      }

      {:ok,
       %__MODULE__{
         manifest: manifest,
         manifest_receipt: manifest_receipt,
         target_manifest: target_manifest,
         cases: cases_file["cases"],
         metrics: metrics_file["metrics"],
         smoke_verify: smoke_verify,
         smoke_benchmark: smoke_benchmark,
         dependency_receipts: receipts,
         dependencies_sha256: dependencies_sha256(receipts)
       }}
    end
  end

  defp validate_scripts(work_root) do
    Enum.reduce_while(~w(verify_cases.sh benchmark_cases.sh), :ok, fn script, :ok ->
      path = Path.join(work_root, script)

      case File.lstat(path) do
        {:ok, %{type: :regular, mode: mode}} when (mode &&& 0o111) != 0 -> {:cont, :ok}
        {:ok, %{type: :regular}} -> {:halt, {:error, {:script_not_executable, script}}}
        {:ok, stat} -> {:halt, {:error, {:script_not_regular, script, stat.type}}}
        {:error, reason} -> {:halt, {:error, {:script_missing, script, reason}}}
      end
    end)
  end

  defp validate_cases(cases) do
    ids = Enum.map(cases, & &1["id"])

    cond do
      ids != Enum.to_list(0..(length(ids) - 1)) ->
        {:error, :case_ids_not_contiguous_from_zero}

      Enum.uniq(Enum.map(cases, & &1["name"])) != Enum.map(cases, & &1["name"]) ->
        {:error, :duplicate_case_names}

      true ->
        :ok
    end
  end

  defp validate_metrics(metrics) do
    ids = Enum.map(metrics, & &1["id"])

    invalid_guard_option =
      Enum.find(
        metrics,
        &(Map.has_key?(&1, "max_regression_ratio") and &1["role"] != "guard")
      )

    cond do
      Enum.uniq(ids) != ids ->
        {:error, :duplicate_metric_ids}

      invalid_guard_option ->
        {:error, {:max_regression_ratio_requires_guard, invalid_guard_option["id"]}}

      not Enum.any?(metrics, &(&1["role"] == "primary")) ->
        {:error, :primary_metric_missing}

      true ->
        :ok
    end
  end

  defp validate_measurement(%{"pair_count" => pair_count, "min_valid_pairs" => minimum}) do
    if minimum <= pair_count, do: :ok, else: {:error, :min_valid_pairs_exceeds_pair_count}
  end

  defp validate_stopping(%{
         "mode" => mode,
         "max_attempts" => max_attempts,
         "max_duration_seconds" => max_duration
       }) do
    valid? =
      case mode do
        "manual" -> is_nil(max_attempts) and is_nil(max_duration)
        "attempt_limit" -> is_integer(max_attempts) and is_nil(max_duration)
        "duration" -> is_nil(max_attempts) and is_integer(max_duration)
        "attempt_or_duration" -> is_integer(max_attempts) and is_integer(max_duration)
        _other -> false
      end

    if valid?, do: :ok, else: {:error, :invalid_stopping_policy}
  end

  defp validate_stopping(_stopping), do: {:error, :invalid_stopping_policy}

  defp validate_target_files(work_root, manifest, target_manifest) do
    target_manifest_path = get_in(manifest, ["optimization_target", "manifest_path"])
    target_root = Path.dirname(target_manifest_path)

    Enum.reduce_while(target_manifest["files"], :ok, fn file, :ok ->
      relative_path = Path.join(target_root, file["path"])

      case FileContract.read(work_root, relative_path) do
        {:ok, _contents, receipt} ->
          if receipt.sha256 == file["sha256"] and receipt.byte_size == file["size"] do
            {:cont, :ok}
          else
            {:halt,
             {:error,
              {:target_file_identity_mismatch, relative_path,
               %{expected_sha256: file["sha256"], actual_sha256: receipt.sha256}}}}
          end

        {:error, reason} ->
          {:halt, {:error, {:invalid_target_file, relative_path, reason}}}
      end
    end)
  end

  defp validate_target_coverage(work_root, manifest, target_manifest) do
    target_manifest_path = get_in(manifest, ["optimization_target", "manifest_path"])
    target_root = Path.join(work_root, Path.dirname(target_manifest_path))
    manifest_name = Path.basename(target_manifest_path)

    actual =
      target_root
      |> Path.join("**/*")
      |> Path.wildcard(match_dot: true)
      |> Enum.filter(&File.regular?/1)
      |> Enum.map(&Path.relative_to(&1, target_root))
      |> Enum.reject(&(&1 == manifest_name))
      |> Enum.sort()

    expected = target_manifest["files"] |> Enum.map(& &1["path"]) |> Enum.sort()

    if actual == expected,
      do: :ok,
      else: {:error, {:target_manifest_coverage_mismatch, %{expected: expected, actual: actual}}}
  end

  defp validate_smoke(verify, benchmark, cases, metrics) do
    requested = verify["requested_case_ids"]
    known_cases = MapSet.new(Enum.map(cases, & &1["id"]))
    metric_ids = MapSet.new(Enum.map(metrics, & &1["id"]))

    verify_passed? =
      length(requested) == 1 and
        MapSet.subset?(MapSet.new(requested), known_cases) and
        Enum.all?(verify["cases"], fn result ->
          result["target"]["passed"] == true and
            result["candidate"]["passed"] == true and
            result["comparison"]["passed"] == true
        end)

    benchmark_case_ids = MapSet.new(Enum.map(benchmark, & &1["case_id"]))
    benchmark_metric_ids = MapSet.new(Enum.map(benchmark, & &1["metric_id"]))
    values_positive? = Enum.all?(benchmark, &positive_pair?/1)

    cond do
      not verify_passed? -> {:error, :smoke_verify_failed}
      benchmark_case_ids != MapSet.new(requested) -> {:error, :smoke_benchmark_case_mismatch}
      benchmark_metric_ids != metric_ids -> {:error, :smoke_benchmark_metric_coverage}
      not values_positive? -> {:error, :smoke_benchmark_invalid_values}
      true -> :ok
    end
  end

  defp positive_pair?(record) do
    record["valid"] == true and is_number(record["target"]) and record["target"] > 0 and
      is_number(record["candidate"]) and record["candidate"] > 0
  end

  defp dependencies_sha256(receipts) do
    receipts
    |> Enum.map(fn {path, receipt} -> [path, receipt.sha256, receipt.byte_size] end)
    |> Enum.sort()
    |> Jason.encode!()
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end
end
