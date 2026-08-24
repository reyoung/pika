defmodule Pika.Attempt.Workspace do
  @moduledoc "Creates and verifies integer-ID Attempt Git worktrees."

  alias Pika.Git

  @spec paths(Path.t(), pos_integer()) :: map()
  def paths(workspace_root, attempt_id) do
    name = attempt_id |> Integer.to_string() |> String.pad_leading(6, "0")
    root = Path.join([workspace_root, "attempts", name])

    %{
      root: root,
      repo: Path.join(root, "repo"),
      branch: "pika/attempt/#{name}",
      relative_root: Path.join("attempts", name)
    }
  end

  @spec prepare(Path.t(), Path.t(), pos_integer(), String.t(), Path.t()) ::
          {:ok, map()} | {:error, term()}
  def prepare(best_repo, workspace_root, attempt_id, best_sha, target_root) do
    paths = paths(workspace_root, attempt_id)

    with :ok <- File.mkdir_p(paths.root),
         :ok <- prepare_worktree(best_repo, paths, best_sha),
         :ok <- verify_worktree(paths, best_sha),
         :ok <- expose_target(paths.repo, target_root),
         :ok <- ensure_journals(paths.root) do
      {:ok, paths}
    end
  end

  defp prepare_worktree(best_repo, paths, best_sha) do
    cond do
      File.dir?(Path.join(paths.repo, ".git")) or File.regular?(Path.join(paths.repo, ".git")) ->
        :ok

      branch_exists?(best_repo, paths.branch) ->
        git_ok(best_repo, ["worktree", "add", paths.repo, paths.branch])

      true ->
        git_ok(best_repo, ["worktree", "add", "-b", paths.branch, paths.repo, best_sha])
    end
  end

  defp verify_worktree(paths, best_sha) do
    with {:ok, branch} <- Git.run(paths.repo, ["branch", "--show-current"]),
         true <- String.trim(branch) == paths.branch,
         {:ok, head} <- Git.run(paths.repo, ["rev-parse", "HEAD"]),
         true <- String.trim(head) == best_sha do
      :ok
    else
      false -> {:error, :attempt_worktree_identity_mismatch}
      {:error, reason} -> {:error, {:attempt_worktree_git_failed, reason}}
    end
  end

  defp expose_target(repo, target_root) do
    link = Path.join(repo, "target")

    cond do
      not File.dir?(target_root) ->
        {:error, {:target_snapshot_missing, target_root}}

      File.exists?(link) and File.read_link(link) == {:ok, target_root} ->
        :ok

      File.exists?(link) ->
        {:error, {:target_link_conflict, link}}

      true ->
        with :ok <- File.ln_s(target_root, link),
             :ok <- exclude_target(repo) do
          :ok
        end
    end
  end

  defp exclude_target(repo) do
    with {:ok, git_dir} <- Git.run(repo, ["rev-parse", "--git-path", "info/exclude"]) do
      path = git_dir |> String.trim() |> absolute_git_path(repo)
      File.mkdir_p!(Path.dirname(path))
      existing = if File.regular?(path), do: File.read!(path), else: ""

      if existing |> String.split("\n") |> Enum.member?("/target") do
        :ok
      else
        File.write(
          path,
          existing <>
            if(String.ends_with?(existing, "\n") or existing == "", do: "", else: "\n") <>
            "/target\n"
        )
      end
    end
  end

  defp ensure_journals(root) do
    Enum.reduce_while(~w(message.jsonl summary.jsonl), :ok, fn filename, :ok ->
      path = Path.join(root, filename)

      case File.open(path, [:append, :utf8]) do
        {:ok, io} ->
          File.close(io)
          {:cont, :ok}

        {:error, reason} ->
          {:halt, {:error, {:attempt_journal_failed, filename, reason}}}
      end
    end)
  end

  defp branch_exists?(repo, branch) do
    match?({:ok, _}, Git.run(repo, ["show-ref", "--verify", "--quiet", "refs/heads/#{branch}"]))
  end

  defp git_ok(repo, args) do
    case Git.run(repo, args) do
      {:ok, _output} -> :ok
      {:error, reason} -> {:error, {:attempt_worktree_creation_failed, reason}}
    end
  end

  defp absolute_git_path(path, repo) do
    if Path.type(path) == :absolute, do: path, else: Path.expand(path, repo)
  end
end
