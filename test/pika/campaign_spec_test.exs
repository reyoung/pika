defmodule Pika.CampaignSpecTest do
  use ExUnit.Case, async: true

  alias Pika.CampaignSpec, as: Spec
  alias Pika.Test.AlignmentFixtures

  test "accepts a complete Campaign Spec v2 and applies non-sampling defaults" do
    result =
      AlignmentFixtures.spec()
      |> Map.update!("metrics", &[Map.delete(hd(&1), "min_improvement_ratio")])
      |> Spec.validate()

    assert result.ready?
    assert result.missing == []
    assert result.errors == []
    assert hd(result.spec["metrics"])["min_improvement_ratio"] == 0.01
    assert result.spec["iteration_sampling"] == %{"max_initial_cases" => 10}
  end

  test "requires explicit Oracle, Optimization Target, and Development roles" do
    spec = AlignmentFixtures.spec()

    without_roles =
      spec
      |> Map.put("schema_version", 1)
      |> Map.delete("implementations")
      |> put_in(["computation", "reference_path"], "kernel/reference.py")
      |> Spec.validate()

    refute without_roles.ready?
    assert "implementations.oracle" in without_roles.missing
    assert "implementations.optimization_target" in without_roles.missing
    assert "implementations.development" in without_roles.missing
    assert "schema_version must be 2" in without_roles.errors
    refute Map.has_key?(without_roles.spec, "implementations")

    same_oracle_and_development =
      spec
      |> put_in(["implementations", "oracle"], %{
        "kind" => "repository_path",
        "entrypoint" => "kernel/development.py"
      })
      |> Spec.validate()

    refute same_oracle_and_development.ready?

    assert "repository_path Oracle and Development entrypoints must be different; use optimization_target when they share a snapshot" in same_oracle_and_development.errors
  end

  test "allows an Optimization Target from an explicitly selected Reference Project" do
    result =
      AlignmentFixtures.spec()
      |> Map.put("reference_ids", ["fa4"])
      |> put_in(["implementations", "optimization_target"], %{
        "source" => %{"kind" => "reference_project", "reference_id" => "fa4"},
        "entrypoint" => "src/flash_attention.py"
      })
      |> Spec.validate()

    assert result.ready?

    missing_selection =
      result.spec
      |> Map.put("reference_ids", [])
      |> Spec.validate()

    refute missing_selection.ready?

    assert "implementations.optimization_target.source.reference_id must be selected in reference_ids" in missing_selection.errors
  end

  test "accepts an arbitrary user-specified formal pair count and validity floor" do
    result =
      AlignmentFixtures.spec()
      |> put_in(["benchmark", "pair_count"], 1_000_003)
      |> put_in(["benchmark", "min_valid_pairs"], 900_001)
      |> Spec.validate()

    assert result.ready?
    assert result.spec["benchmark"]["pair_count"] == 1_000_003
    assert result.spec["benchmark"]["min_valid_pairs"] == 900_001

    pair_schema =
      get_in(Spec.json_schema(), ["properties", "benchmark", "properties", "pair_count"])

    assert pair_schema == %{"type" => "integer", "minimum" => 1}
  end

  test "does not invent a formal pair count or validity floor" do
    result =
      AlignmentFixtures.spec()
      |> update_in(["benchmark"], &Map.drop(&1, ~w(pair_count min_valid_pairs)))
      |> Spec.validate()

    refute result.ready?
    assert "benchmark.pair_count" in result.missing
    assert "benchmark.min_valid_pairs" in result.missing
    refute Map.has_key?(result.spec["benchmark"], "pair_count")
    refute Map.has_key?(result.spec["benchmark"], "min_valid_pairs")
  end

  test "rejects invalid Campaign-specific pair thresholds" do
    result =
      AlignmentFixtures.spec()
      |> put_in(["benchmark", "pair_count"], 8)
      |> put_in(["benchmark", "min_valid_pairs"], 9)
      |> Spec.validate()

    refute result.ready?
    assert "benchmark min_valid_pairs must not exceed pair_count" in result.errors
  end

  test "reports missing fields and semantic gate failures" do
    result = Spec.validate(%{"title" => "Incomplete", "benchmark_cases" => [], "metrics" => []})
    refute result.ready?
    assert "target_hardware" in result.missing
    assert "at least one target Benchmark Case is required" in result.errors
    assert "at least one target Metric is required" in result.errors
  end

  test "rejects non-target-only and non-unique identifiers" do
    spec = AlignmentFixtures.spec()
    metric = hd(spec["metrics"])
    guard = %{metric | "role" => "guard"}
    bad = %{spec | "metrics" => [guard, guard]}
    result = Spec.validate(bad)
    refute result.ready?
    assert "at least one target Metric is required" in result.errors

    assert "metrics[1].id: duplicates metrics[0].id \"latency_us\"" in result.errors
  end

  test "reports every invalid Metric field with its exact array path" do
    spec = AlignmentFixtures.spec()

    invalid_metric = %{
      "id" => "Bad Metric ID",
      "name" => "",
      "unit" => 42,
      "direction" => "sideways",
      "role" => "optional",
      "min_improvement_ratio" => 0.001
    }

    result = Spec.validate(%{spec | "metrics" => [invalid_metric, "not-an-object"]})

    refute result.ready?
    assert "metrics[0].id: must be a stable slug matching [a-z][a-z0-9_-]*" in result.errors
    assert "metrics[0].name: must be a non-empty string" in result.errors
    assert "metrics[0].unit: must be a non-empty string" in result.errors
    assert "metrics[0].direction: must be one of minimize, maximize" in result.errors
    assert "metrics[0].role: must be one of target, guard, informational" in result.errors

    assert "metrics[0].min_improvement_ratio: must be a number greater than or equal to 0.01" in result.errors

    assert "metrics[1]: must be an object" in result.errors
    refute "Metrics have invalid fields" in result.errors
  end

  test "reports deterministic nested paths for Spec diff" do
    old = AlignmentFixtures.spec()

    new =
      old
      |> put_in(["computation", "fusion_scope"], "two kernels")
      |> put_in(["stopping", "max_attempts"], 20)

    diff = Spec.diff(old, new)
    assert diff.changed_paths == ["computation.fusion_scope", "stopping.max_attempts"]
  end

  test "accepts structured dtype and layout contracts" do
    spec = AlignmentFixtures.spec()
    [case_] = spec["benchmark_cases"]

    structured = %{
      spec
      | "benchmark_cases" => [
          %{
            case_
            | "dtype" => %{"activation" => "bfloat16", "accumulation" => "float32"},
              "layout" => ["contiguous", "paged_kv"]
          }
        ]
    }

    assert Spec.validate(structured).ready?
  end

  test "rejects grouped min/max Shapes and requires concrete Cases" do
    spec = AlignmentFixtures.spec()
    [case_] = spec["benchmark_cases"]
    grouped = %{case_ | "shape" => %{"batch" => %{"min" => 1, "max" => 32}}}
    result = Spec.validate(%{spec | "benchmark_cases" => [grouped]})
    refute result.ready?

    assert "Benchmark Case shapes must be concrete; split min/max ranges into stable Case IDs" in result.errors
  end

  test "accepts a relative Shape source and arbitrary Metric names and units" do
    spec =
      AlignmentFixtures.spec()
      |> put_in(["benchmark", "shape_source"], %{
        "kind" => "artifact",
        "path" => "artifacts/inputs/shapes.pkl",
        "record_count" => 5_496,
        "summary" => "production rank-0 dump"
      })
      |> put_in(["metrics"], [
        %{
          "id" => "useful_tokens_per_joule",
          "name" => "Useful tokens per joule",
          "unit" => "token/J",
          "direction" => "maximize",
          "role" => "target",
          "min_improvement_ratio" => 0.01
        }
      ])

    assert Spec.validate(spec).ready?
  end

  test "preserves target, guard, and informational gates with explicit directions" do
    spec = AlignmentFixtures.spec()
    [target_case] = spec["benchmark_cases"]
    [target_metric] = spec["metrics"]

    cases = [
      target_case,
      %{target_case | "id" => "guard_case", "name" => "Guard case", "kind" => "guard"},
      %{
        target_case
        | "id" => "info_case",
          "name" => "Informational case",
          "kind" => "informational"
      }
    ]

    metrics = [
      target_metric,
      %{
        target_metric
        | "id" => "throughput",
          "name" => "Throughput",
          "unit" => "items/s",
          "direction" => "maximize",
          "role" => "guard"
      },
      %{
        target_metric
        | "id" => "workspace_bytes",
          "name" => "Workspace",
          "unit" => "bytes",
          "role" => "informational"
      }
    ]

    result = Spec.validate(%{spec | "benchmark_cases" => cases, "metrics" => metrics})
    assert result.ready?

    assert Enum.map(result.spec["benchmark_cases"], & &1["kind"]) ==
             ~w(target guard informational)

    assert Enum.map(result.spec["metrics"], &{&1["role"], &1["direction"]}) == [
             {"target", "minimize"},
             {"guard", "maximize"},
             {"informational", "minimize"}
           ]
  end
end
