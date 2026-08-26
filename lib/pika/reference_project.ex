defmodule Pika.ReferenceProject do
  @moduledoc "Materializes and freezes Iteration Reference Projects outside candidate Git history."

  alias Pika.FileSystem
  alias Pika.Git
  alias Pika.Optimization.Config.ReferenceProject

  @manifest_name "reference-projects.json"
  @metadata_suffix ".pika-reference.json"

  @spec prepare(Path.t(), Path.t(), Path.t(), [ReferenceProject.t()]) ::
          {:ok, [map()]} | {:error, term()}
  def prepare(workspace_root, attempt_root, attempt_repo, projects)
      when is_binary(workspace_root) and is_binary(attempt_root) and is_binary(attempt_repo) and
             is_list(projects) do
    manifest = manifest_path(attempt_root)

    case read_manifest(manifest) do
      {:ok, snapshots} ->
        with :ok <- verify_snapshots(workspace_root, snapshots),
             :ok <- link_snapshots(workspace_root, attempt_repo, snapshots) do
          {:ok, snapshots}
        end

      {:error, :enoent} ->
        with {:ok, snapshots} <- materialize(workspace_root, projects),
             :ok <- write_manifest(manifest, snapshots),
             :ok <- link_snapshots(workspace_root, attempt_repo, snapshots) do
          {:ok, snapshots}
        end

      {:error, reason} ->
        {:error, {:reference_manifest_invalid, manifest, reason}}
    end
  end

  @spec load_for_attempt(Path.t(), pos_integer()) :: {:ok, [map()]} | {:error, term()}
  def load_for_attempt(workspace_root, attempt_id) do
    root = Pika.Attempt.Workspace.paths(workspace_root, attempt_id).root

    case read_manifest(manifest_path(root)) do
      {:ok, snapshots} ->
        with :ok <- verify_snapshots(workspace_root, snapshots), do: {:ok, snapshots}

      {:error, :enoent} ->
        {:ok, []}

      {:error, reason} ->
        {:error, {:reference_manifest_invalid, manifest_path(root), reason}}
    end
  end

  @spec manifest_path(Path.t()) :: Path.t()
  def manifest_path(attempt_root), do: Path.join(attempt_root, @manifest_name)

  defp materialize(workspace_root, projects) do
    refs_root = Path.join(workspace_root, "refs")

    with :ok <- ensure_real_directory(refs_root) do
      Enum.reduce_while(projects, {:ok, []}, fn project, {:ok, snapshots} ->
        case ensure_checkout(refs_root, project) do
          {:ok, snapshot} -> {:cont, {:ok, [snapshot | snapshots]}}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)
      |> case do
        {:ok, snapshots} -> {:ok, Enum.reverse(snapshots)}
        {:error, reason} -> {:error, reason}
      end
    end
  end

  defp ensure_checkout(refs_root, %ReferenceProject{} = project) do
    checkout = Path.join(refs_root, project.id)

    :global.trans({{__MODULE__, :checkout, checkout}, self()}, fn ->
      metadata = metadata_path(refs_root, project.id)

      case File.lstat(checkout) do
        {:ok, %{type: :directory}} ->
          if File.dir?(Path.join(checkout, ".git")),
            do: verify_existing(checkout, metadata, project),
            else: {:error, {:reference_checkout_conflict, project.id, checkout}}

        {:ok, _stat} ->
          {:error, {:reference_checkout_conflict, project.id, checkout}}

        {:error, :enoent} ->
          clone_and_freeze(refs_root, checkout, metadata, project)

        {:error, reason} ->
          {:error, {:reference_checkout_unavailable, project.id, checkout, reason}}
      end
    end)
  end

  defp verify_existing(checkout, metadata, project) do
    with {:ok, frozen} <- read_json(metadata),
         :ok <- equal(frozen["id"], project.id, {:reference_metadata_mismatch, project.id}),
         :ok <-
           equal(
             canonical_url(frozen["url"]),
             canonical_url(project.url),
             {:reference_origin_mismatch, project.id}
           ),
         :ok <-
           equal(
             frozen["revision"],
             project.revision,
             {:reference_revision_changed, project.id, frozen["revision"], project.revision}
           ),
         {:ok, origin} <- Git.run(checkout, ["remote", "get-url", "origin"]),
         :ok <-
           equal(
             canonical_url(origin),
             canonical_url(project.url),
             {:reference_origin_mismatch, project.id}
           ),
         {:ok, sha} <- Git.run(checkout, ["rev-parse", "HEAD"]),
         :ok <- equal(sha, frozen["sha"], {:reference_sha_mismatch, project.id}),
         true <- Git.clean?(checkout) do
      {:ok, snapshot(project, sha)}
    else
      false -> {:error, {:reference_checkout_dirty, project.id, checkout}}
      {:error, :enoent} -> {:error, {:reference_metadata_missing, project.id, metadata}}
      {:error, _reason} = error -> error
    end
  end

  defp clone_and_freeze(refs_root, checkout, metadata, project) do
    temporary =
      Path.join(
        refs_root,
        ".#{project.id}.clone-#{System.unique_integer([:positive, :monotonic])}"
      )

    result =
      with {:ok, _output} <-
             Git.run(refs_root, [
               "-c",
               "protocol.file.allow=always",
               "clone",
               "--depth",
               "1",
               "--no-checkout",
               "--",
               project.url,
               temporary
             ]),
           {:ok, sha} <- resolve_revision(temporary, project.revision),
           {:ok, _output} <- Git.run(temporary, ["checkout", "--detach", sha]),
           true <- Git.clean?(temporary),
           :ok <- File.rename(temporary, checkout),
           :ok <- write_metadata(metadata, project, sha) do
        {:ok, snapshot(project, sha)}
      else
        false -> {:error, {:reference_checkout_dirty, project.id, temporary}}
        {:error, reason} -> {:error, {:reference_materialization_failed, project.id, reason}}
      end

    if File.exists?(temporary), do: File.rm_rf(temporary)

    if match?({:error, _reason}, result) and File.dir?(checkout) do
      _ = File.rm(metadata)
      _ = File.rm_rf(checkout)
    end

    result
  end

  defp resolve_revision(checkout, nil), do: rev_parse(checkout, "HEAD^{commit}")

  defp resolve_revision(checkout, revision) do
    case rev_parse(checkout, "#{revision}^{commit}") do
      {:ok, sha} ->
        {:ok, sha}

      {:error, _reason} ->
        with {:ok, _output} <- Git.run(checkout, ["fetch", "--depth", "1", "origin", revision]),
             {:ok, sha} <- rev_parse(checkout, "FETCH_HEAD^{commit}") do
          {:ok, sha}
        end
    end
  end

  defp rev_parse(checkout, expression) do
    case Git.run(checkout, ["rev-parse", "--verify", expression]) do
      {:ok, sha} -> {:ok, sha}
      {:error, reason} -> {:error, reason}
    end
  end

  defp verify_snapshots(workspace_root, snapshots) do
    refs_root = Path.join(workspace_root, "refs")

    Enum.reduce_while(snapshots, :ok, fn snapshot, :ok ->
      checkout = Path.join(refs_root, snapshot["id"])
      metadata = metadata_path(refs_root, snapshot["id"])

      result =
        with {:ok, %{type: :directory}} <- File.lstat(checkout),
             true <- File.dir?(Path.join(checkout, ".git")),
             {:ok, frozen} <- read_json(metadata),
             :ok <-
               equal(
                 Map.take(frozen, ~w(id url revision sha)),
                 Map.take(snapshot, ~w(id url revision sha)),
                 {:reference_metadata_mismatch, snapshot["id"]}
               ),
             {:ok, origin} <- Git.run(checkout, ["remote", "get-url", "origin"]),
             :ok <-
               equal(
                 canonical_url(origin),
                 canonical_url(snapshot["url"]),
                 {:reference_origin_mismatch, snapshot["id"]}
               ),
             {:ok, sha} <- Git.run(checkout, ["rev-parse", "HEAD"]),
             :ok <- equal(sha, snapshot["sha"], {:reference_sha_mismatch, snapshot["id"]}),
             true <- Git.clean?(checkout) do
          :ok
        else
          false -> {:error, {:reference_checkout_invalid, snapshot["id"], checkout}}
          {:ok, _stat} -> {:error, {:reference_checkout_invalid, snapshot["id"], checkout}}
          {:error, _reason} = error -> error
        end

      case result do
        :ok -> {:cont, :ok}
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
  end

  defp link_snapshots(_workspace_root, _attempt_repo, []), do: :ok

  defp link_snapshots(workspace_root, attempt_repo, snapshots) do
    refs_root = Path.join(workspace_root, "refs")
    link_root = Path.join(attempt_repo, "ref")

    with :ok <- ensure_real_directory(link_root),
         :ok <- ensure_reference_exclude(attempt_repo) do
      Enum.reduce_while(snapshots, :ok, fn snapshot, :ok ->
        source = Path.join(refs_root, snapshot["id"])
        link = Path.join(link_root, snapshot["id"])

        case ensure_link(link, source) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)
    end
  end

  defp ensure_link(link, source) do
    case File.lstat(link) do
      {:error, :enoent} ->
        case File.ln_s(source, link) do
          :ok -> :ok
          {:error, reason} -> {:error, {:reference_link_failed, link, reason}}
        end

      {:ok, %{type: :symlink}} ->
        with {:ok, target} <- File.read_link(link),
             :ok <-
               equal(
                 Path.expand(target, Path.dirname(link)),
                 Path.expand(source),
                 {:reference_link_conflict, link}
               ) do
          :ok
        end

      {:ok, _stat} ->
        {:error, {:reference_link_conflict, link}}

      {:error, reason} ->
        {:error, {:reference_link_failed, link, reason}}
    end
  end

  defp ensure_reference_exclude(repo) do
    with {:ok, git_path} <- Git.run(repo, ["rev-parse", "--git-path", "info/exclude"]) do
      path = if Path.type(git_path) == :absolute, do: git_path, else: Path.expand(git_path, repo)

      :global.trans({{__MODULE__, :exclude, path}, self()}, fn ->
        with :ok <- File.mkdir_p(Path.dirname(path)),
             {:ok, contents} <- read_optional(path) do
          patterns = String.split(contents, "\n", trim: true)

          if "/ref/" in patterns do
            :ok
          else
            separator = if contents == "" or String.ends_with?(contents, "\n"), do: "", else: "\n"
            File.write(path, contents <> separator <> "/ref/\n")
          end
        end
      end)
    end
  end

  defp snapshot(project, sha) do
    %{
      "id" => project.id,
      "url" => project.url,
      "description" => project.description,
      "revision" => project.revision,
      "sha" => sha
    }
  end

  defp write_metadata(path, project, sha) do
    metadata =
      project
      |> snapshot(sha)
      |> Map.take(~w(id url revision sha))

    write_frozen_json(path, metadata)
  end

  defp write_manifest(path, snapshots),
    do: write_frozen_json(path, %{"reference_projects" => snapshots})

  defp write_frozen_json(path, value) do
    with :ok <- FileSystem.atomic_write(path, Jason.encode!(value)),
         :ok <- File.chmod(path, 0o444) do
      :ok
    end
  end

  defp read_manifest(path) do
    with {:ok, decoded} <- read_json(path),
         snapshots when is_list(snapshots) <- decoded["reference_projects"],
         true <- Enum.all?(snapshots, &valid_snapshot?/1) do
      {:ok, snapshots}
    else
      {:error, _reason} = error -> error
      _other -> {:error, :invalid_shape}
    end
  end

  defp read_json(path) do
    with {:ok, contents} <- File.read(path),
         {:ok, decoded} when is_map(decoded) <- Jason.decode(contents) do
      {:ok, decoded}
    else
      {:error, reason} -> {:error, reason}
      _other -> {:error, :invalid_json}
    end
  end

  defp valid_snapshot?(snapshot) when is_map(snapshot) do
    is_binary(snapshot["id"]) and is_binary(snapshot["url"]) and
      is_binary(snapshot["description"]) and
      (is_nil(snapshot["revision"]) or is_binary(snapshot["revision"])) and
      is_binary(snapshot["sha"]) and
      Regex.match?(~r/\A[0-9a-f]{40}([0-9a-f]{24})?\z/i, snapshot["sha"])
  end

  defp valid_snapshot?(_snapshot), do: false

  defp ensure_real_directory(path) do
    case File.lstat(path) do
      {:ok, %{type: :directory}} -> :ok
      {:ok, %{type: type}} -> {:error, {:reference_path_conflict, path, type}}
      {:error, :enoent} -> File.mkdir_p(path)
      {:error, reason} -> {:error, {:reference_path_unavailable, path, reason}}
    end
  end

  defp read_optional(path) do
    case File.read(path) do
      {:error, :enoent} -> {:ok, ""}
      result -> result
    end
  end

  defp metadata_path(refs_root, id), do: Path.join(refs_root, ".#{id}#{@metadata_suffix}")

  defp canonical_url(value) when is_binary(value) do
    value = value |> String.trim() |> String.trim_trailing("/")
    if Path.type(value) == :absolute, do: Path.expand(value), else: value
  end

  defp canonical_url(value), do: value

  defp equal(value, value, _reason), do: :ok
  defp equal(_actual, _expected, reason), do: {:error, reason}
end
