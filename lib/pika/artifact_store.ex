defmodule Pika.ArtifactStore do
  @moduledoc false

  import Ecto.Query

  alias Pika.FileSystem
  alias Pika.Persistence.Artifact
  alias Pika.{Repo, Workspace}

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
