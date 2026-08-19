defmodule Pika.SyncWorkspace do
  @moduledoc false

  alias Pika.Git

  def preview(workspace, remote, branch) do
    with :ok <- validate_name(remote, :remote),
         :ok <- validate_name(branch, :branch),
         {:ok, remote_sha} <- remote_sha(workspace.repo, remote, branch),
         {:ok, _} <-
           Git.run(workspace.repo, ["fetch", "--no-tags", remote, "refs/heads/#{branch}"]),
         {:ok, best_sha} <- Git.head(workspace.repo),
         {:ok, relationship, pending} <- relationship(workspace.repo, remote_sha, best_sha) do
      {:ok,
       %{
         remote: remote,
         branch: branch,
         remote_sha: remote_sha,
         best_sha: best_sha,
         pending_commits: pending,
         relationship: relationship
       }}
    end
  end

  def prepare(workspace, run) do
    path = Path.join(workspace.root, run.worktree_relative_path)
    fetch_ref = fetch_ref(run.id)

    with :ok <- File.mkdir_p(Path.dirname(path)),
         {:ok, _} <-
           Git.run(workspace.repo, [
             "fetch",
             "--no-tags",
             run.remote,
             "+refs/heads/#{run.branch}:#{fetch_ref}"
           ]),
         {:ok, fetched_sha} <- Git.run(workspace.repo, ["rev-parse", fetch_ref]),
         true <- fetched_sha == run.remote_before_sha,
         :ok <- ensure_worktree(workspace.repo, path, run.sync_branch, run.base_sha) do
      {:ok, %{path: path, remote_ref: fetch_ref, remote_sha: fetched_sha}}
    else
      false -> {:error, :remote_changed_before_prepare}
      {:error, _} = error -> error
    end
  end

  def verify_candidate(workspace, run, candidate_sha, protected_paths) do
    path = Path.join(workspace.root, run.worktree_relative_path)

    with {:ok, ^candidate_sha} <- Git.head(path),
         true <- Git.clean?(path),
         :ok <- ancestor(workspace.repo, run.base_sha, candidate_sha, :best_not_ancestor),
         :ok <-
           ancestor(
             workspace.repo,
             run.remote_before_sha,
             candidate_sha,
             :remote_not_ancestor
           ),
         {:ok, changed} <-
           Git.run(workspace.repo, [
             "diff",
             "--name-only",
             "--no-renames",
             "#{run.base_sha}..#{candidate_sha}"
           ]) do
      changed = String.split(changed, "\n", trim: true)
      protected = MapSet.new(protected_paths)

      {:ok,
       %{
         changed_paths: changed,
         protected_paths: Enum.filter(changed, &MapSet.member?(protected, &1))
       }}
    else
      false -> {:error, :sync_worktree_dirty}
      {:ok, actual} -> {:error, {:candidate_head_mismatch, actual}}
      {:error, _} = error -> error
    end
  end

  def push(workspace, run, candidate_sha) do
    with {:ok, current} <- remote_sha(workspace.repo, run.remote, run.branch),
         true <- current == run.remote_before_sha,
         {:ok, _} <-
           Git.run(workspace.repo, [
             "push",
             run.remote,
             "#{candidate_sha}:refs/heads/#{run.branch}"
           ]),
         {:ok, ^candidate_sha} <- remote_sha(workspace.repo, run.remote, run.branch) do
      :ok
    else
      false -> {:error, :remote_changed_before_push}
      {:ok, actual} -> {:error, {:remote_push_mismatch, actual}}
      {:error, _} = error -> error
    end
  end

  def advance_best(workspace, expected_sha, candidate_sha) do
    with {:ok, "pika/best"} <- Git.run(workspace.repo, ["branch", "--show-current"]),
         {:ok, current_sha} <- Git.head(workspace.repo),
         :ok <- validate_best_head(current_sha, expected_sha, candidate_sha),
         :ok <- maybe_fast_forward(workspace.repo, current_sha, candidate_sha),
         {:ok, ^candidate_sha} <- Git.head(workspace.repo) do
      :ok
    else
      {:ok, actual} -> {:error, {:best_identity_mismatch, actual}}
      {:error, _} = error -> error
    end
  end

  def remote_sha(repo, remote, branch) do
    case Git.run(repo, ["ls-remote", "--heads", remote, "refs/heads/#{branch}"]) do
      {:ok, output} ->
        case String.split(output, ~r/\s+/, trim: true) do
          [sha, _ref] when byte_size(sha) in [40, 64] -> {:ok, sha}
          [] -> {:error, :remote_branch_missing}
          _ -> {:error, :invalid_remote_response}
        end

      {:error, _} = error ->
        error
    end
  end

  def cleanup(workspace, run) do
    path = Path.join(workspace.root, run.worktree_relative_path)

    with {:ok, _} <- remove_worktree(workspace.repo, path),
         {:ok, _} <- remove_branch(workspace.repo, run.sync_branch),
         {:ok, _} <- remove_ref(workspace.repo, fetch_ref(run.id)) do
      :ok
    end
  end

  defp relationship(repo, remote_sha, best_sha) do
    cond do
      remote_sha == best_sha ->
        {:ok, "equal", 0}

      ancestor?(repo, remote_sha, best_sha) ->
        {:ok, count} = Git.run(repo, ["rev-list", "--count", "#{remote_sha}..#{best_sha}"])
        {:ok, "local_ahead", String.to_integer(count)}

      ancestor?(repo, best_sha, remote_sha) ->
        {:ok, "remote_ahead", 0}

      true ->
        {:ok, "diverged", 0}
    end
  end

  defp ensure_worktree(repo, path, branch, base_sha) do
    cond do
      File.dir?(path) ->
        with {:ok, ^branch} <- Git.run(path, ["branch", "--show-current"]) do
          :ok
        else
          {:ok, actual} -> {:error, {:sync_branch_mismatch, actual}}
          {:error, _} = error -> error
        end

      branch_exists?(repo, branch) ->
        case Git.run(repo, ["worktree", "add", path, branch]) do
          {:ok, _} -> :ok
          {:error, _} = error -> error
        end

      true ->
        case Git.run(repo, ["worktree", "add", "-b", branch, path, base_sha]) do
          {:ok, _} -> :ok
          {:error, _} = error -> error
        end
    end
  end

  defp ancestor(repo, older, newer, reason) do
    if ancestor?(repo, older, newer), do: :ok, else: {:error, reason}
  end

  defp ancestor?(repo, older, newer) do
    match?({:ok, _}, Git.run(repo, ["merge-base", "--is-ancestor", older, newer]))
  end

  defp validate_best_head(candidate_sha, _expected_sha, candidate_sha), do: :ok
  defp validate_best_head(expected_sha, expected_sha, _candidate_sha), do: :ok

  defp validate_best_head(actual, _expected_sha, _candidate_sha),
    do: {:error, {:best_changed, actual}}

  defp maybe_fast_forward(_repo, sha, sha), do: :ok

  defp maybe_fast_forward(repo, _current_sha, candidate_sha) do
    case Git.run(repo, ["merge", "--ff-only", candidate_sha]) do
      {:ok, _} -> :ok
      {:error, _} = error -> error
    end
  end

  defp remove_worktree(repo, path) do
    if File.exists?(path),
      do: Git.run(repo, ["worktree", "remove", "--force", path]),
      else: {:ok, ""}
  end

  defp remove_branch(repo, branch) do
    if branch_exists?(repo, branch),
      do: Git.run(repo, ["branch", "-D", branch]),
      else: {:ok, ""}
  end

  defp remove_ref(repo, ref) do
    case Git.run(repo, ["show-ref", "--verify", "--quiet", ref]) do
      {:ok, _} -> Git.run(repo, ["update-ref", "-d", ref])
      {:error, _} -> {:ok, ""}
    end
  end

  defp branch_exists?(repo, branch),
    do:
      match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))

  defp fetch_ref(id), do: "refs/pika/sync/#{id}/remote"

  defp validate_name(value, _field) when is_binary(value) and value != "", do: :ok
  defp validate_name(_value, field), do: {:error, {:missing_sync_config, field}}
end
