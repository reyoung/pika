defmodule Pika.Optimization.SchemaValidatorTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePromptRegistry
  alias Pika.Optimization.SchemaValidator

  @sha String.duplicate("a", 40)
  @digest String.duplicate("b", 64)
  @project_root Path.expand("../../..", __DIR__)

  setup_all do
    assert {:ok, schemas} = RolePromptRegistry.schemas()
    %{schemas: schemas}
  end

  test "validates Baseline Definition and reports JSON Pointer errors", %{schemas: schemas} do
    schema = schema(schemas.baseline_definition)

    assert :ok = SchemaValidator.validate(baseline_definition(), schema)

    invalid =
      baseline_definition()
      |> Map.delete("summary")
      |> Map.put("unexpected", true)

    assert {:error, errors} = SchemaValidator.validate(invalid, schema)
    assert %{path: "/summary", message: "required property is missing"} in errors
    assert %{path: "/unexpected", message: "additional property is not allowed"} in errors
  end

  test "validates accepted and rejected Baseline Verification branches", %{schemas: schemas} do
    schema = schema(schemas.baseline_verification_result)

    accepted = %{
      "schema_version" => 1,
      "role" => "baseline_verify",
      "work_id" => "baseline-revision-1",
      "outcome" => "accepted",
      "summary" => "full baseline accepted",
      "files" => %{
        "verify" => file("verify.json"),
        "benchmark" => file("benchmark.jsonl")
      },
      "details" => %{
        "baseline_revision" => 1,
        "definition_sha256" => @digest,
        "development_sha" => @sha,
        "initial_iteration_case_ids" => [0],
        "case_selection_reasons" => [%{"case_id" => 0, "reason" => "representative"}],
        "case_metrics" => [
          %{
            "case_id" => 0,
            "metric_id" => "latency_us",
            "unit" => "us",
            "target_value" => 10.0,
            "development_value" => 12.0,
            "relative_difference" => 0.2,
            "noise_tolerance" => 0.005,
            "valid_pair_count" => 20
          }
        ],
        "judgement" => %{"reasonable" => true, "reason" => "stable"}
      }
    }

    assert :ok = SchemaValidator.validate(accepted, schema)

    rejected =
      @project_root
      |> Path.join(
        "test/fixtures/v2/roles/baseline_alignment/verification-failures/verification-01.json"
      )
      |> File.read!()
      |> Jason.decode!()

    assert :ok = SchemaValidator.validate(rejected, schema)

    invalid = put_in(accepted, ["details", "case_metrics"], [])
    assert {:error, errors} = SchemaValidator.validate(invalid, schema)
    assert Enum.any?(errors, &(&1.path == "/details/case_metrics"))
  end

  test "validates Iteration, Verify, and Benchmark schemas", %{schemas: schemas} do
    iteration = %{
      "schema_version" => 1,
      "role" => "iteration",
      "work_id" => "1",
      "outcome" => "rejected",
      "summary" => "no gain",
      "files" => %{},
      "details" => %{
        "attempt_id" => 1,
        "iteration_round" => 1,
        "base_sha" => @sha,
        "candidate_sha" => nil,
        "sampling_revision" => 0,
        "hypothesis" => "vectorize",
        "changes" => [],
        "risks" => [],
        "failure_reason" => "no improvement"
      }
    }

    assert :ok = SchemaValidator.validate(iteration, schema(schemas.iteration_result))

    verify = %{
      "schema_version" => 1,
      "requested_case_ids" => [0],
      "cases" => [
        %{
          "case_id" => 0,
          "target" => %{"passed" => true},
          "candidate" => %{"passed" => true},
          "comparison" => %{"passed" => true, "max_abs_error" => 0.0},
          "error" => nil
        }
      ]
    }

    assert :ok = SchemaValidator.validate(verify, schema(schemas.verify_result))

    record = %{
      "schema_version" => 1,
      "case_id" => 0,
      "metric_id" => "latency_us",
      "pair_index" => 0,
      "order" => "target_candidate",
      "target" => 10.0,
      "candidate" => 12.0,
      "valid" => true,
      "error" => nil
    }

    assert :ok = SchemaValidator.validate(record, schema(schemas.benchmark_record))
  end

  test "accepts guard as a Metric role", %{schemas: schemas} do
    metric = %{
      "schema_version" => 1,
      "metrics" => [
        %{
          "id" => "accuracy",
          "unit" => "ratio",
          "direction" => "maximize",
          "role" => "guard"
        }
      ]
    }

    assert :ok = SchemaValidator.validate(metric, schema(schemas.metrics))
  end

  defp baseline_definition do
    %{
      "schema_version" => 1,
      "summary" => "baseline",
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
      "measurement" => %{"warmup" => 20, "pair_count" => 20, "min_valid_pairs" => 15},
      "stopping" => %{
        "mode" => "manual",
        "max_attempts" => nil,
        "max_duration_seconds" => nil
      },
      "smoke_verify_path" => "smoke-verify.json",
      "smoke_benchmark_path" => "smoke-benchmark.jsonl"
    }
  end

  defp file(path), do: %{"path" => path, "sha256" => @digest}
  defp schema(path), do: path |> File.read!() |> Jason.decode!()
end
