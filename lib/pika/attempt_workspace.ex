defmodule Pika.AttemptWorkspace do
  @moduledoc false

  alias Pika.{Git, ReferenceCatalog}

  def create(workspace, attempt_id, best_sha, references) do
    relative = Path.join("attempts", attempt_id)
    path = Path.join(workspace.root, relative)
    branch = "pika/attempt/#{attempt_id}"

    with :ok <- ensure_attempt_directories(workspace.root, attempt_id),
         :ok <- ensure_worktree(workspace.repo, path, branch, best_sha),
         :ok <- materialize_references(workspace.root, path, references),
         {:ok, head} <- Git.head(path),
         true <- head == best_sha do
      {:ok,
       %{
         id: attempt_id,
         path: path,
         relative_path: relative,
         branch: branch,
         base_sha: best_sha
       }}
    else
      false -> {:error, {:attempt_head_mismatch, best_sha}}
      {:error, _} = error -> error
    end
  end

  def verify_candidate(workspace, attempt, candidate_sha, protected) do
    with {:ok, branch} <- Git.run(attempt.path, ["branch", "--show-current"]),
         true <- branch == attempt.branch,
         {:ok, head} <- Git.head(attempt.path),
         true <- head == candidate_sha,
         true <- Git.clean?(attempt.path),
         :ok <-
           Pika.Harness.verify_candidate(
             workspace.repo,
             attempt.base_sha,
             candidate_sha,
             protected
           ) do
      :ok
    else
      false -> {:error, :attempt_worktree_not_clean_or_head_mismatch}
      {:error, _} = error -> error
    end
  end

  def patch(workspace, attempt, candidate_sha) do
    args = [
      "diff",
      "--binary",
      "--full-index",
      "#{attempt.base_sha}...#{candidate_sha}",
      "--",
      ".",
      ":(exclude)ref/**"
    ]

    with {:ok, patch} <- Git.run(workspace.repo, args),
         false <- patch_contains_injected_paths?(patch) do
      {:ok, patch <> if(patch == "", do: "", else: "\n")}
    else
      true -> {:error, :injected_reference_in_patch}
      {:error, _} = error -> error
    end
  end

  def current(workspace, attempt) do
    path = Path.join(workspace.root, attempt.worktree_relative_path)

    {:ok,
     %{
       id: attempt.id,
       path: path,
       relative_path: attempt.worktree_relative_path,
       branch: attempt.branch_name,
       base_sha: attempt.base_sha
     }}
  end

  defp ensure_attempt_directories(root, attempt_id) do
    Enum.reduce_while(~w(patches profiles prompts logs plans), :ok, fn kind, :ok ->
      path = Path.join([root, "artifacts", kind, attempt_id])

      case File.mkdir_p(path) do
        :ok -> {:cont, :ok}
        {:error, reason} -> {:halt, {:error, {:attempt_artifact_directory_failed, kind, reason}}}
      end
    end)
  end

  defp ensure_worktree(repo, path, branch, best_sha) do
    cond do
      File.dir?(path) ->
        verify_existing(path, branch)

      branch_exists?(repo, branch) ->
        case Git.run(repo, ["worktree", "add", path, branch]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:attempt_worktree_restore_failed, reason}}
        end

      true ->
        case Git.run(repo, ["worktree", "add", "-b", branch, path, best_sha]) do
          {:ok, _} -> :ok
          {:error, reason} -> {:error, {:attempt_worktree_create_failed, reason}}
        end
    end
  end

  defp verify_existing(path, branch) do
    with {:ok, "true"} <- Git.run(path, ["rev-parse", "--is-inside-work-tree"]),
         {:ok, ^branch} <- Git.run(path, ["branch", "--show-current"]) do
      :ok
    else
      {:ok, actual} -> {:error, {:attempt_branch_mismatch, actual, branch}}
      {:error, reason} -> {:error, {:invalid_attempt_worktree, reason}}
    end
  end

  defp materialize_references(_workspace_root, _path, []), do: :ok

  defp materialize_references(workspace_root, path, references) do
    references =
      Enum.map(references, fn reference ->
        %{
          id: reference[:id] || reference["id"],
          url: reference[:url] || reference["url"],
          description: reference[:description] || reference["description"] || "",
          selected: Map.get(reference, :selected, Map.get(reference, "selected", true)),
          branch: reference[:branch] || reference["branch"],
          sha: reference[:sha] || reference["sha"],
          status: reference_status(reference[:status] || reference["status"])
        }
      end)

    case ReferenceCatalog.materialize_selected(workspace_root, references, link_into: path) do
      {:ok, _} -> :ok
      {:error, failures} -> {:error, {:reference_materialization_failed, failures}}
    end
  end

  defp reference_status(nil), do: :resolved
  defp reference_status(status) when is_atom(status), do: status
  defp reference_status("resolved"), do: :resolved
  defp reference_status("unresolved"), do: :unresolved
  defp reference_status("error"), do: :error
  defp reference_status(_status), do: :resolved

  defp branch_exists?(repo, branch) do
    match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))
  end

  defp patch_contains_injected_paths?(patch) do
    String.contains?(patch, [" a/ref/", " b/ref/"])
  end
end
