defmodule Pika.CampaignSpecTest do
  use ExUnit.Case, async: true

  alias Pika.CampaignSpec, as: Spec
  alias Pika.Test.AlignmentFixtures

  test "accepts a complete Campaign Spec v1 and applies fixed defaults" do
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
    assert "Metric ids must be unique stable slugs" in result.errors
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
