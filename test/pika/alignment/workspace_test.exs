defmodule Pika.Alignment.WorkspaceTest do
  use ExUnit.Case, async: true

  alias Pika.Git
  alias Pika.Alignment.Workspace
  alias Pika.Test.AlignmentFixtures

  test "re-clones committed HEAD from a dirty source and leaves dirty state unchanged" do
    repo = AlignmentFixtures.git_repo(dirty: true)
    {:ok, head} = Git.head(repo)
    status_before = Git.run!(repo, ["status", "--porcelain=v1", "--untracked-files=normal"])

    assert {:ok, workspace} = Workspace.prepare(repo)
    assert Git.run!(repo, ["rev-parse", "HEAD"]) == head
    refute File.exists?(Path.join(workspace.repo, "dirty.txt"))
    assert workspace.source_status == status_before

    assert Git.run!(repo, ["status", "--porcelain=v1", "--untracked-files=normal"]) ==
             status_before
  end

  test "creates an independent clone, best branch and setup worktree" do
    repo = AlignmentFixtures.git_repo()
    {:ok, workspace} = Workspace.prepare(repo)
    assert File.dir?(workspace.repo)
    assert File.dir?(workspace.setup_worktree)
    assert Git.run!(workspace.repo, ["rev-parse", "pika/best"]) == workspace.source_sha

    assert Git.run!(workspace.setup_worktree, ["rev-parse", "--abbrev-ref", "HEAD"]) ==
             "pika/setup/1"

    assert Git.run!(workspace.repo, ["remote"]) == ""

    assert :ok = Workspace.verify_source_unchanged(workspace)

    File.write!(Path.join(workspace.setup_worktree, "only-in-clone.txt"), "safe\n")
    refute File.exists?(Path.join(repo, "only-in-clone.txt"))
  end

  test "rejects non-empty explicit Workspace" do
    repo = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-nonempty")
    File.write!(Path.join(root, "existing"), "x")
    assert {:error, {:workspace_not_empty, ^root}} = Workspace.prepare(repo, workspace: root)
  end
end
