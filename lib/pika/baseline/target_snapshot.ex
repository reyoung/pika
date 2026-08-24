defmodule Pika.Baseline.TargetSnapshot do
  @moduledoc "Copies and re-verifies the immutable Optimization Target used by Attempts."

  alias Pika.FileSystem
  alias Pika.Optimization.Persistence

  @spec create(Path.t(), String.t(), map(), String.t()) :: {:ok, Path.t()} | {:error, term()}
  def create(work_root, snapshot_id, target_manifest, manifest_sha256) do
    workspace = Persistence.current().workspace_canonical_path
    relative = Path.join("target-snapshots", snapshot_id)
    destination = Path.join(workspace, relative)
    temporary = destination <> ".preparing"
    source = Path.join(work_root, "target")

    with :ok <- File.mkdir_p(Path.dirname(destination)),
         :ok <- ensure_missing(destination),
         :ok <- validate_source_tree(source),
         {:ok, _files} <- File.cp_r(source, temporary),
         :ok <- verify_root(temporary, target_manifest, manifest_sha256),
         :ok <- freeze_regular_files(temporary),
         :ok <- File.rename(temporary, destination) do
      {:ok, relative}
    else
      {:error, reason, path} ->
        _ = File.rm_rf(temporary)
        {:error, {:target_snapshot_creation_failed, {reason, path}}}

      {:error, reason} ->
        _ = File.rm_rf(temporary)
        {:error, {:target_snapshot_creation_failed, reason}}
    end
  end

  defp ensure_missing(path) do
    if File.exists?(path), do: {:error, :target_snapshot_already_exists}, else: :ok
  end

  @spec verify(String.t()) :: :ok | {:error, term()}
  def verify(snapshot_id) do
    case Pika.Repo.query!(
           "SELECT relative_path, provenance_json, digest FROM target_snapshots WHERE id = ?",
           [snapshot_id]
         ).rows do
      [[relative, provenance, manifest_sha256]] ->
        root = Path.join(Persistence.current().workspace_canonical_path, relative)
        verify_root(root, Jason.decode!(provenance), manifest_sha256)

      [] ->
        {:error, :target_snapshot_not_found}
    end
  end

  defp verify_root(root, manifest, manifest_sha256) do
    with {:ok, _contents, receipt} <-
           Pika.Optimization.FileContract.read(root, "manifest.json"),
         true <-
           receipt.sha256 == manifest_sha256 ||
             {:error, {:target_manifest_identity_mismatch, manifest_sha256, receipt.sha256}} do
      verify_files(root, manifest)
    else
      {:error, _reason} = error -> error
    end
  end

  defp verify_files(root, manifest) do
    Enum.reduce_while(manifest["files"], :ok, fn file, :ok ->
      path = Path.join(root, file["path"])

      case Pika.Optimization.FileContract.read(root, file["path"]) do
        {:ok, _contents, receipt} ->
          if receipt.sha256 == file["sha256"] and receipt.byte_size == file["size"] do
            {:cont, :ok}
          else
            {:halt,
             {:error,
              {:target_snapshot_identity_mismatch, path,
               %{expected: file["sha256"], actual: receipt.sha256}}}}
          end

        {:error, reason} ->
          {:halt, {:error, {:target_snapshot_file_invalid, path, reason}}}
      end
    end)
  end

  defp validate_source_tree(root) do
    root
    |> tree_paths()
    |> Enum.reduce_while(:ok, fn path, :ok ->
      case File.lstat(path) do
        {:ok, %{type: type}} when type in [:regular, :directory] -> {:cont, :ok}
        {:ok, stat} -> {:halt, {:error, {:target_snapshot_unsafe_entry, path, stat.type}}}
        {:error, reason} -> {:halt, {:error, {:target_snapshot_entry_unreadable, path, reason}}}
      end
    end)
  end

  defp tree_paths(root), do: [root | Path.wildcard(Path.join(root, "**/*"), match_dot: true)]

  defp freeze_regular_files(root) do
    root
    |> tree_paths()
    |> Enum.filter(&File.regular?/1)
    |> FileSystem.freeze_files()
  end
end
