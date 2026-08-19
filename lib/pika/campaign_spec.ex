defmodule Pika.CampaignSpec do
  @moduledoc false

  @case_kinds ~w(target guard informational)
  @metric_roles ~w(target guard informational)
  @directions ~w(minimize maximize)
  @stop_modes ~w(all_goals any_goal)
  @shape_source_kinds ~w(artifact script repository manual)

  def json_schema do
    %{
      "type" => "object",
      "required" =>
        ~w(schema_version revision title target_hardware computation benchmark_cases metrics benchmark iteration_sampling stopping reference_ids),
      "properties" => %{
        "schema_version" => %{"type" => "integer", "const" => 1},
        "revision" => %{"type" => "integer", "minimum" => 1},
        "title" => string_schema(),
        "target_hardware" => string_schema(),
        "computation" => %{
          "type" => "object",
          "required" => ~w(semantics reference_path inputs outputs fusion_scope correctness),
          "properties" => %{
            "semantics" => string_schema(),
            "reference_path" => string_schema(),
            "inputs" => object_array_schema(),
            "outputs" => object_array_schema(),
            "fusion_scope" => string_schema(),
            "correctness" => %{
              "type" => "object",
              "required" => ~w(rtol atol),
              "properties" => %{
                "rtol" => number_schema(),
                "atol" => number_schema(),
                "invariants" => %{"type" => "array", "items" => string_schema()}
              }
            }
          }
        },
        "benchmark_cases" => %{
          "type" => "array",
          "minItems" => 1,
          "items" => %{
            "type" => "object",
            "required" => ~w(id name kind shape dtype layout),
            "properties" => %{
              "id" => slug_schema(),
              "name" => string_schema(),
              "kind" => %{"type" => "string", "enum" => @case_kinds},
              "shape" => %{"type" => "object"},
              "dtype" => %{"type" => ["string", "object", "array"]},
              "layout" => %{"type" => ["string", "object", "array"]},
              "distribution" => %{"type" => ["object", "null"]},
              "frequency_weight" => %{"type" => ["number", "null"]}
            }
          }
        },
        "metrics" => %{
          "type" => "array",
          "minItems" => 1,
          "items" => %{
            "type" => "object",
            "required" => ~w(id name unit direction role min_improvement_ratio),
            "properties" => %{
              "id" => slug_schema(),
              "name" => string_schema(),
              "unit" => string_schema(),
              "direction" => %{"type" => "string", "enum" => @directions},
              "role" => %{"type" => "string", "enum" => @metric_roles},
              "min_improvement_ratio" => %{"type" => "number", "minimum" => 0.01}
            }
          }
        },
        "benchmark" => %{
          "type" => "object",
          "required" => ~w(harness_path warmup pair_count min_valid_pairs retry_limit),
          "properties" => %{
            "harness_path" => string_schema(),
            "warmup" => %{"type" => "integer", "const" => 10},
            "pair_count" => %{"type" => "integer", "minimum" => 1},
            "min_valid_pairs" => %{"type" => "integer", "minimum" => 1},
            "retry_limit" => %{"type" => "integer", "const" => 1},
            "shape_source" => %{
              "type" => ["object", "null"],
              "required" => ~w(kind path record_count summary),
              "properties" => %{
                "kind" => %{"type" => "string", "enum" => @shape_source_kinds},
                "path" => %{"type" => ["string", "null"]},
                "record_count" => %{"type" => ["integer", "null"], "minimum" => 1},
                "summary" => string_schema()
              }
            }
          }
        },
        "iteration_sampling" => %{
          "type" => "object",
          "required" => ~w(max_initial_cases),
          "properties" => %{
            "max_initial_cases" => %{"type" => "integer", "const" => 10}
          }
        },
        "stopping" => %{
          "type" => "object",
          "required" => ~w(mode),
          "properties" => %{
            "max_attempts" => %{"type" => ["integer", "null"], "minimum" => 1},
            "metric_goals" => %{"type" => "array", "items" => %{"type" => "object"}},
            "mode" => %{"type" => "string", "enum" => @stop_modes}
          }
        },
        "reference_ids" => %{"type" => "array", "minItems" => 1, "items" => string_schema()}
      }
    }
  end

  def validate(input) when is_map(input) do
    spec = input |> stringify_keys() |> defaults()
    missing = missing_fields(spec)
    errors = semantic_errors(spec)

    %{spec: spec, missing: missing, errors: errors, ready?: missing == [] and errors == []}
  end

  def validate(_),
    do: %{spec: %{}, missing: ["spec"], errors: ["spec must be an object"], ready?: false}

  def diff(old, new) when is_map(old) and is_map(new) do
    changed_paths = diff_paths(old, new, []) |> Enum.sort()
    %{from: old, to: new, changed_paths: changed_paths}
  end

  defp defaults(spec) do
    metrics =
      Enum.map(list(spec["metrics"]), fn
        metric when is_map(metric) -> Map.put_new(metric, "min_improvement_ratio", 0.01)
        metric -> metric
      end)

    supplied_benchmark = map(spec["benchmark"])

    benchmark =
      %{
        "warmup" => 10,
        "retry_limit" => 1
      }
      |> Map.merge(supplied_benchmark)

    spec
    |> Map.put_new("schema_version", 1)
    |> Map.put_new("revision", 1)
    |> Map.put("metrics", metrics)
    |> Map.put("benchmark", benchmark)
    |> Map.put_new("iteration_sampling", %{"max_initial_cases" => 10})
  end

  defp missing_fields(spec) do
    checks = [
      {"title", present?(spec["title"])},
      {"target_hardware", present?(spec["target_hardware"])},
      {"computation.semantics", present?(get_in(spec, ["computation", "semantics"]))},
      {"computation.reference_path", present?(get_in(spec, ["computation", "reference_path"]))},
      {"computation.inputs", nonempty_list?(get_in(spec, ["computation", "inputs"]))},
      {"computation.outputs", nonempty_list?(get_in(spec, ["computation", "outputs"]))},
      {"computation.fusion_scope", present?(get_in(spec, ["computation", "fusion_scope"]))},
      {"computation.correctness.rtol",
       number?(get_in(spec, ["computation", "correctness", "rtol"]))},
      {"computation.correctness.atol",
       number?(get_in(spec, ["computation", "correctness", "atol"]))},
      {"benchmark_cases", nonempty_list?(spec["benchmark_cases"])},
      {"metrics", nonempty_list?(spec["metrics"])},
      {"benchmark.harness_path", present?(get_in(spec, ["benchmark", "harness_path"]))},
      {"benchmark.pair_count", positive_integer?(get_in(spec, ["benchmark", "pair_count"]))},
      {"benchmark.min_valid_pairs",
       positive_integer?(get_in(spec, ["benchmark", "min_valid_pairs"]))},
      {"iteration_sampling.max_initial_cases",
       get_in(spec, ["iteration_sampling", "max_initial_cases"]) == 10},
      {"reference_ids", nonempty_list?(spec["reference_ids"])},
      {"stopping", valid_stopping?(spec["stopping"])}
    ]

    for {path, false} <- checks, do: path
  end

  defp semantic_errors(spec) do
    cases = list(spec["benchmark_cases"])
    metrics = list(spec["metrics"])
    benchmark = map(spec["benchmark"])
    stopping = map(spec["stopping"])

    []
    |> add_error(spec["schema_version"] != 1, "schema_version must be 1")
    |> add_error(
      not (is_integer(spec["revision"]) and spec["revision"] >= 1),
      "revision must be a positive integer"
    )
    |> add_error(
      not Enum.any?(cases, &(&1["kind"] == "target")),
      "at least one target Benchmark Case is required"
    )
    |> add_error(
      not Enum.any?(metrics, &(is_map(&1) and &1["role"] == "target")),
      "at least one target Metric is required"
    )
    |> add_error(not unique_ids?(cases), "Benchmark Case ids must be unique stable slugs")
    |> add_error(Enum.any?(cases, &(not valid_case?(&1))), "Benchmark Cases have invalid fields")
    |> add_error(
      Enum.any?(cases, &range_shape?(&1["shape"])),
      "Benchmark Case shapes must be concrete; split min/max ranges into stable Case IDs"
    )
    |> Kernel.++(metric_errors(metrics))
    |> Kernel.++(duplicate_metric_id_errors(metrics))
    |> add_error(benchmark["warmup"] != 10, "benchmark warmup must equal 10")
    |> add_error(
      not positive_integer?(benchmark["pair_count"]),
      "benchmark pair_count must be a positive integer"
    )
    |> add_error(
      not positive_integer?(benchmark["min_valid_pairs"]),
      "benchmark min_valid_pairs must be a positive integer"
    )
    |> add_error(
      positive_integer?(benchmark["pair_count"]) and
        positive_integer?(benchmark["min_valid_pairs"]) and
        benchmark["min_valid_pairs"] > benchmark["pair_count"],
      "benchmark min_valid_pairs must not exceed pair_count"
    )
    |> add_error(benchmark["retry_limit"] != 1, "benchmark retry_limit must equal 1")
    |> add_error(not valid_shape_source?(benchmark["shape_source"]), "shape_source is invalid")
    |> add_error(
      get_in(spec, ["iteration_sampling", "max_initial_cases"]) != 10,
      "iteration_sampling max_initial_cases must equal 10"
    )
    |> add_error(
      stopping["mode"] not in @stop_modes,
      "stopping mode must be all_goals or any_goal"
    )
  end

  defp valid_case?(case_) do
    slug?(case_["id"]) and present?(case_["name"]) and case_["kind"] in @case_kinds and
      is_map(case_["shape"]) and map_size(case_["shape"]) > 0 and
      structured_value?(case_["dtype"]) and
      structured_value?(case_["layout"])
  end

  defp range_shape?(value) when is_map(value) do
    (Map.has_key?(value, "min") and Map.has_key?(value, "max") and
       value["min"] != value["max"]) or Enum.any?(Map.values(value), &range_shape?/1)
  end

  defp range_shape?(value) when is_list(value), do: Enum.any?(value, &range_shape?/1)
  defp range_shape?(_value), do: false

  defp valid_shape_source?(nil), do: true

  defp valid_shape_source?(source) when is_map(source) do
    source["kind"] in @shape_source_kinds and nullable_relative_path?(source["path"]) and
      nullable_positive_integer?(source["record_count"]) and present?(source["summary"])
  end

  defp valid_shape_source?(_source), do: false

  defp nullable_relative_path?(nil), do: true

  defp nullable_relative_path?(value) when is_binary(value) do
    present?(value) and Path.type(value) == :relative and ".." not in Path.split(value)
  end

  defp nullable_relative_path?(_value), do: false

  defp nullable_positive_integer?(nil), do: true
  defp nullable_positive_integer?(value), do: positive_integer?(value)

  defp metric_errors(metrics) do
    metrics
    |> Enum.with_index()
    |> Enum.flat_map(fn
      {metric, index} when is_map(metric) ->
        prefix = "metrics[#{index}]"

        [
          {"#{prefix}.id: must be a stable slug matching [a-z][a-z0-9_-]*", slug?(metric["id"])},
          {"#{prefix}.name: must be a non-empty string", present?(metric["name"])},
          {"#{prefix}.unit: must be a non-empty string", present?(metric["unit"])},
          {"#{prefix}.direction: must be one of #{Enum.join(@directions, ", ")}",
           metric["direction"] in @directions},
          {"#{prefix}.role: must be one of #{Enum.join(@metric_roles, ", ")}",
           metric["role"] in @metric_roles},
          {"#{prefix}.min_improvement_ratio: must be a number greater than or equal to 0.01",
           number?(metric["min_improvement_ratio"]) and
             metric["min_improvement_ratio"] >= 0.01}
        ]
        |> for_failed_checks()

      {_metric, index} ->
        ["metrics[#{index}]: must be an object"]
    end)
  end

  defp duplicate_metric_id_errors(metrics) do
    metrics
    |> Enum.with_index()
    |> Enum.reduce({%{}, []}, fn
      {metric, index}, {seen, errors} when is_map(metric) ->
        id = metric["id"]

        if slug?(id) do
          case Map.fetch(seen, id) do
            {:ok, first_index} ->
              {seen,
               errors ++
                 ["metrics[#{index}].id: duplicates metrics[#{first_index}].id #{inspect(id)}"]}

            :error ->
              {Map.put(seen, id, index), errors}
          end
        else
          {seen, errors}
        end

      {_metric, _index}, acc ->
        acc
    end)
    |> elem(1)
  end

  defp for_failed_checks(checks),
    do: for({message, false} <- checks, do: message)

  defp unique_ids?(items) do
    ids = Enum.map(items, & &1["id"])
    Enum.all?(ids, &slug?/1) and length(ids) == length(Enum.uniq(ids))
  end

  defp valid_stopping?(value) do
    stopping = map(value)
    positive_integer?(stopping["max_attempts"]) or nonempty_list?(stopping["metric_goals"])
  end

  defp add_error(errors, true, message), do: [message | errors]
  defp add_error(errors, false, _message), do: errors

  defp slug?(value), do: is_binary(value) and Regex.match?(~r/^[a-z][a-z0-9_-]*$/, value)
  defp present?(value), do: is_binary(value) and String.trim(value) != ""
  defp structured_value?(value) when is_binary(value), do: present?(value)
  defp structured_value?(value) when is_map(value), do: map_size(value) > 0
  defp structured_value?(value) when is_list(value), do: value != []
  defp structured_value?(_), do: false
  defp number?(value), do: is_integer(value) or is_float(value)
  defp positive_integer?(value), do: is_integer(value) and value > 0
  defp nonempty_list?(value), do: is_list(value) and value != []
  defp list(value) when is_list(value), do: value
  defp list(_), do: []
  defp map(value) when is_map(value), do: value
  defp map(_), do: %{}

  defp stringify_keys(map) when is_map(map) do
    Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)
  end

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp diff_paths(left, right, path) when is_map(left) and is_map(right) do
    (Map.keys(left) ++ Map.keys(right))
    |> Enum.uniq()
    |> Enum.flat_map(fn key ->
      diff_paths(
        Map.get(left, key, :__missing__),
        Map.get(right, key, :__missing__),
        path ++ [to_string(key)]
      )
    end)
  end

  defp diff_paths(value, value, _path), do: []
  defp diff_paths(_left, _right, path), do: [Enum.join(path, ".")]

  defp string_schema, do: %{"type" => "string", "minLength" => 1}
  defp number_schema, do: %{"type" => "number", "minimum" => 0}
  defp slug_schema, do: %{"type" => "string", "pattern" => "^[a-z][a-z0-9_-]*$"}

  defp object_array_schema,
    do: %{"type" => "array", "minItems" => 1, "items" => %{"type" => "object"}}
end
