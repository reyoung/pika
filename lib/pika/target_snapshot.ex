defmodule Pika.TargetSnapshot do
  @moduledoc false

  alias Pika.{FileSystem, Git}

  @manifest_name "manifest.json"

  def reusable?(spec, snapshot) when is_map(spec) and is_map(snapshot) do
    implementations = spec["implementations"] || %{}
    target = implementations["optimization_target"] || %{}
    target_compatible?(target, snapshot)
  end

  def reusable?(_spec, _snapshot), do: false

  def prepare(workspace_root, revision, setup_root, setup_sha, spec, references, opts \\ [])
      when is_binary(workspace_root) and is_integer(revision) and revision > 0 and
             is_binary(setup_root) and is_binary(setup_sha) and is_map(spec) and
             is_list(references) do
    implementations = spec["implementations"] || %{}
    target = implementations["optimization_target"] || %{}
    development = implementations["development"] || %{}
    oracle = implementations["oracle"] || %{}

    with :ok <- verify_setup(setup_root, setup_sha),
         :ok <- verify_tracked_entrypoint(setup_root, development["entrypoint"]),
         :ok <- verify_oracle(setup_root, oracle),
         {:ok, snapshot} <-
           prepare_snapshot(
             workspace_root,
             revision,
             setup_root,
             setup_sha,
             target,
             references,
             Keyword.get(opts, :inherited_snapshot)
           ) do
      {:ok,
       Map.merge(snapshot, %{
         development_sha: setup_sha,
         development_entrypoint: development["entrypoint"],
         oracle: oracle
       })}
    end
  end

  defp prepare_snapshot(
         workspace_root,
         revision,
         setup_root,
         setup_sha,
         target,
         references,
         inherited
       )
       when is_map(inherited) do
    if target_compatible?(target, inherited) do
      with :ok <- verify(workspace_root, inherited) do
        {:ok, inherited}
      end
    else
      prepare_snapshot(
        workspace_root,
        revision,
        setup_root,
        setup_sha,
        target,
        references,
        nil
      )
    end
  end

  defp prepare_snapshot(
         workspace_root,
         revision,
         setup_root,
         setup_sha,
         target,
         references,
         _inherited
       ) do
    with {:ok, source} <-
           target_source(workspace_root, setup_root, setup_sha, target, references),
         :ok <- verify_tracked_entrypoint(source.repo, target["entrypoint"], source.sha),
         {:ok, snapshot} <- materialize(workspace_root, revision, source, target["entrypoint"]),
         :ok <- verify(workspace_root, snapshot) do
      {:ok, snapshot}
    end
  end

  def verify(workspace_root, snapshot) when is_binary(workspace_root) and is_map(snapshot) do
    checkout = checkout_path(workspace_root, snapshot)
    manifest_path = manifest_path(workspace_root, snapshot)

    with true <- safe_snapshot_path?(workspace_root, checkout),
         {:ok, manifest_body} <- File.read(manifest_path),
         {:ok, manifest} <- Jason.decode(manifest_body),
         true <- manifest_identity(manifest) == manifest_identity(snapshot),
         {:ok, actual_sha} <- Git.head(checkout),
         true <- actual_sha == value(snapshot, :source_sha),
         true <- Git.clean?(checkout),
         {:ok, actual_tree} <- Git.run(checkout, ["rev-parse", "HEAD^{tree}"]),
         true <- actual_tree == value(snapshot, :tree_sha),
         :ok <- verify_tracked_entrypoint(checkout, value(snapshot, :entrypoint)) do
      :ok
    else
      false -> {:error, :target_snapshot_identity_mismatch}
      {:ok, actual} -> {:error, {:target_snapshot_identity_mismatch, actual}}
      {:error, reason} -> {:error, {:target_snapshot_invalid, reason}}
    end
  end

  def link(workspace_root, worktree, snapshot) do
    source = checkout_path(workspace_root, snapshot)
    link = Path.join(worktree, "target")

    with :ok <- verify(workspace_root, snapshot),
         :ok <- ensure_exclude(worktree),
         :ok <- ensure_link(link, source, Path.join(workspace_root, "targets")) do
      :ok
    end
  end

  def checkout_path(workspace_root, snapshot) do
    Path.join(workspace_root, value(snapshot, :checkout_relative_path))
  end

  def public(snapshot) when is_map(snapshot) do
    Map.take(snapshot, [
      :id,
      :revision,
      :source_kind,
      :source_reference_id,
      :source_sha,
      :tree_sha,
      :entrypoint,
      :digest,
      :checkout_relative_path,
      :development_sha,
      :development_entrypoint,
      :oracle
    ])
  end

  defp target_source(workspace_root, setup_root, setup_sha, target, references) do
    source = target["source"] || %{}

    case source["kind"] do
      "development_snapshot" ->
        {:ok,
         %{
           kind: "development_snapshot",
           reference_id: nil,
           repo: setup_root,
           sha: setup_sha
         }}

      "reference_project" ->
        reference_id = source["reference_id"]

        case Enum.find(references, &(value(&1, :id) == reference_id)) do
          nil ->
            {:error, {:target_reference_not_found, reference_id}}

          reference ->
            sha = value(reference, :sha)
            status = value(reference, :status)
            repo = Path.join([workspace_root, "refs", reference_id])

            if status in [:resolved, "resolved"] and is_binary(sha) do
              {:ok,
               %{
                 kind: "reference_project",
                 reference_id: reference_id,
                 repo: repo,
                 sha: sha
               }}
            else
              {:error, {:target_reference_not_resolved, reference_id}}
            end
        end

      other ->
        {:error, {:invalid_target_source, other}}
    end
  end

  defp target_compatible?(target, inherited) do
    source = target["source"] || %{}

    target["entrypoint"] == value(inherited, :entrypoint) and
      source["kind"] == value(inherited, :source_kind) and
      source["reference_id"] == value(inherited, :source_reference_id)
  end

  defp materialize(workspace_root, revision, source, entrypoint) do
    relative_root = Path.join(["targets", Integer.to_string(revision)])
    root = Path.join(workspace_root, relative_root)
    checkout = Path.join(root, "repo")
    manifest_path = Path.join(root, @manifest_name)

    desired = %{
      "schema_version" => 1,
      "revision" => revision,
      "source_kind" => source.kind,
      "source_reference_id" => source.reference_id,
      "source_sha" => source.sha,
      "entrypoint" => entrypoint,
      "checkout_relative_path" => Path.join(relative_root, "repo")
    }

    case existing_snapshot(manifest_path, desired) do
      {:ok, snapshot} ->
        {:ok, snapshot}

      :replace ->
        with :ok <- reset_snapshot_root(workspace_root, root),
             :ok <- clone_snapshot(workspace_root, source.repo, source.sha, root, checkout),
             {:ok, tree_sha} <- Git.run(checkout, ["rev-parse", "HEAD^{tree}"]),
             :ok <- verify_tracked_entrypoint(checkout, entrypoint),
             snapshot <- build_snapshot(desired, tree_sha),
             {:ok, body} <- Jason.encode(stringify(snapshot), pretty: true),
             :ok <- FileSystem.atomic_write(manifest_path, body <> "\n") do
          {:ok, snapshot}
        end
    end
  end

  defp existing_snapshot(manifest_path, desired) do
    with {:ok, body} <- File.read(manifest_path),
         {:ok, manifest} <- Jason.decode(body),
         true <- Map.take(manifest, Map.keys(desired)) == desired do
      {:ok, atomize_snapshot(manifest)}
    else
      _ -> :replace
    end
  end

  defp build_snapshot(desired, tree_sha) do
    digest_input =
      desired
      |> Map.take(~w(source_kind source_reference_id source_sha entrypoint))
      |> Map.put("tree_sha", tree_sha)

    digest = :crypto.hash(:sha256, Jason.encode!(digest_input)) |> Base.encode16(case: :lower)

    desired
    |> Map.merge(%{
      "id" => "target-#{String.slice(digest, 0, 24)}",
      "tree_sha" => tree_sha,
      "digest" => digest
    })
    |> atomize_snapshot()
  end

  defp clone_snapshot(workspace_root, source_repo, source_sha, root, checkout) do
    temp = Path.join(root, ".repo.tmp-#{System.unique_integer([:positive, :monotonic])}")

    result =
      with :ok <- File.mkdir_p(root),
           {:ok, _} <-
             Git.run(workspace_root, [
               "-c",
               "protocol.file.allow=always",
               "clone",
               "--no-hardlinks",
               "--no-checkout",
               source_repo,
               temp
             ]),
           {:ok, _} <- Git.run(temp, ["checkout", "--detach", source_sha]),
           {:ok, ^source_sha} <- Git.head(temp),
           true <- Git.clean?(temp),
           {:ok, _} <- Git.run(temp, ["remote", "remove", "origin"]),
           :ok <- File.rename(temp, checkout) do
        :ok
      else
        false -> {:error, :target_snapshot_dirty}
        {:ok, actual} -> {:error, {:target_snapshot_sha_mismatch, actual, source_sha}}
        {:error, reason} -> {:error, {:target_snapshot_clone_failed, reason}}
      end

    if File.exists?(temp), do: File.rm_rf(temp)
    result
  end

  defp reset_snapshot_root(workspace_root, root) do
    targets = workspace_root |> Path.join("targets") |> Path.expand()
    expanded = Path.expand(root)

    if Path.dirname(expanded) == targets do
      case File.lstat(expanded) do
        {:error, :enoent} -> :ok
        {:ok, %{type: :directory}} -> File.rm_rf(expanded) |> normalize_remove_result()
        {:ok, stat} -> {:error, {:target_snapshot_path_conflict, stat.type}}
        {:error, reason} -> {:error, {:target_snapshot_path_unavailable, reason}}
      end
    else
      {:error, :unsafe_target_snapshot_path}
    end
  end

  defp normalize_remove_result({:ok, _paths}), do: :ok
  defp normalize_remove_result({:error, reason, _path}), do: {:error, reason}

  defp verify_setup(setup_root, setup_sha) do
    with {:ok, actual} <- Git.head(setup_root),
         true <- actual == setup_sha,
         true <- Git.clean?(setup_root) do
      :ok
    else
      false -> {:error, :setup_not_clean_or_head_mismatch}
      {:ok, actual} -> {:error, {:setup_sha_mismatch, actual, setup_sha}}
      {:error, reason} -> {:error, {:setup_invalid, reason}}
    end
  end

  defp verify_oracle(_setup_root, %{"kind" => "optimization_target"}), do: :ok

  defp verify_oracle(setup_root, %{"kind" => "repository_path", "entrypoint" => path}),
    do: verify_tracked_entrypoint(setup_root, path)

  defp verify_oracle(_setup_root, _oracle), do: {:error, :invalid_oracle}

  defp verify_tracked_entrypoint(repo, entrypoint, sha \\ "HEAD") do
    with :ok <- safe_relative_path(entrypoint),
         {:ok, _} <- Git.run(repo, ["cat-file", "-e", "#{sha}:#{entrypoint}"]),
         {:ok, type} <- Git.run(repo, ["cat-file", "-t", "#{sha}:#{entrypoint}"]),
         true <- type == "blob",
         true <- File.regular?(Path.join(repo, entrypoint)) do
      :ok
    else
      false -> {:error, {:implementation_entrypoint_not_regular, entrypoint}}
      {:error, reason} -> {:error, {:implementation_entrypoint_not_tracked, entrypoint, reason}}
    end
  end

  defp safe_relative_path(path) when is_binary(path) do
    if Path.type(path) == :relative and
         not Enum.any?(Path.split(path), &(&1 in ["", ".", ".."])) do
      :ok
    else
      {:error, {:invalid_implementation_entrypoint, path}}
    end
  end

  defp safe_relative_path(path), do: {:error, {:invalid_implementation_entrypoint, path}}

  defp manifest_path(workspace_root, snapshot) do
    snapshot
    |> value(:checkout_relative_path)
    |> Path.dirname()
    |> then(&Path.join([workspace_root, &1, @manifest_name]))
  end

  defp safe_snapshot_path?(workspace_root, checkout) do
    targets = workspace_root |> Path.join("targets") |> Path.expand()
    expanded = Path.expand(checkout)
    String.starts_with?(expanded, targets <> "/")
  end

  defp ensure_exclude(worktree) do
    with {:ok, exclude_path} <- Git.run(worktree, ["rev-parse", "--git-path", "info/exclude"]) do
      exclude_path = Path.expand(exclude_path, worktree)

      :global.trans({{__MODULE__, :target_exclude, exclude_path}, self()}, fn ->
        with :ok <- File.mkdir_p(Path.dirname(exclude_path)),
             {:ok, contents} <- read_optional(exclude_path) do
          patterns = String.split(contents, "\n", trim: true)

          if "/target" in patterns do
            :ok
          else
            separator = if contents == "" or String.ends_with?(contents, "\n"), do: "", else: "\n"
            File.write(exclude_path, contents <> separator <> "/target\n")
          end
        end
      end)
    end
  end

  defp ensure_link(link, source, targets_root) do
    case File.lstat(link) do
      {:error, :enoent} ->
        File.ln_s(source, link)

      {:ok, %{type: :symlink}} ->
        with {:ok, target} <- File.read_link(link) do
          actual = Path.expand(target, Path.dirname(link))

          cond do
            actual == source ->
              :ok

            String.starts_with?(actual, Path.expand(targets_root) <> "/") ->
              replace_link(link, source)

            true ->
              {:error, {:target_link_conflict, link}}
          end
        end

      {:ok, _stat} ->
        {:error, {:target_link_conflict, link}}

      {:error, reason} ->
        {:error, {:target_link_failed, reason}}
    end
  end

  defp replace_link(link, source) do
    with :ok <- File.rm(link), :ok <- File.ln_s(source, link), do: :ok
  end

  defp read_optional(path) do
    case File.read(path) do
      {:error, :enoent} -> {:ok, ""}
      result -> result
    end
  end

  defp manifest_identity(map) do
    %{
      "id" => value(map, :id),
      "revision" => value(map, :revision),
      "source_kind" => value(map, :source_kind),
      "source_reference_id" => value(map, :source_reference_id),
      "source_sha" => value(map, :source_sha),
      "tree_sha" => value(map, :tree_sha),
      "entrypoint" => value(map, :entrypoint),
      "digest" => value(map, :digest),
      "checkout_relative_path" => value(map, :checkout_relative_path)
    }
  end

  defp atomize_snapshot(map) do
    %{
      id: map["id"],
      revision: map["revision"],
      source_kind: map["source_kind"],
      source_reference_id: map["source_reference_id"],
      source_sha: map["source_sha"],
      tree_sha: map["tree_sha"],
      entrypoint: map["entrypoint"],
      digest: map["digest"],
      checkout_relative_path: map["checkout_relative_path"]
    }
  end

  defp stringify(map),
    do: Map.new(map, fn {key, val} -> {to_string(key), stringify_value(val)} end)

  defp stringify_value(map) when is_map(map), do: stringify(map)
  defp stringify_value(list) when is_list(list), do: Enum.map(list, &stringify_value/1)
  defp stringify_value(value), do: value

  defp value(map, key), do: Map.get(map, key, Map.get(map, to_string(key)))
end
