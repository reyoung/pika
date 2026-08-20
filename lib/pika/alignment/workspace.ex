defmodule Pika.Alignment.Workspace do
  @moduledoc false

  alias Pika.Git

  @enforce_keys [
    :root,
    :source_repo,
    :source_sha,
    :source_status,
    :repo,
    :setup_worktree,
    :artifacts
  ]
  defstruct [:root, :source_repo, :source_sha, :source_status, :repo, :setup_worktree, :artifacts]

  def prepare(source_repo, opts \\ []) do
    source_repo = Path.expand(source_repo)

    with :ok <- validate_source(source_repo),
         {:ok, source_sha} <- Git.head(source_repo),
         {:ok, source_status} <-
           Git.run(source_repo, ["status", "--porcelain=v1", "--untracked-files=normal"]),
         {:ok, root} <- prepare_root(Keyword.get(opts, :workspace)),
         {:ok, workspace} <- clone_and_initialize(source_repo, source_sha, source_status, root) do
      {:ok, workspace}
    end
  end

  def verify_source_unchanged(%__MODULE__{} = workspace) do
    with {:ok, sha} <- Git.head(workspace.source_repo),
         true <- sha == workspace.source_sha,
         {:ok, status} <-
           Git.run(workspace.source_repo, ["status", "--porcelain=v1", "--untracked-files=normal"]),
         true <- status == workspace.source_status do
      :ok
    else
      _ -> {:error, :source_repo_changed}
    end
  end

  def verify_setup_merge(%__MODULE__{} = workspace, base_sha, setup_sha, best_sha) do
    verify_setup_merge(workspace, workspace.source_sha, base_sha, setup_sha, best_sha)
  end

  def verify_setup_merge(
        %__MODULE__{} = workspace,
        expected_base_sha,
        base_sha,
        setup_sha,
        best_sha
      ) do
    with true <- base_sha == expected_base_sha,
         {:ok, actual_setup} <- Git.run(workspace.setup_worktree, ["rev-parse", "HEAD"]),
         true <- actual_setup == setup_sha,
         {:ok, actual_best} <- Git.run(workspace.repo, ["rev-parse", "pika/best"]),
         true <- actual_best == best_sha,
         {:ok, parent} <- Git.run(workspace.repo, ["rev-parse", "#{best_sha}^"]),
         true <- parent == base_sha,
         {:ok, changed} <-
           Git.run(workspace.repo, ["diff", "--name-only", "#{base_sha}..#{best_sha}"]),
         changed_paths <- String.split(changed, "\n", trim: true),
         true <- changed_paths != [],
         true <- Enum.all?(changed_paths, &deliverable_path?/1) do
      :ok
    else
      false -> {:error, :git_verification_failed}
      {:error, reason} -> {:error, reason}
    end
  end

  def prepare_setup_revision(%__MODULE__{} = workspace, base_sha, revision)
      when is_binary(base_sha) and is_integer(revision) and revision > 1 do
    setup = Path.join(workspace.root, "setup/#{revision}")
    branch = "pika/setup/#{revision}"

    with {:ok, ^base_sha} <- Git.run(workspace.repo, ["rev-parse", "pika/best"]),
         :ok <- ensure_revision_worktree(workspace.repo, setup, branch, base_sha) do
      {:ok, %{workspace | setup_worktree: setup}}
    else
      {:ok, actual} -> {:error, {:best_sha_mismatch, actual, base_sha}}
      {:error, reason} -> {:error, reason}
    end
  end

  defp deliverable_path?("ref/" <> _path), do: false
  defp deliverable_path?(_path), do: true

  defp ensure_revision_worktree(repo, setup, branch, base_sha) do
    cond do
      File.dir?(setup) ->
        verify_revision_worktree(setup, branch, base_sha)

      branch_exists?(repo, branch) ->
        with {:ok, _} <- Git.run(repo, ["worktree", "add", setup, branch]) do
          verify_revision_worktree(setup, branch, base_sha)
        end

      true ->
        with :ok <- File.mkdir_p(Path.dirname(setup)),
             {:ok, _} <- Git.run(repo, ["worktree", "add", "-b", branch, setup, base_sha]) do
          :ok
        end
    end
  end

  defp verify_revision_worktree(setup, branch, base_sha) do
    with {:ok, "true"} <- Git.run(setup, ["rev-parse", "--is-inside-work-tree"]),
         {:ok, ^branch} <- Git.run(setup, ["branch", "--show-current"]),
         {:ok, _} <- Git.run(setup, ["merge-base", "--is-ancestor", base_sha, "HEAD"]) do
      :ok
    else
      {:ok, actual} -> {:error, {:setup_revision_mismatch, actual, branch, base_sha}}
      {:error, reason} -> {:error, {:invalid_setup_revision, reason}}
    end
  end

  defp branch_exists?(repo, branch) do
    match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))
  end

  defp validate_source(repo) do
    with true <- File.dir?(repo) || {:error, {:repo_not_found, repo}},
         {:ok, top_level} <- Git.run(repo, ["rev-parse", "--show-toplevel"]),
         true <- same_directory?(top_level, repo) || {:error, {:repo_must_be_git_root, repo}} do
      :ok
    else
      {:error, reason} -> {:error, reason}
    end
  end

  defp same_directory?(left, right) do
    with {:ok, left_stat} <- File.stat(left),
         {:ok, right_stat} <- File.stat(right) do
      left_stat.inode == right_stat.inode and left_stat.major_device == right_stat.major_device
    else
      _ -> false
    end
  end

  defp prepare_root(nil) do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-alignment-#{DateTime.utc_now() |> Calendar.strftime("%Y%m%d-%H%M%S")}-#{System.unique_integer([:positive])}"
      )

    File.mkdir_p!(root)
    {:ok, root}
  end

  defp prepare_root(path) do
    root = Path.expand(path)

    case File.ls(root) do
      {:error, :enoent} ->
        File.mkdir_p!(root)
        {:ok, root}

      {:ok, []} ->
        {:ok, root}

      {:ok, _} ->
        {:error, {:workspace_not_empty, root}}

      {:error, reason} ->
        {:error, {:workspace_unavailable, root, reason}}
    end
  end

  defp clone_and_initialize(source_repo, source_sha, source_status, root) do
    repo = Path.join(root, "repo")
    setup = Path.join(root, "setup/1")
    artifacts = Path.join(root, "artifacts")
    references = Path.join(root, "refs")

    with {:ok, _} <- Git.run(root, ["clone", "--no-hardlinks", source_repo, repo]),
         {:ok, _} <- Git.run(repo, ["remote", "remove", "origin"]),
         {:ok, _} <- Git.run(repo, ["config", "user.name", "Pika Alignment Agent"]),
         {:ok, _} <- Git.run(repo, ["config", "user.email", "pika-alignment@example.invalid"]),
         {:ok, _} <- Git.run(repo, ["checkout", "--detach", source_sha]),
         {:ok, _} <- Git.run(repo, ["branch", "-f", "pika/best", source_sha]),
         {:ok, _} <- Git.run(repo, ["checkout", "pika/best"]),
         :ok <- File.mkdir_p(references),
         :ok <- File.mkdir_p(Path.dirname(setup)),
         {:ok, _} <- Git.run(repo, ["worktree", "add", "-b", "pika/setup/1", setup, source_sha]) do
      Enum.each(~w(inputs logs prompts profiles baseline), fn dir ->
        File.mkdir_p!(Path.join(artifacts, dir))
      end)

      {:ok,
       %__MODULE__{
         root: root,
         source_repo: source_repo,
         source_sha: source_sha,
         source_status: source_status,
         repo: repo,
         setup_worktree: setup,
         artifacts: artifacts
       }}
    end
  end
end
