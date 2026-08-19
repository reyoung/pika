defmodule Pika.Paths do
  @moduledoc false

  def canonical(path) when is_binary(path) do
    expanded = Path.expand(path)

    case existing_directory(expanded, []) do
      {:ok, directory, suffix} ->
        case System.cmd("pwd", ["-P"], cd: directory, stderr_to_stdout: true) do
          {physical, 0} -> {:ok, Path.join([String.trim(physical) | suffix])}
          {output, status} -> {:error, {:canonical_path_failed, expanded, status, output}}
        end

      error ->
        error
    end
  end

  def canonical!(path) do
    case canonical(path) do
      {:ok, canonical} -> canonical
      {:error, reason} -> raise ArgumentError, "cannot canonicalize path: #{inspect(reason)}"
    end
  end

  defp existing_directory(path, suffix) do
    cond do
      File.dir?(path) ->
        {:ok, path, suffix}

      path == Path.dirname(path) ->
        {:error, {:no_existing_parent, path}}

      true ->
        existing_directory(Path.dirname(path), [Path.basename(path) | suffix])
    end
  end
end
