defmodule Pika.Attempt.WorkspaceTest do
  use ExUnit.Case, async: true

  alias Pika.Attempt.Workspace
  alias Pika.Git

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-attempt-workspace-#{System.unique_integer([:positive])}")

    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    target_snapshot = Path.join(root, "target-snapshot")
    File.mkdir_p!(Path.join(repo, "target"))
    File.mkdir_p!(target_snapshot)

    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "kernel.py"), "def run(): return 1\n")
    File.write!(Path.join(repo, "target/kernel.bin"), "checked-out target\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "baseline"])
    sha = Git.run!(repo, ["rev-parse", "HEAD"])

    File.write!(Path.join(target_snapshot, "kernel.bin"), "reviewed immutable target\n")
    on_exit(fn -> File.rm_rf!(root) end)

    %{repo: repo, workspace: workspace, target_snapshot: target_snapshot, sha: sha}
  end

  test "overlays a clean checked-out Target with the immutable snapshot", context do
    assert {:ok, paths} =
             Workspace.prepare(
               context.repo,
               context.workspace,
               1,
               context.sha,
               context.target_snapshot
             )

    assert File.read_link!(Path.join(paths.repo, "target")) == context.target_snapshot
    assert File.read!(Path.join(paths.repo, "target/kernel.bin")) == "reviewed immutable target\n"
    assert Git.run!(paths.repo, ["status", "--porcelain=v1"]) == ""
  end

  test "refuses to replace a Target tree with local changes", context do
    paths = Workspace.paths(context.workspace, 1)
    File.mkdir_p!(paths.root)
    Git.run!(context.repo, ["worktree", "add", "-b", paths.branch, paths.repo, context.sha])
    File.write!(Path.join(paths.repo, "target/kernel.bin"), "local mutation\n")

    assert {:error, {:target_link_conflict, link}} =
             Workspace.prepare(
               context.repo,
               context.workspace,
               1,
               context.sha,
               context.target_snapshot
             )

    assert link == Path.join(paths.repo, "target")
    assert File.regular?(Path.join(paths.root, "message.jsonl"))
    assert File.regular?(Path.join(paths.root, "summary.jsonl"))
  end
end
