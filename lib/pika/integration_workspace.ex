defmodule Pika.IntegrationWorkspace do
  @moduledoc false

  alias Pika.{Git, Harness}

  def verify_merge(workspace, attempt, receipt, intent, new_sha) do
    with {:ok, "pika/best"} <- Git.run(workspace.repo, ["branch", "--show-current"]),
         {:ok, ^new_sha} <- Git.head(workspace.repo),
         {:ok, parents} <- Git.run(workspace.repo, ["rev-list", "--parents", "-n", "1", new_sha]),
         :ok <- verify_single_parent(parents, new_sha, intent.expected_best_sha),
         :ok <-
           Harness.verify_candidate(
             workspace.repo,
             intent.expected_best_sha,
             new_sha,
             %{protected_paths: protected_paths(attempt.spec_revision_id)}
           ),
         {:ok, message} <- Git.run(workspace.repo, ["show", "-s", "--format=%B", new_sha]),
         :ok <- verify_trailers(message, attempt),
         :ok <- verify_patch(workspace, attempt, intent.expected_best_sha, new_sha),
         true <- receipt.base_sha == intent.expected_best_sha,
         true <- receipt.candidate_sha == attempt.candidate_sha do
      :ok
    else
      false -> {:error, :merge_identity_mismatch}
      {:ok, actual} -> {:error, {:merge_verification_mismatch, actual}}
      {:error, _} = error -> error
    end
  end

  def cleanup(workspace, attempt) do
    path = Path.join(workspace.root, attempt.worktree_relative_path)

    with {:ok, _} <- remove_worktree(workspace.repo, path),
         {:ok, _} <- remove_branch(workspace.repo, attempt.branch_name) do
      :ok
    end
  end

  defp verify_single_parent(parents, new_sha, expected_parent) do
    case String.split(parents) do
      [^new_sha, ^expected_parent] -> :ok
      values -> {:error, {:invalid_merge_parents, values, expected_parent}}
    end
  end

  defp verify_trailers(message, attempt) do
    required = [
      "Pika-Attempt: #{attempt.id}",
      "Pika-Spec-Revision: #{attempt.spec_revision_id}",
      "Pika-Sampling-Revision: #{attempt.sampling_revision_id}"
    ]

    case Enum.reject(required, &String.contains?(message, &1)) do
      [] -> :ok
      missing -> {:error, {:missing_merge_trailers, missing}}
    end
  end

  defp verify_patch(workspace, attempt, old_sha, new_sha) do
    patch_path = Path.join(workspace.root, "artifacts/patches/#{attempt.id}/candidate.patch")

    with {:ok, expected} <- File.read(patch_path),
         {:ok, actual} <-
           Git.run(workspace.repo, [
             "diff",
             "--binary",
             "--full-index",
             "#{old_sha}..#{new_sha}",
             "--",
             "."
           ]),
         actual <- actual <> if(actual == "", do: "", else: "\n"),
         true <- expected == actual do
      :ok
    else
      false -> {:error, :merge_patch_mismatch}
      {:error, reason} -> {:error, {:merge_patch_read_failed, reason}}
    end
  end

  defp protected_paths(spec_revision_id) do
    case Pika.Repo.query!("SELECT protected_paths_json FROM spec_revisions WHERE id = ?", [
           spec_revision_id
         ]).rows do
      [[paths]] -> Jason.decode!(paths)
      [] -> []
    end
  end

  defp remove_worktree(repo, path) do
    if File.exists?(path),
      do: Git.run(repo, ["worktree", "remove", "--force", path]),
      else: {:ok, ""}
  end

  defp remove_branch(repo, branch) do
    case Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]) do
      {:ok, _} -> Git.run(repo, ["branch", "-D", branch])
      {:error, _} -> {:ok, ""}
    end
  end
end
