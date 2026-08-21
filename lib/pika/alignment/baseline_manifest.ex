defmodule Pika.Alignment.BaselineManifest do
  @moduledoc false

  alias Pika.Alignment.ArtifactStore

  @max_manifest_bytes 1_048_576

  def load(workspace_root, relative_path) when is_binary(relative_path) do
    with true <- artifact_path?(relative_path),
         {:ok, absolute} <- ArtifactStore.resolve(workspace_root, relative_path),
         {:ok, stat} <- File.stat(absolute),
         true <- stat.type == :regular and stat.size <= @max_manifest_bytes,
         {:ok, body} <- File.read(absolute),
         true <- byte_size(body) <= @max_manifest_bytes,
         {:ok, manifest} <- Jason.decode(body),
         {:ok, normalized} <- validate(workspace_root, manifest),
         sha256 <- :crypto.hash(:sha256, body) |> Base.encode16(case: :lower),
         {:ok, artifact} <-
           ArtifactStore.verified(workspace_root, relative_path, sha256, byte_size(body),
             kind: "baseline_manifest",
             mime: "application/json"
           ) do
      {:ok, Map.put(normalized, :manifest_artifact, artifact)}
    else
      false -> {:error, :invalid_baseline_manifest_path_or_size}
      {:error, reason} -> {:error, reason}
    end
  end

  def load(_workspace_root, _relative_path), do: {:error, :invalid_baseline_manifest_path}

  defp validate(workspace_root, manifest) when is_map(manifest) do
    dependencies = manifest["profiler_dependencies"]

    with true <- manifest["schema_version"] == 2,
         target_snapshot_id when is_binary(target_snapshot_id) <- manifest["target_snapshot_id"],
         candidate_sha when is_binary(candidate_sha) <- manifest["candidate_sha"],
         summary when is_binary(summary) <- manifest["summary"],
         true <- String.trim(summary) != "",
         samples when is_binary(samples) <- manifest["samples_artifact"],
         correctness when is_binary(correctness) <- manifest["correctness_artifact"],
         profiler when is_binary(profiler) <- manifest["profiler_artifact"],
         dependencies when is_list(dependencies) <- dependencies,
         true <- Enum.all?(dependencies, &is_binary/1),
         paths <- [samples, correctness, profiler | dependencies],
         true <- length(paths) == length(Enum.uniq(paths)),
         :ok <- validate_artifact_paths(workspace_root, paths) do
      {:ok,
       %{
         schema_version: 2,
         target_snapshot_id: target_snapshot_id,
         candidate_sha: candidate_sha,
         summary: summary,
         samples_artifact: samples,
         correctness_artifact: correctness,
         profiler_artifact: profiler,
         profiler_dependencies: dependencies
       }}
    else
      {:error, reason} -> {:error, reason}
      _ -> {:error, :invalid_baseline_manifest}
    end
  end

  defp validate(_workspace_root, _manifest), do: {:error, :invalid_baseline_manifest}

  defp validate_artifact_paths(workspace_root, paths) do
    if Enum.all?(paths, &valid_artifact?(workspace_root, &1)),
      do: :ok,
      else: {:error, :invalid_baseline_artifact_path}
  end

  defp valid_artifact?(workspace_root, relative_path) do
    with true <- artifact_path?(relative_path),
         {:ok, absolute} <- ArtifactStore.resolve(workspace_root, relative_path),
         {:ok, stat} <- File.stat(absolute) do
      stat.type == :regular
    else
      _ -> false
    end
  end

  defp artifact_path?(relative_path) do
    Path.type(relative_path) == :relative and
      match?(["artifacts" | _rest], Path.split(relative_path))
  end
end
