defmodule Pika.Stage0.Harness do
  @moduledoc false

  def validate(setup_root, attrs) do
    attrs = stringify_keys(attrs)

    paths =
      [attrs["reference_path"], attrs["benchmark_path"]] ++
        List.wrap(attrs["correctness_paths"]) ++ List.wrap(attrs["protected_paths"])

    paths = Enum.reject(paths, &is_nil/1) |> Enum.uniq()

    with true <- is_binary(attrs["reference_path"]),
         true <- is_binary(attrs["benchmark_path"]),
         true <- List.wrap(attrs["correctness_paths"]) != [],
         {:ok, files} <- validate_files(setup_root, paths) do
      manifest = Enum.map(files, fn {path, sha} -> %{"path" => path, "sha256" => sha} end)
      digest = :crypto.hash(:sha256, Jason.encode!(manifest)) |> Base.encode16(case: :lower)

      {:ok,
       %{
         reference_path: attrs["reference_path"],
         benchmark_path: attrs["benchmark_path"],
         correctness_paths: List.wrap(attrs["correctness_paths"]),
         protected_paths: paths,
         manifest: manifest,
         digest: digest
       }}
    else
      false -> {:error, :missing_harness_paths}
      {:error, reason} -> {:error, reason}
    end
  end

  def verify_digest(setup_root, harness) do
    case validate(setup_root, %{
           reference_path: harness.reference_path,
           benchmark_path: harness.benchmark_path,
           correctness_paths: harness.correctness_paths,
           protected_paths: harness.protected_paths
         }) do
      {:ok, %{digest: digest}} when digest == harness.digest -> :ok
      _ -> {:error, :protected_digest_changed}
    end
  end

  defp validate_files(root, paths) do
    Enum.reduce_while(paths, {:ok, []}, fn path, {:ok, acc} ->
      case safe_file(root, path) do
        {:ok, sha} -> {:cont, {:ok, [{path, sha} | acc]}}
        error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, files} -> {:ok, Enum.sort(files)}
      error -> error
    end
  end

  defp safe_file(root, relative) when is_binary(relative) do
    cond do
      Path.type(relative) == :absolute ->
        {:error, {:absolute_path, relative}}

      Enum.any?(Path.split(relative), &(&1 == "..")) ->
        {:error, {:path_escape, relative}}

      true ->
        absolute = Path.expand(relative, root)
        root = Path.expand(root)

        with true <- String.starts_with?(absolute, root <> "/"),
             {:ok, info} <- :file.read_link_info(String.to_charlist(absolute)),
             true <- elem(info, 2) == :regular,
             {:ok, contents} <- File.read(absolute) do
          {:ok, :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)}
        else
          _ -> {:error, {:invalid_harness_file, relative}}
        end
    end
  end

  defp safe_file(_root, path), do: {:error, {:invalid_path, path}}

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
