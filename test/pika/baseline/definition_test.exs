defmodule Pika.Baseline.DefinitionTest do
  use ExUnit.Case, async: true

  alias Pika.Baseline.Definition

  @sha String.duplicate("a", 40)

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-baseline-definition-#{System.unique_integer([:positive])}"
      )

    File.mkdir_p!(Path.join(root, "target"))
    on_exit(fn -> File.rm_rf!(root) end)
    write_valid_bundle(root)
    %{root: root}
  end

  test "validates a complete file-based Baseline Definition bundle", %{root: root} do
    assert {:ok, definition} = Definition.validate(root, "baseline-definition.json")

    assert definition.manifest["development_baseline"]["commit_sha"] == @sha
    assert Enum.map(definition.cases, & &1["id"]) == [0, 1]
    assert Enum.map(definition.metrics, & &1["id"]) == ["latency_us", "bandwidth_gbps"]
    assert map_size(definition.dependency_receipts) == 6
    assert byte_size(definition.dependencies_sha256) == 64
  end

  test "rejects a tampered Target bundle", %{root: root} do
    File.write!(Path.join(root, "target/target.py"), "tampered\n")

    assert {:error, {:target_file_identity_mismatch, "target/target.py", _identity}} =
             Definition.validate(root, "baseline-definition.json")
  end

  test "requires contiguous integer Cases from zero", %{root: root} do
    cases = read_json(root, "cases.json")
    cases = put_in(cases, ["cases", Access.at(1), "id"], 3)
    write_json(root, "cases.json", cases)

    assert {:error, :case_ids_not_contiguous_from_zero} =
             Definition.validate(root, "baseline-definition.json")
  end

  test "requires smoke coverage for every Metric", %{root: root} do
    [first | _] = read_jsonl(root, "smoke-benchmark.jsonl")
    write_jsonl(root, "smoke-benchmark.jsonl", [first])

    assert {:error, :smoke_benchmark_metric_coverage} =
             Definition.validate(root, "baseline-definition.json")
  end

  test "requires the Target manifest to cover every Target file", %{root: root} do
    File.write!(Path.join(root, "target/unlisted.py"), "hidden dependency\n")

    assert {:error, {:target_manifest_coverage_mismatch, coverage}} =
             Definition.validate(root, "baseline-definition.json")

    assert coverage.actual == ["target.py", "unlisted.py"]
    assert coverage.expected == ["target.py"]
  end

  defp write_valid_bundle(root) do
    write_executable(root, "verify_cases.sh")
    write_executable(root, "benchmark_cases.sh")

    target_contents = "def run():\n    return 1\n"
    File.write!(Path.join(root, "target/target.py"), target_contents)

    write_json(root, "target/manifest.json", %{
      "schema_version" => 1,
      "source_kind" => "generated_bundle",
      "source_description" => "test target",
      "source_commit_sha" => nil,
      "entrypoint" => "target.py:run",
      "files" => [
        %{
          "path" => "target.py",
          "sha256" => sha256(target_contents),
          "size" => byte_size(target_contents)
        }
      ]
    })

    write_json(root, "cases.json", %{
      "schema_version" => 1,
      "cases" => [
        %{
          "id" => 0,
          "name" => "small",
          "description" => "small case",
          "inputs" => %{"m" => 16},
          "weight" => 1.0,
          "critical" => true
        },
        %{
          "id" => 1,
          "name" => "large",
          "description" => "large case",
          "inputs" => %{"m" => 64},
          "weight" => 2.0,
          "critical" => false
        }
      ]
    })

    write_json(root, "metrics.json", %{
      "schema_version" => 1,
      "metrics" => [
        %{
          "id" => "latency_us",
          "unit" => "us",
          "direction" => "minimize",
          "role" => "primary"
        },
        %{
          "id" => "bandwidth_gbps",
          "unit" => "GB/s",
          "direction" => "maximize",
          "role" => "informational"
        }
      ]
    })

    write_json(root, "smoke-verify.json", %{
      "schema_version" => 1,
      "requested_case_ids" => [0],
      "cases" => [
        %{
          "case_id" => 0,
          "target" => %{"passed" => true},
          "candidate" => %{"passed" => true},
          "comparison" => %{"passed" => true},
          "error" => nil
        }
      ]
    })

    write_jsonl(root, "smoke-benchmark.jsonl", [
      benchmark_record("latency_us", 10.0, 12.0),
      benchmark_record("bandwidth_gbps", 100.0, 90.0)
    ])

    write_json(root, "baseline-definition.json", %{
      "schema_version" => 1,
      "summary" => "test baseline",
      "optimization_target" => %{
        "manifest_path" => "target/manifest.json",
        "entrypoint" => "target.py:run"
      },
      "development_baseline" => %{"commit_sha" => @sha, "entrypoint" => "kernel.py:run"},
      "correctness" => %{"mode" => "target_equivalence"},
      "verify" => %{"script" => "verify_cases.sh", "output_schema_version" => 1},
      "benchmark" => %{"script" => "benchmark_cases.sh", "output_schema_version" => 1},
      "cases_path" => "cases.json",
      "metrics_path" => "metrics.json",
      "measurement" => %{"warmup" => 2, "pair_count" => 3, "min_valid_pairs" => 2},
      "stopping" => %{
        "mode" => "manual",
        "max_attempts" => nil,
        "max_duration_seconds" => nil
      },
      "smoke_verify_path" => "smoke-verify.json",
      "smoke_benchmark_path" => "smoke-benchmark.jsonl"
    })
  end

  defp benchmark_record(metric_id, target, candidate) do
    %{
      "schema_version" => 1,
      "case_id" => 0,
      "metric_id" => metric_id,
      "pair_index" => 0,
      "order" => "target_candidate",
      "target" => target,
      "candidate" => candidate,
      "valid" => true,
      "error" => nil
    }
  end

  defp write_executable(root, path) do
    absolute = Path.join(root, path)
    File.write!(absolute, "#!/usr/bin/env bash\nexit 0\n")
    File.chmod!(absolute, 0o755)
  end

  defp write_json(root, path, value), do: File.write!(Path.join(root, path), Jason.encode!(value))

  defp write_jsonl(root, path, values) do
    File.write!(Path.join(root, path), Enum.map_join(values, "", &(Jason.encode!(&1) <> "\n")))
  end

  defp read_json(root, path), do: root |> Path.join(path) |> File.read!() |> Jason.decode!()

  defp read_jsonl(root, path) do
    root
    |> Path.join(path)
    |> File.stream!()
    |> Enum.map(&Jason.decode!/1)
  end

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
