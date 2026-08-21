defmodule Pika.CampaignWorkspaceTest do
  use ExUnit.Case, async: true

  alias Pika.CampaignWorkspace
  alias Pika.Git
  alias Pika.Test.AlignmentFixtures

  test "recovers a detached setup worktree after the squash commit reached pika/best" do
    source = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-campaign-workspace-recovery")
    repo = Path.join(root, "repo")
    setup = Path.join(root, "setup/1")
    artifacts = Path.join(root, "artifacts")

    Git.run!(root, ["clone", "--no-hardlinks", source, repo])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika@example.invalid"])
    base_sha = Git.run!(repo, ["rev-parse", "HEAD"])
    Git.run!(repo, ["branch", "-f", "pika/best", base_sha])
    Git.run!(repo, ["checkout", "pika/best"])
    File.mkdir_p!(Path.dirname(setup))
    Git.run!(repo, ["worktree", "add", "-b", "pika/setup/1", setup, base_sha])

    File.mkdir_p!(Path.join(setup, "kernel"))
    File.write!(Path.join(setup, "kernel/optimized.py"), "optimized = true\n")
    Git.run!(setup, ["add", "kernel/optimized.py"])
    Git.run!(setup, ["commit", "-m", "prepare setup"])
    setup_sha = Git.run!(setup, ["rev-parse", "HEAD"])

    Git.run!(repo, ["merge", "--squash", setup_sha])
    Git.run!(repo, ["commit", "-m", "squash setup"])
    best_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    Git.run!(setup, ["checkout", "--detach", best_sha])

    workspace = %{root: root, repo: repo, artifacts: artifacts}
    campaign = %{best_branch: "pika/best", best_sha: base_sha}

    assert {:ok, recovered} = CampaignWorkspace.prepare(workspace, campaign)
    assert recovered.source_sha == base_sha
    assert Git.run!(recovered.setup_worktree, ["branch", "--show-current"]) == ""
    assert Git.run!(recovered.setup_worktree, ["rev-parse", "HEAD"]) == best_sha
    assert Git.run!(recovered.repo, ["rev-parse", "pika/best"]) == best_sha
  end

  test "rejects a detached setup worktree unrelated to pika/best" do
    source = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-campaign-workspace-mismatch")
    repo = Path.join(root, "repo")
    setup = Path.join(root, "setup/1")

    Git.run!(root, ["clone", "--no-hardlinks", source, repo])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika@example.invalid"])
    base_sha = Git.run!(repo, ["rev-parse", "HEAD"])
    Git.run!(repo, ["branch", "-f", "pika/best", base_sha])
    Git.run!(repo, ["checkout", "pika/best"])
    File.mkdir_p!(Path.dirname(setup))
    Git.run!(repo, ["worktree", "add", "-b", "pika/setup/1", setup, base_sha])

    File.write!(Path.join(setup, "unrelated.txt"), "unrelated\n")
    Git.run!(setup, ["add", "unrelated.txt"])
    Git.run!(setup, ["commit", "-m", "unrelated detached commit"])
    Git.run!(setup, ["checkout", "--detach"])

    workspace = %{root: root, repo: repo, artifacts: Path.join(root, "artifacts")}
    campaign = %{best_branch: "pika/best", best_sha: base_sha}

    assert {:error, {:setup_detached_checkpoint_mismatch, ^base_sha}} =
             CampaignWorkspace.prepare(workspace, campaign)
  end
end
