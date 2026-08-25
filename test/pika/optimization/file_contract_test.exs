defmodule Pika.Optimization.FileContractTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePromptRegistry
  alias Pika.Optimization.FileContract

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-file-contract-#{System.unique_integer([:positive])}")

    File.mkdir_p!(Path.join(root, "artifacts"))
    on_exit(fn -> File.rm_rf!(root) end)
    assert {:ok, schemas} = RolePromptRegistry.schemas()
    %{root: root, schemas: schemas}
  end

  test "validates JSON and returns Pika-computed file identity", %{root: root, schemas: schemas} do
    relative = "artifacts/verify.json"
    contents = Jason.encode!(verify_result())
    File.write!(Path.join(root, relative), contents)

    assert {:ok, value, receipt} =
             FileContract.validate_json(root, relative, schemas.verify_result)

    assert value == verify_result()
    assert receipt.relative_path == relative
    assert receipt.byte_size == byte_size(contents)
    assert receipt.sha256 == sha256(contents)
  end

  test "accepts an absolute path inside the work root and records it as relative", %{root: root} do
    relative = "artifacts/value.json"
    absolute = Path.join(root, relative)
    File.write!(absolute, "{}")

    assert {:ok, "{}", receipt} = FileContract.read(root, absolute)
    assert receipt.relative_path == relative
    assert receipt.absolute_path == absolute
  end

  test "rejects outside-root, escaping, noncanonical, symlink, and oversized paths", %{root: root} do
    File.write!(Path.join(root, "artifacts/value.json"), "{}")

    assert {:error, :absolute_file_path} = FileContract.read(root, "/tmp/value.json")

    assert {:error, :absolute_file_path} =
             FileContract.read(root, Path.join(root <> "-outside", "value.json"))

    assert {:error, :file_path_escape} = FileContract.read(root, "../value.json")
    assert {:error, :noncanonical_file_path} = FileContract.read(root, "artifacts//value.json")

    File.ln_s!(Path.join(root, "artifacts/value.json"), Path.join(root, "artifacts/link.json"))

    assert {:error, {:symlink_component, _path}} =
             FileContract.read(root, "artifacts/link.json")

    assert {:error, {:symlink_component, _path}} =
             FileContract.read(root, Path.join(root, "artifacts/link.json"))

    assert {:error, {:file_too_large, 2, 1}} =
             FileContract.read(root, "artifacts/value.json", max_bytes: 1)
  end

  test "validates every JSONL record and reports its line", %{root: root, schemas: schemas} do
    relative = "artifacts/benchmark.jsonl"
    valid = benchmark_record(0)
    File.write!(Path.join(root, relative), Jason.encode!(valid) <> "\n")

    assert {:ok, [^valid], _receipt} =
             FileContract.validate_jsonl(root, relative, schemas.benchmark_record)

    invalid = Map.delete(benchmark_record(1), "metric_id")

    File.write!(
      Path.join(root, relative),
      Jason.encode!(valid) <> "\n" <> Jason.encode!(invalid) <> "\n"
    )

    assert {:error, {:jsonl_schema_validation_failed, 2, errors}} =
             FileContract.validate_jsonl(root, relative, schemas.benchmark_record)

    assert Enum.any?(errors, &(&1.path == "/metric_id"))
  end

  defp verify_result do
    %{
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
    }
  end

  defp benchmark_record(index) do
    %{
      "schema_version" => 1,
      "case_id" => 0,
      "metric_id" => "latency_us",
      "pair_index" => index,
      "order" => if(rem(index, 2) == 0, do: "target_candidate", else: "candidate_target"),
      "target" => 10.0,
      "candidate" => 12.0,
      "valid" => true,
      "error" => nil
    }
  end

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
