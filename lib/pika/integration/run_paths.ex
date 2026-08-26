defmodule Pika.Integration.RunPaths do
  @moduledoc "Stable, run-scoped output paths for immutable Integration artifacts."

  @spec for_run(pos_integer()) :: map()
  def for_run(run_sequence) when is_integer(run_sequence) and run_sequence > 0 do
    root = Path.join(["integration", "runs", padded(run_sequence)])

    %{
      root: root,
      validation: Path.join(root, "integration-validation.json"),
      verify: Path.join(root, "verify-full.json"),
      benchmark: Path.join(root, "benchmark-full.jsonl"),
      result: Path.join(root, "integration-result.json")
    }
  end

  defp padded(sequence), do: sequence |> Integer.to_string() |> String.pad_leading(6, "0")
end
