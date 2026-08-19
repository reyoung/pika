defmodule Pika.Workspace do
  @moduledoc false

  alias Pika.{Config, FileSystem, Paths}
  alias Pika.Git

  @artifact_kinds ~w(plans patches profiles prompts logs)

  defstruct [
    :root,
    :repo,
    :attempts,
    :artifacts,
    :database,
    :config_path,
    :mode,
    :fresh?,
    :managed_repo,
    :git_common_dir,
    :base_sha,
    :snapshot,
    :config_hash
  ]

  def plan(%Config{} = config) do
    with {:ok, fresh?} <- classify(config.workspace),
         :ok <- validate_layout(config, fresh?),
         :ok <- verify_recovery_identity(config, fresh?),
         {:ok, repo_identity} <- inspect_repo(config, fresh?) do
      {:ok,
       %{
         config: config,
         fresh?: fresh?,
         mode: if(config.repo, do: :managed_repo, else: :owned_repo),
         repo_identity: repo_identity,
         lock_path: repo_identity && Path.join(repo_identity.git_common_dir, ".pika.lock")
       }}
    end
  end

  def activate(%{config: config, fresh?: fresh?, mode: mode} = plan) do
    root = config.workspace

    with :ok <- make_layout(root),
         :ok <- activate_repo(plan),
         :ok <- ensure_best_branch(plan),
         {:ok, identity} <- current_identity(root, mode),
         :ok <- verify_planned_identity(plan, identity),
         snapshot <- enrich_snapshot(config.snapshot, identity),
         {:ok, encoded} <- Jason.encode(snapshot, pretty: true),
         :ok <- FileSystem.atomic_write(Path.join(root, "config.json"), encoded <> "\n") do
      config_path = Path.join(root, "config.json")
      {:ok, persisted} = File.read(config_path)

      {:ok,
       %__MODULE__{
         root: root,
         repo: Path.join(root, "repo"),
         attempts: Path.join(root, "attempts"),
         artifacts: Path.join(root, "artifacts"),
         database: Path.join(root, "pika.sqlite3"),
         config_path: config_path,
         mode: mode,
         fresh?: fresh?,
         managed_repo: identity.managed_repo,
         git_common_dir: identity.git_common_dir,
         base_sha: identity.base_sha,
         snapshot: snapshot,
         config_hash: sha256(persisted)
       }}
    end
  end

  def verify_identity(%__MODULE__{} = workspace),
    do: verify_identity(workspace, workspace.base_sha)

  def verify_identity(%__MODULE__{} = workspace, expected_head) do
    with {:ok, current} <- current_identity(workspace.root, workspace.mode),
         :ok <- compare_identity(workspace.snapshot, current),
         true <- current.base_sha == expected_head do
      :ok
    else
      false -> {:error, {:git_head_changed, expected_head}}
      {:error, _} = error -> error
    end
  end

  def artifact_directories, do: @artifact_kinds

  defp classify(root) do
    case File.ls(root) do
      {:error, :enoent} -> {:ok, true}
      {:ok, []} -> {:ok, true}
      {:ok, _entries} -> {:ok, false}
      {:error, reason} -> {:error, {:workspace_unreadable, root, reason}}
    end
  end

  defp validate_layout(_config, true), do: :ok

  defp validate_layout(config, false) do
    root = config.workspace

    required_dirs =
      ["attempts", "artifacts"] ++ Enum.map(@artifact_kinds, &Path.join("artifacts", &1))

    required_files = ["config.json", "pika.sqlite3"]

    missing =
      Enum.reject(required_dirs, &File.dir?(Path.join(root, &1))) ++
        Enum.reject(required_files, &File.regular?(Path.join(root, &1)))

    cond do
      config.existing_snapshot == nil ->
        {:error, {:workspace_not_empty, root}}

      missing != [] ->
        {:error, {:invalid_workspace_layout, missing}}

      true ->
        validate_repo_entry(root, config.existing_snapshot)
    end
  end

  defp validate_repo_entry(root, %{"immutable" => %{"repo_mode" => "managed_repo"}}) do
    case File.lstat(Path.join(root, "repo")) do
      {:ok, %{type: :symlink}} -> :ok
      _ -> {:error, {:invalid_workspace, "repo must be a symlink in managed_repo mode"}}
    end
  end

  defp validate_repo_entry(root, _snapshot) do
    if File.dir?(Path.join(root, "repo")),
      do: :ok,
      else: {:error, {:invalid_workspace, "repo must be a directory in owned_repo mode"}}
  end

  defp verify_recovery_identity(_config, true), do: :ok

  defp verify_recovery_identity(%Config{} = config, false) do
    mode = if config.repo, do: :managed_repo, else: :owned_repo

    with {:ok, current} <- current_identity(config.workspace, mode) do
      compare_identity(config.existing_snapshot, current)
    end
  end

  defp inspect_repo(%Config{repo: repo}, fresh?) when is_binary(repo) do
    with true <- File.dir?(repo),
         {:ok, "false"} <- Git.run(repo, ["rev-parse", "--is-bare-repository"]),
         true <- Git.clean?(repo),
         {:ok, common} <-
           Git.run(repo, ["rev-parse", "--path-format=absolute", "--git-common-dir"]),
         {:ok, common} <- Paths.canonical(common),
         {:ok, head} <- Git.head(repo),
         {:ok, stat} <- File.stat(repo) do
      identity = %{
        canonical_path: repo,
        git_common_dir: common,
        base_sha: head,
        major_device: stat.major_device,
        minor_device: stat.minor_device,
        inode: stat.inode
      }

      if fresh?, do: {:ok, identity}, else: verify_saved_managed_identity(identity)
    else
      false -> {:error, {:invalid_managed_repo, repo, :not_clean_or_not_directory}}
      {:ok, value} -> {:error, {:invalid_managed_repo, repo, {:unexpected_git_value, value}}}
      {:error, reason} -> {:error, {:invalid_managed_repo, repo, reason}}
    end
  end

  defp inspect_repo(%Config{repo: nil}, true) do
    {:ok, nil}
  end

  defp inspect_repo(%Config{repo: nil, workspace: root, existing_snapshot: snapshot}, false) do
    repo = Path.join(root, "repo")

    with {:ok, common} <-
           Git.run(repo, ["rev-parse", "--path-format=absolute", "--git-common-dir"]),
         {:ok, common} <- Paths.canonical(common),
         {:ok, head} <- Git.head(repo),
         expected <- get_in(snapshot, ["immutable", "owned_repo", "git_common_dir"]),
         true <- expected == common do
      {:ok, %{git_common_dir: common, base_sha: head}}
    else
      false -> {:error, {:owned_repo_identity_changed, repo}}
      {:error, reason} -> {:error, {:invalid_owned_repo, repo, reason}}
    end
  end

  defp verify_saved_managed_identity(current) do
    # The symlink and its saved lstat identity are checked below before any lock is acquired.
    {:ok, current}
  end

  defp make_layout(root) do
    Enum.reduce_while(
      [root, Path.join(root, "attempts"), Path.join(root, "artifacts")] ++
        Enum.map(@artifact_kinds, &Path.join([root, "artifacts", &1])),
      :ok,
      fn path, :ok ->
        case File.mkdir_p(path) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, {:workspace_create_failed, path, reason}}}
        end
      end
    )
  end

  defp activate_repo(%{mode: :managed_repo, fresh?: true, config: config}) do
    File.ln_s(config.repo, Path.join(config.workspace, "repo"))
  end

  defp activate_repo(%{mode: :managed_repo}), do: :ok

  defp activate_repo(%{mode: :owned_repo, fresh?: true, config: config}) do
    repo = Path.join(config.workspace, "repo")

    with :ok <- File.mkdir_p(repo),
         {:ok, _} <- Git.run(repo, ["init", "--initial-branch=pika/best"]),
         {:ok, _} <-
           git_with_identity(repo, [
             "commit",
             "--allow-empty",
             "-m",
             "Initialize Pika owned repository"
           ]) do
      :ok
    else
      {:error, reason} -> {:error, {:owned_repo_init_failed, reason}}
    end
  end

  defp activate_repo(%{mode: :owned_repo}), do: :ok

  defp ensure_best_branch(%{config: config, fresh?: fresh?}) do
    repo = Path.join(config.workspace, "repo")

    case Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/pika/best"]) do
      {:ok, _} ->
        :ok

      {:error, _} when fresh? ->
        case Git.run(repo, ["branch", "pika/best", "HEAD"]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:best_branch_create_failed, reason}}
        end

      {:error, reason} ->
        {:error, {:best_branch_missing, reason}}
    end
  end

  defp git_with_identity(repo, args) do
    env = [
      {"GIT_AUTHOR_NAME", "Pika"},
      {"GIT_AUTHOR_EMAIL", "pika@localhost"},
      {"GIT_COMMITTER_NAME", "Pika"},
      {"GIT_COMMITTER_EMAIL", "pika@localhost"}
    ]

    case System.cmd("git", args, cd: repo, env: env, stderr_to_stdout: true) do
      {output, 0} -> {:ok, String.trim(output)}
      {output, status} -> {:error, %{status: status, output: String.trim(output), args: args}}
    end
  end

  defp current_identity(root, :managed_repo) do
    link = Path.join(root, "repo")

    with {:ok, %{type: :symlink} = link_stat} <- File.lstat(link),
         {:ok, target} <- File.read_link(link),
         {:ok, canonical} <- Paths.canonical(Path.expand(target, Path.dirname(link))),
         {:ok, repo_stat} <- File.stat(canonical),
         {:ok, common} <-
           Git.run(canonical, ["rev-parse", "--path-format=absolute", "--git-common-dir"]),
         {:ok, common} <- Paths.canonical(common),
         {:ok, head} <- Git.head(canonical) do
      {:ok,
       %{
         managed_repo: canonical,
         git_common_dir: common,
         base_sha: head,
         repo_stat: stat_map(repo_stat),
         link_stat: stat_map(link_stat)
       }}
    else
      {:ok, stat} -> {:error, {:managed_repo_link_invalid, stat.type}}
      {:error, reason} -> {:error, {:managed_repo_identity_failed, reason}}
    end
  end

  defp current_identity(root, :owned_repo) do
    repo = Path.join(root, "repo")

    with true <- File.dir?(repo),
         {:ok, common} <-
           Git.run(repo, ["rev-parse", "--path-format=absolute", "--git-common-dir"]),
         {:ok, common} <- Paths.canonical(common),
         {:ok, head} <- Git.head(repo) do
      {:ok,
       %{
         managed_repo: nil,
         git_common_dir: common,
         base_sha: head,
         repo_stat: nil,
         link_stat: nil
       }}
    else
      false -> {:error, {:owned_repo_missing, repo}}
      {:error, reason} -> {:error, {:owned_repo_identity_failed, reason}}
    end
  end

  defp verify_planned_identity(%{repo_identity: nil}, _identity), do: :ok

  defp verify_planned_identity(%{mode: :owned_repo, repo_identity: planned}, identity) do
    current = %{git_common_dir: identity.git_common_dir, base_sha: identity.base_sha}

    if current == planned,
      do: :ok,
      else: {:error, {:owned_repo_changed_during_startup, planned, current}}
  end

  defp verify_planned_identity(%{mode: :managed_repo, repo_identity: planned}, identity) do
    current = %{
      canonical_path: identity.managed_repo,
      git_common_dir: identity.git_common_dir,
      base_sha: identity.base_sha,
      major_device: identity.repo_stat["major_device"],
      minor_device: identity.repo_stat["minor_device"],
      inode: identity.repo_stat["inode"]
    }

    if current == planned,
      do: :ok,
      else: {:error, {:managed_repo_changed_during_startup, planned, current}}
  end

  defp enrich_snapshot(snapshot, identity) do
    immutable = snapshot["immutable"]

    immutable =
      if identity.managed_repo do
        Map.put(immutable, "managed_repo", %{
          "canonical_path" => identity.managed_repo,
          "device" => identity.repo_stat,
          "symlink" => identity.link_stat,
          "git_common_dir" => identity.git_common_dir
        })
      else
        Map.put(immutable, "owned_repo", %{"git_common_dir" => identity.git_common_dir})
      end

    Map.put(snapshot, "immutable", immutable)
  end

  defp compare_identity(snapshot, identity) do
    immutable = snapshot["immutable"]

    expected =
      if identity.managed_repo do
        immutable["managed_repo"]
      else
        immutable["owned_repo"]
      end

    current =
      if identity.managed_repo do
        %{
          "canonical_path" => identity.managed_repo,
          "device" => identity.repo_stat,
          "symlink" => identity.link_stat,
          "git_common_dir" => identity.git_common_dir
        }
      else
        %{"git_common_dir" => identity.git_common_dir}
      end

    if expected == current,
      do: :ok,
      else: {:error, {:workspace_identity_mismatch, expected, current}}
  end

  defp stat_map(stat) do
    %{
      "major_device" => stat.major_device,
      "minor_device" => stat.minor_device,
      "inode" => stat.inode
    }
  end

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
