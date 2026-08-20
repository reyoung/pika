defmodule Pika.ArtifactStore do
  @moduledoc false

  import Ecto.Query

  alias Pika.FileSystem
  alias Pika.Persistence.Artifact
  alias Pika.{CampaignStore, Repo, Workspace}

  def write(%Workspace{} = workspace, relative_path, contents, attrs)
      when is_binary(contents) and is_map(attrs) do
    with {:ok, absolute} <- resolve(workspace, relative_path),
         :ok <- FileSystem.atomic_write(absolute, contents) do
      register(workspace, relative_path, attrs)
    end
  end

  def register(%Workspace{} = workspace, relative_path, attrs) when is_map(attrs) do
    with {:ok, absolute} <- resolve(workspace, relative_path),
         {:ok, %{type: :regular} = stat} <- File.stat(absolute),
         {:ok, sha256} <- file_sha256(absolute),
         {:ok, metadata_json} <- Jason.encode(Map.get(attrs, :metadata, %{})) do
      values = %{
        campaign_id: Map.fetch!(attrs, :campaign_id),
        owner_type: Map.fetch!(attrs, :owner_type),
        owner_id: Map.fetch!(attrs, :owner_id),
        kind: Map.fetch!(attrs, :kind),
        relative_path: normalized_relative(relative_path),
        sha256: sha256,
        byte_size: stat.size,
        mime_type: Map.get(attrs, :mime_type) || MIME.from_path(absolute),
        metadata_json: metadata_json,
        created_at: System.system_time(:microsecond)
      }

      case Repo.get_by(Artifact, relative_path: values.relative_path) do
        nil ->
          %Artifact{}
          |> Artifact.changeset(values)
          |> Repo.insert()

        artifact ->
          if same_owner?(artifact, values) do
            artifact
            |> Artifact.changeset(
              Map.take(values, [:sha256, :byte_size, :mime_type, :metadata_json])
            )
            |> Repo.update()
          else
            {:error, :artifact_owner_conflict}
          end
      end
    else
      {:ok, stat} -> {:error, {:not_a_regular_file, stat.type}}
      {:error, reason} -> {:error, reason}
    end
  end

  def append_jsonl(%Workspace{} = workspace, relative_path, value, attrs) when is_map(attrs) do
    with {:ok, absolute} <- resolve(workspace, relative_path),
         {:ok, _dropped} <- recover_jsonl(workspace, relative_path),
         :ok <- append_sync(absolute, Jason.encode!(value) <> "\n") do
      register(workspace, relative_path, Map.put_new(attrs, :mime_type, "application/x-ndjson"))
    end
  end

  def recover_jsonl(%Workspace{} = workspace, relative_path) do
    with {:ok, absolute} <- resolve(workspace, relative_path) do
      case File.read(absolute) do
        {:error, :enoent} ->
          {:ok, 0}

        {:ok, ""} ->
          {:ok, 0}

        {:ok, contents} ->
          if String.ends_with?(contents, "\n") do
            {:ok, 0}
          else
            keep = last_complete_line_end(contents)
            dropped = byte_size(contents) - keep

            with {:ok, io} <- File.open(absolute, [:read, :write, :binary]),
                 {:ok, ^keep} <- :file.position(io, keep),
                 :ok <- :file.truncate(io),
                 :ok <- :file.sync(io),
                 :ok <- File.close(io) do
              {:ok, dropped}
            end
          end

        {:error, reason} ->
          {:error, {:jsonl_read_failed, reason}}
      end
    end
  end

  def replay_jsonl(%Workspace{} = workspace, relative_path) do
    with {:ok, absolute} <- resolve(workspace, relative_path),
         {:ok, _} <- recover_jsonl(workspace, relative_path) do
      absolute
      |> File.stream!(:line, [])
      |> Enum.reduce_while({:ok, []}, fn line, {:ok, records} ->
        case Jason.decode(line) do
          {:ok, record} -> {:cont, {:ok, [record | records]}}
          {:error, error} -> {:halt, {:error, {:invalid_jsonl_record, error}}}
        end
      end)
      |> case do
        {:ok, records} -> {:ok, Enum.reverse(records)}
        error -> error
      end
    end
  end

  def verify_all(%Workspace{} = workspace) do
    Repo.all(from(artifact in Artifact, order_by: artifact.relative_path))
    |> Enum.reduce([], fn artifact, errors ->
      case verify_one(workspace, artifact) do
        :ok ->
          errors

        {:error, reason} ->
          [%{artifact_id: artifact.id, path: artifact.relative_path, reason: reason} | errors]
      end
    end)
    |> Enum.reverse()
    |> case do
      [] -> :ok
      errors -> {:error, {:artifact_verification_failed, errors}}
    end
  end

  def migrate_legacy_paths(%Workspace{} = workspace) do
    Artifact
    |> where([artifact], not like(artifact.relative_path, "artifacts/%"))
    |> order_by([artifact], artifact.relative_path)
    |> Repo.all()
    |> Enum.reduce_while({:ok, []}, fn artifact, {:ok, migrations} ->
      case prepare_legacy_migration(workspace, artifact) do
        {:ok, migration} -> {:cont, {:ok, [migration | migrations]}}
        {:error, reason} -> {:halt, {:error, {artifact.relative_path, reason}}}
      end
    end)
    |> case do
      {:ok, []} ->
        :ok

      {:ok, migrations} ->
        persist_legacy_migrations(Enum.reverse(migrations))

      {:error, reason} ->
        {:error, {:legacy_artifact_migration_failed, reason}}
    end
  end

  def resolve(%Workspace{root: root}, relative_path) when is_binary(relative_path) do
    normalized = normalized_relative(relative_path)

    cond do
      relative_path == "" ->
        {:error, :empty_artifact_path}

      Path.type(relative_path) == :absolute ->
        {:error, :absolute_artifact_path}

      normalized != relative_path ->
        {:error, :noncanonical_artifact_path}

      not String.starts_with?(relative_path, "artifacts/") ->
        {:error, :outside_artifact_root}

      Enum.any?(Path.split(relative_path), &(&1 in [".", "..", ""])) ->
        {:error, :artifact_path_escape}

      true ->
        absolute = Path.join(root, relative_path)

        case reject_symlink_components(root, Path.split(relative_path)) do
          :ok -> {:ok, absolute}
          {:error, _} = error -> error
        end
    end
  end

  def resolve(_workspace, _relative_path), do: {:error, :invalid_artifact_path}

  defp verify_one(workspace, artifact) do
    with :ok <- recover_tail_if_jsonl(workspace, artifact),
         {:ok, absolute} <- resolve(workspace, artifact.relative_path),
         {:ok, %{type: :regular, size: size}} <- File.stat(absolute),
         true <- size == artifact.byte_size,
         {:ok, sha256} <- file_sha256(absolute),
         true <- sha256 == artifact.sha256 do
      :ok
    else
      false -> {:error, :metadata_mismatch}
      {:ok, stat} -> {:error, {:not_regular, stat.type}}
      {:error, reason} -> {:error, reason}
    end
  end

  defp prepare_legacy_migration(workspace, artifact) do
    destination_relative =
      Path.join([
        "artifacts",
        "recovered",
        artifact.id,
        Path.basename(artifact.relative_path)
      ])

    with {:ok, source} <- resolve_legacy(workspace, artifact.relative_path),
         :ok <- verify_file_metadata(source, artifact),
         {:ok, destination} <- resolve(workspace, destination_relative),
         :ok <- File.mkdir_p(Path.dirname(destination)),
         {:ok, ^destination} <- resolve(workspace, destination_relative),
         :ok <- preserve_legacy_file(source, destination),
         :ok <- verify_file_metadata(destination, artifact) do
      {:ok,
       %{
         artifact_id: artifact.id,
         old_path: artifact.relative_path,
         new_path: destination_relative
       }}
    end
  end

  defp resolve_legacy(%Workspace{root: root}, relative_path) do
    normalized = normalized_relative(relative_path)

    cond do
      relative_path == "" ->
        {:error, :empty_artifact_path}

      Path.type(relative_path) == :absolute ->
        {:error, :absolute_artifact_path}

      normalized != relative_path ->
        {:error, :noncanonical_artifact_path}

      Enum.any?(Path.split(relative_path), &(&1 in [".", "..", ""])) ->
        {:error, :artifact_path_escape}

      true ->
        case reject_symlink_components(root, Path.split(relative_path)) do
          :ok -> {:ok, Path.join(root, relative_path)}
          {:error, _} = error -> error
        end
    end
  end

  defp preserve_legacy_file(source, destination) do
    case File.stat(destination) do
      {:ok, %{type: :regular}} ->
        :ok

      {:ok, stat} ->
        {:error, {:migration_destination_not_regular, stat.type}}

      {:error, :enoent} ->
        copy_legacy_file(source, destination)

      {:error, reason} ->
        {:error, {:migration_destination_stat_failed, reason}}
    end
  end

  defp copy_legacy_file(source, destination) do
    temporary =
      destination <>
        ".migrating-#{System.unique_integer([:positive, :monotonic])}"

    result =
      with {:ok, _bytes} <- File.copy(source, temporary),
           :ok <- File.rename(temporary, destination) do
        :ok
      end

    if result != :ok, do: File.rm(temporary)
    result
  end

  defp verify_file_metadata(path, artifact) do
    with {:ok, %{type: :regular, size: size}} <- File.stat(path),
         true <- size == artifact.byte_size,
         {:ok, sha256} <- file_sha256(path),
         true <- sha256 == artifact.sha256 do
      :ok
    else
      false -> {:error, :metadata_mismatch}
      {:ok, stat} -> {:error, {:not_regular, stat.type}}
      {:error, reason} -> {:error, reason}
    end
  end

  defp persist_legacy_migrations(migrations) do
    replacements = Map.new(migrations, &{&1.old_path, &1.new_path})
    now = System.system_time(:microsecond)

    Repo.transaction(fn ->
      Enum.each(migrations, fn migration ->
        {updated, _} =
          Artifact
          |> where(
            [artifact],
            artifact.id == ^migration.artifact_id and
              artifact.relative_path == ^migration.old_path
          )
          |> Repo.update_all(set: [relative_path: migration.new_path])

        if updated != 1,
          do: Repo.rollback({:legacy_artifact_update_conflict, migration.artifact_id})
      end)

      rewrite_runtime_snapshots!(replacements, now)
    end)
    |> case do
      {:ok, _} -> :ok
      {:error, reason} -> {:error, {:legacy_artifact_migration_failed, reason}}
    end
  rescue
    error -> {:error, {:legacy_artifact_migration_failed, Exception.message(error)}}
  end

  defp rewrite_runtime_snapshots!(replacements, now) do
    Repo.query!("SELECT campaign_id, state_blob FROM campaign_runtime_snapshots").rows
    |> Enum.each(fn [campaign_id, blob] ->
      case CampaignStore.decode_snapshot(blob) do
        {:ok, durable} ->
          rewritten = rewrite_paths(durable, replacements)

          if rewritten != durable do
            encoded = :erlang.term_to_binary(rewritten, compressed: 6)

            Repo.query!(
              "UPDATE campaign_runtime_snapshots SET state_blob = ?, updated_at = ? WHERE campaign_id = ?",
              [{:blob, encoded}, now, campaign_id]
            )
          end

        {:error, reason} ->
          Repo.rollback(reason)
      end
    end)
  end

  defp rewrite_paths(value, replacements) when is_binary(value),
    do: Map.get(replacements, value, value)

  defp rewrite_paths(value, replacements) when is_map(value) do
    value
    |> Map.to_list()
    |> Map.new(fn {key, item} ->
      {rewrite_paths(key, replacements), rewrite_paths(item, replacements)}
    end)
  end

  defp rewrite_paths(value, replacements) when is_list(value),
    do: Enum.map(value, &rewrite_paths(&1, replacements))

  defp rewrite_paths(value, replacements) when is_tuple(value) do
    value
    |> Tuple.to_list()
    |> Enum.map(&rewrite_paths(&1, replacements))
    |> List.to_tuple()
  end

  defp rewrite_paths(value, _replacements), do: value

  defp recover_tail_if_jsonl(workspace, artifact) do
    if artifact.mime_type == "application/x-ndjson" or
         String.ends_with?(artifact.relative_path, ".jsonl") do
      case recover_jsonl(workspace, artifact.relative_path) do
        {:ok, _dropped} -> :ok
        {:error, reason} -> {:error, reason}
      end
    else
      :ok
    end
  end

  defp reject_symlink_components(root, components) do
    components
    |> Enum.scan(root, &Path.join(&2, &1))
    |> Enum.reduce_while(:ok, fn path, :ok ->
      case File.lstat(path) do
        {:ok, %{type: :symlink}} -> {:halt, {:error, {:artifact_symlink_forbidden, path}}}
        {:ok, _} -> {:cont, :ok}
        {:error, :enoent} -> {:cont, :ok}
        {:error, reason} -> {:halt, {:error, {:artifact_path_stat_failed, path, reason}}}
      end
    end)
  end

  defp same_owner?(artifact, values) do
    artifact.campaign_id == values.campaign_id and artifact.owner_type == values.owner_type and
      artifact.owner_id == values.owner_id and artifact.kind == values.kind
  end

  defp append_sync(path, contents) do
    with :ok <- File.mkdir_p(Path.dirname(path)),
         {:ok, io} <- File.open(path, [:append, :binary]),
         :ok <- IO.binwrite(io, contents),
         :ok <- :file.sync(io),
         :ok <- File.close(io) do
      :ok
    end
  end

  defp file_sha256(path) do
    try do
      digest =
        path
        |> File.stream!(1_048_576, [])
        |> Enum.reduce(:crypto.hash_init(:sha256), &:crypto.hash_update(&2, &1))
        |> :crypto.hash_final()
        |> Base.encode16(case: :lower)

      {:ok, digest}
    rescue
      error -> {:error, {:artifact_hash_failed, Exception.message(error)}}
    end
  end

  defp last_complete_line_end(contents) do
    case :binary.matches(contents, "\n") do
      [] -> 0
      matches -> matches |> List.last() |> elem(0) |> Kernel.+(1)
    end
  end

  defp normalized_relative(path), do: path |> Path.split() |> Path.join()
end
