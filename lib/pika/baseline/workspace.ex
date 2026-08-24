defmodule Pika.Baseline.Workspace do
  @moduledoc "Creates one immutable-numbered Baseline branch/worktree directory per v2 Revision."

  alias Pika.Baseline.Lifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Git

  @spec paths(Path.t(), non_neg_integer()) :: map()
  def paths(workspace_root, revision) do
    name = revision |> Integer.to_string() |> String.pad_leading(6, "0")
    root = Path.join([workspace_root, "baseline", "revisions", name])

    %{
      root: root,
      repo: Path.join(root, "repo"),
      target: Path.join(root, "target"),
      branch: "pika/baseline/#{name}",
      relative_root: Path.join(["baseline", "revisions", name])
    }
  end

  @spec ensure_current(Config.t()) :: {:ok, map()} | {:error, term()}
  def ensure_current(%Config{} = config) do
    case Lifecycle.latest_revision() do
      nil ->
        prepare_and_register(config, 0)

      %{status: "superseded", revision: revision, development_sha: development_sha} ->
        prepare_and_register(config, revision + 1, development_sha)

      revision ->
        {:ok, %{revision: revision, paths: paths(config.workspace, revision.revision)}}
    end
  end

  defp prepare_and_register(config, revision, base_sha \\ nil) do
    base_sha = base_sha || Persistence.current().best_sha || Persistence.current().initial_sha
    paths = paths(config.workspace, revision)

    with :ok <- File.mkdir_p(paths.root),
         :ok <- prepare_worktree(config.repo, paths, base_sha),
         :ok <- File.mkdir_p(paths.target),
         {:ok, baseline_revision} <- Lifecycle.ensure_draft(paths.root) do
      {:ok, %{revision: baseline_revision, paths: paths}}
    end
  end

  defp prepare_worktree(source_repo, paths, sha) do
    cond do
      git_worktree?(paths.repo) -> verify_worktree(paths, sha)
      branch_exists?(source_repo, paths.branch) -> add_existing_worktree(source_repo, paths, sha)
      true -> create_worktree(source_repo, paths, sha)
    end
  end

  defp add_existing_worktree(source_repo, paths, sha) do
    with {:ok, _output} <- Git.run(source_repo, ["worktree", "add", paths.repo, paths.branch]),
         do: verify_worktree(paths, sha)
  end

  defp create_worktree(source_repo, paths, sha) do
    with {:ok, _output} <-
           Git.run(source_repo, ["worktree", "add", "-b", paths.branch, paths.repo, sha]),
         do: verify_worktree(paths, sha)
  end

  defp verify_worktree(paths, sha) do
    with {:ok, branch} <- Git.run(paths.repo, ["branch", "--show-current"]),
         true <- branch == paths.branch,
         {:ok, head} <- Git.run(paths.repo, ["rev-parse", "HEAD"]),
         true <- head == sha do
      :ok
    else
      false -> {:error, :baseline_worktree_identity_mismatch}
      {:error, reason} -> {:error, {:baseline_worktree_git_failed, reason}}
    end
  end

  defp git_worktree?(path),
    do: File.dir?(Path.join(path, ".git")) or File.regular?(Path.join(path, ".git"))

  defp branch_exists?(repo, branch),
    do:
      match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))
end
