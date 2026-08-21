defmodule Pika.CampaignWorkspace do
  @moduledoc false

  alias Pika.Git

  def prepare(workspace, campaign, revision \\ 1) do
    setup = Path.join(workspace.root, "setup/#{revision}")
    branch = "pika/setup/#{revision}"

    with :ok <- ensure_artifact_directories(workspace.root),
         :ok <- ensure_git_identity(workspace.repo),
         :ok <- ensure_best_checkout(workspace.repo, campaign.best_branch),
         :ok <- ensure_setup_worktree(workspace.repo, setup, branch, campaign.best_sha),
         {:ok, status} <-
           Git.run(workspace.repo, ["status", "--porcelain=v1", "--untracked-files=normal"]) do
      {:ok,
       %Pika.Alignment.Workspace{
         root: workspace.root,
         source_repo: workspace.repo,
         source_sha: campaign.best_sha,
         source_status: status,
         repo: workspace.repo,
         setup_worktree: setup,
         artifacts: workspace.artifacts
       }}
    end
  end

  defp ensure_artifact_directories(root) do
    with :ok <- ensure_reference_directory(root),
         :ok <- ensure_target_directory(root) do
      Enum.reduce_while(~w(inputs baseline profiles logs prompts), :ok, fn directory, :ok ->
        case File.mkdir_p(Path.join([root, "artifacts", directory])) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, {:artifact_directory_failed, directory, reason}}}
        end
      end)
    end
  end

  defp ensure_reference_directory(root) do
    case File.mkdir_p(Path.join(root, "refs")) do
      :ok -> :ok
      {:error, reason} -> {:error, {:reference_directory_failed, reason}}
    end
  end

  defp ensure_target_directory(root) do
    case File.mkdir_p(Path.join(root, "targets")) do
      :ok -> :ok
      {:error, reason} -> {:error, {:target_directory_failed, reason}}
    end
  end

  defp ensure_best_checkout(repo, branch) do
    with {:ok, current} <- Git.run(repo, ["branch", "--show-current"]) do
      if current == branch do
        :ok
      else
        case Git.run(repo, ["checkout", branch]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:best_checkout_failed, reason}}
        end
      end
    end
  end

  defp ensure_git_identity(repo) do
    with :ok <- ensure_git_config(repo, "user.name", "Pika Agent"),
         :ok <- ensure_git_config(repo, "user.email", "pika@localhost") do
      :ok
    end
  end

  defp ensure_git_config(repo, key, default) do
    case Git.run(repo, ["config", "--get", key]) do
      {:ok, value} when value != "" ->
        :ok

      _ ->
        case Git.run(repo, ["config", key, default]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:git_identity_failed, key, reason}}
        end
    end
  end

  defp ensure_setup_worktree(repo, setup, branch, best_sha) do
    cond do
      File.dir?(setup) ->
        verify_setup_worktree(repo, setup, branch, best_sha)

      branch_exists?(repo, branch) ->
        case Git.run(repo, ["worktree", "add", setup, branch]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:setup_worktree_restore_failed, reason}}
        end

      true ->
        File.mkdir_p!(Path.dirname(setup))

        case Git.run(repo, ["worktree", "add", "-b", branch, setup, best_sha]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:setup_worktree_create_failed, reason}}
        end
    end
  end

  defp verify_setup_worktree(repo, setup, branch, persisted_best_sha) do
    with {:ok, "true"} <- Git.run(setup, ["rev-parse", "--is-inside-work-tree"]),
         {:ok, actual_branch} <- Git.run(setup, ["branch", "--show-current"]) do
      case actual_branch do
        ^branch -> :ok
        "" -> verify_detached_setup_checkpoint(repo, setup, persisted_best_sha)
        actual -> {:error, {:setup_branch_mismatch, actual, branch}}
      end
    else
      {:ok, actual} -> {:error, {:setup_branch_mismatch, actual, branch}}
      {:error, reason} -> {:error, {:invalid_setup_worktree, reason}}
    end
  end

  defp verify_detached_setup_checkpoint(repo, setup, persisted_best_sha) do
    with {:ok, setup_sha} <- Git.run(setup, ["rev-parse", "HEAD"]),
         {:ok, actual_best_sha} <- Git.run(repo, ["rev-parse", "pika/best"]),
         true <- setup_sha == actual_best_sha,
         true <-
           actual_best_sha == persisted_best_sha or
             commit_parent?(repo, actual_best_sha, persisted_best_sha) do
      :ok
    else
      false -> {:error, {:setup_detached_checkpoint_mismatch, persisted_best_sha}}
      {:error, reason} -> {:error, {:invalid_setup_worktree, reason}}
    end
  end

  defp commit_parent?(repo, commit, expected_parent) do
    match?({:ok, ^expected_parent}, Git.run(repo, ["rev-parse", "#{commit}^"]))
  end

  defp branch_exists?(repo, branch) do
    match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))
  end
end
