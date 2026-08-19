defmodule Pika.Alignment.ArtifactStore do
  @moduledoc false

  def register(workspace_root, relative_path, attrs \\ %{}) do
    with {:ok, absolute} <- resolve(workspace_root, relative_path),
         {:ok, stat} <- File.stat(absolute),
         true <- stat.type == :regular,
         {:ok, sha256, hashed_size} <- hash_file(absolute),
         true <- hashed_size == stat.size do
      expected_sha = attr(attrs, :sha256)
      expected_size = attr(attrs, :size)

      cond do
        expected_sha && expected_sha != sha256 ->
          {:error, :sha256_mismatch}

        expected_size && expected_size != stat.size ->
          {:error, :size_mismatch}

        true ->
          {:ok,
           %{
             id: Pika.AgentBackend.Id.new("artifact"),
             kind: attr(attrs, :kind, "generic"),
             relative_path: Path.relative_to(absolute, Path.expand(workspace_root)),
             sha256: sha256,
             size: hashed_size,
             mime: attr(attrs, :mime, MIME.from_path(absolute)),
             metadata: attr(attrs, :metadata, %{})
           }}
      end
    else
      false -> {:error, :not_a_regular_file}
      {:error, reason} -> {:error, reason}
    end
  end

  def verified(workspace_root, relative_path, sha256, size, attrs \\ %{})
      when is_binary(sha256) and is_integer(size) do
    with {:ok, absolute} <- resolve(workspace_root, relative_path),
         {:ok, stat} <- File.stat(absolute),
         true <- stat.type == :regular,
         true <- stat.size == size do
      {:ok,
       %{
         id: Pika.AgentBackend.Id.new("artifact"),
         kind: attr(attrs, :kind, "generic"),
         relative_path: Path.relative_to(absolute, Path.expand(workspace_root)),
         sha256: sha256,
         size: size,
         mime: attr(attrs, :mime, MIME.from_path(absolute)),
         metadata: attr(attrs, :metadata, %{})
       }}
    else
      false -> {:error, :artifact_changed_during_validation}
      {:error, reason} -> {:error, reason}
    end
  end

  def copy_upload(workspace_root, source_path, filename) do
    safe_name = filename |> Path.basename() |> String.replace(~r/[^A-Za-z0-9._-]/, "_")

    relative =
      Path.join(["artifacts", "inputs", "#{System.unique_integer([:positive])}-#{safe_name}"])

    with {:ok, destination} <- resolve(workspace_root, relative),
         :ok <- File.mkdir_p(Path.dirname(destination)),
         {:ok, _bytes} <- File.copy(source_path, destination) do
      register(workspace_root, relative, %{kind: "input"})
    end
  end

  def delete_uploads(workspace_root, artifacts) when is_list(artifacts) do
    Enum.each(artifacts, fn artifact ->
      case resolve(workspace_root, artifact.relative_path) do
        {:ok, absolute} -> File.rm(absolute)
        {:error, _reason} -> :ok
      end
    end)

    :ok
  end

  def resolve(workspace_root, relative_path) when is_binary(relative_path) do
    root = Path.expand(workspace_root)

    cond do
      Path.type(relative_path) == :absolute ->
        {:error, :absolute_path_forbidden}

      Enum.any?(Path.split(relative_path), &(&1 == "..")) ->
        {:error, :path_escape}

      true ->
        absolute = Path.expand(relative_path, root)

        if absolute == root or String.starts_with?(absolute, root <> "/") do
          {:ok, absolute}
        else
          {:error, :path_escape}
        end
    end
  end

  defp hash_file(path) do
    path
    |> File.stream!([], 1_048_576)
    |> Enum.reduce({:crypto.hash_init(:sha256), 0}, fn chunk, {context, size} ->
      {:crypto.hash_update(context, chunk), size + byte_size(chunk)}
    end)
    |> then(fn {context, size} ->
      sha256 = context |> :crypto.hash_final() |> Base.encode16(case: :lower)
      {:ok, sha256, size}
    end)
  rescue
    File.Error -> {:error, :artifact_unreadable}
  end

  defp attr(attrs, key, default \\ nil)
  defp attr(attrs, key, default) when is_list(attrs), do: Keyword.get(attrs, key, default)

  defp attr(attrs, key, default) when is_map(attrs),
    do: Map.get(attrs, key) || Map.get(attrs, Atom.to_string(key)) || default
end
