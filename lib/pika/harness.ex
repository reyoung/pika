defmodule Pika.Harness do
  @moduledoc false

  @reference_preview_bytes 262_144
  @hash_chunk_bytes 1_048_576

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

  def reference_review(setup_root, harness, opts \\ []) do
    preview_bytes = Keyword.get(opts, :preview_bytes, @reference_preview_bytes)

    with :ok <- validate_preview_bytes(preview_bytes),
         {:ok, expected_sha} <- reference_manifest_sha(harness),
         {:ok, absolute, size} <- safe_regular_path(setup_root, harness.reference_path),
         {:ok, sha256, content} <- hash_and_preview(absolute, preview_bytes),
         :ok <- verify_reference_sha(sha256, expected_sha),
         {:ok, content} <- normalize_utf8_preview(content, size > byte_size(content)) do
      {:ok,
       %{
         path: harness.reference_path,
         sha256: sha256,
         size: size,
         content: content,
         preview_bytes: byte_size(content),
         truncated: size > byte_size(content)
       }}
    else
      {:error, _reason} = error -> error
    end
  end

  def verify_candidate(repo, base_sha, candidate_sha, harness) do
    with {:ok, changed} <-
           Pika.Git.run(repo, [
             "diff",
             "--name-only",
             "--no-renames",
             "#{base_sha}..#{candidate_sha}"
           ]) do
      changed_paths = String.split(changed, "\n", trim: true)
      protected = MapSet.new(harness.protected_paths)

      case Enum.filter(changed_paths, &MapSet.member?(protected, &1)) do
        [] -> :ok
        paths -> {:error, {:protected_paths_changed, paths}}
      end
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
    with {:ok, absolute, _size} <- safe_regular_path(root, relative),
         {:ok, contents} <- File.read(absolute) do
      {:ok, :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)}
    else
      {:error, _reason} = error -> error
      _ -> {:error, {:invalid_harness_file, relative}}
    end
  end

  defp safe_file(_root, path), do: {:error, {:invalid_path, path}}

  defp safe_regular_path(root, relative) when is_binary(relative) do
    cond do
      Path.type(relative) == :absolute ->
        {:error, {:absolute_path, relative}}

      Enum.any?(Path.split(relative), &(&1 == "..")) ->
        {:error, {:path_escape, relative}}

      true ->
        absolute = Path.expand(relative, root)
        root = Path.expand(root)

        with true <- String.starts_with?(absolute, root <> "/"),
             :ok <- validate_path_components(root, Path.split(relative)),
             {:ok, stat} <- File.stat(absolute) do
          {:ok, absolute, stat.size}
        else
          _ -> {:error, {:invalid_harness_file, relative}}
        end
    end
  end

  defp safe_regular_path(_root, path), do: {:error, {:invalid_path, path}}

  defp validate_path_components(root, components) do
    last_index = length(components) - 1

    components
    |> Enum.with_index()
    |> Enum.reduce_while(root, fn {component, index}, parent ->
      path = Path.join(parent, component)
      expected_type = if index == last_index, do: :regular, else: :directory

      case File.lstat(path) do
        {:ok, %File.Stat{type: ^expected_type}} -> {:cont, path}
        _ -> {:halt, :error}
      end
    end)
    |> case do
      :error -> {:error, :unsafe_path_component}
      _path -> :ok
    end
  end

  defp validate_preview_bytes(value) when is_integer(value) and value > 0, do: :ok
  defp validate_preview_bytes(_value), do: {:error, :invalid_preview_bytes}

  defp reference_manifest_sha(%{reference_path: path, manifest: manifest}) do
    case Enum.find(manifest, &(&1["path"] == path)) do
      %{"sha256" => sha256} when is_binary(sha256) -> {:ok, sha256}
      _ -> {:error, :reference_not_in_harness_manifest}
    end
  end

  defp reference_manifest_sha(_harness), do: {:error, :reference_not_in_harness_manifest}

  defp verify_reference_sha(sha256, sha256), do: :ok
  defp verify_reference_sha(_actual, _expected), do: {:error, :reference_sha_changed}

  defp normalize_utf8_preview(content, truncated?) do
    case :unicode.characters_to_binary(content, :utf8, :utf8) do
      normalized when is_binary(normalized) ->
        {:ok, normalized}

      {:incomplete, valid_prefix, _remainder} when truncated? ->
        {:ok, valid_prefix}

      {:incomplete, _valid_prefix, _remainder} ->
        {:error, :reference_not_utf8}

      {:error, _valid_prefix, _remainder} ->
        {:error, :reference_not_utf8}
    end
  end

  defp hash_and_preview(path, preview_bytes) do
    case File.open(path, [:read, :binary], fn io ->
           stream_hash_and_preview(io, :crypto.hash_init(:sha256), [], 0, preview_bytes)
         end) do
      {:ok, {:ok, sha256, content}} -> {:ok, sha256, content}
      {:ok, {:error, reason}} -> {:error, {:reference_read_failed, reason}}
      {:error, reason} -> {:error, {:reference_read_failed, reason}}
    end
  end

  defp stream_hash_and_preview(io, hash, preview, preview_size, preview_bytes) do
    case IO.binread(io, @hash_chunk_bytes) do
      :eof ->
        sha256 = hash |> :crypto.hash_final() |> Base.encode16(case: :lower)
        {:ok, sha256, IO.iodata_to_binary(Enum.reverse(preview))}

      {:error, reason} ->
        {:error, reason}

      chunk when is_binary(chunk) ->
        remaining = max(preview_bytes - preview_size, 0)
        take = min(byte_size(chunk), remaining)

        {preview, preview_size} =
          if take > 0 do
            {[binary_part(chunk, 0, take) | preview], preview_size + take}
          else
            {preview, preview_size}
          end

        stream_hash_and_preview(
          io,
          :crypto.hash_update(hash, chunk),
          preview,
          preview_size,
          preview_bytes
        )
    end
  end

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
