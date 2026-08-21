defmodule Pika.ManagedWorkspaceTest do
  use ExUnit.Case, async: false

  alias Pika.Git
  alias Pika.Test.{AlignmentFixtures, CampaignFixtures}
  alias Pika.{Config, Workspace, WorkspaceLock}

  test "rejects a dirty Managed Repo before creating the Workspace" do
    repo = AlignmentFixtures.git_repo(dirty: true)
    root = missing_workspace()
    config_path = CampaignFixtures.config_file()
    {:ok, config} = Config.load(config_path, workspace: root, repo: repo)

    assert {:error, {:invalid_managed_repo, _, _}} = Workspace.plan(config)
    refute File.exists?(root)
  end

  test "holds an OS advisory lock with diagnostics and rejects a second owner without writes" do
    repo = AlignmentFixtures.git_repo()
    first_root = missing_workspace()
    second_root = missing_workspace()
    config_path = CampaignFixtures.config_file()
    {:ok, first_config} = Config.load(config_path, workspace: first_root, repo: repo)
    {:ok, second_config} = Config.load(config_path, workspace: second_root, repo: repo)
    {:ok, first_plan} = Workspace.plan(first_config)
    {:ok, second_plan} = Workspace.plan(second_config)

    {:ok, owner} = WorkspaceLock.start_link({first_plan, name: unique_name("owner")})
    Process.unlink(owner)
    on_exit(fn -> if Process.alive?(owner), do: Process.exit(owner, :kill) end)

    lock_path = first_plan.lock_path
    diagnostic = lock_path |> File.read!() |> Jason.decode!()
    assert diagnostic["pid"] == System.pid()
    assert diagnostic["workspace"] == first_config.workspace
    assert is_binary(diagnostic["server_uuid"])
    assert is_binary(diagnostic["started_at"])

    repo_status = Git.run!(repo, ["status", "--porcelain=v1", "--untracked-files=all"])
    refs_before = Git.run!(repo, ["show-ref"])

    assert {:error, {:managed_repo_locked, ^lock_path, _diagnostic}} =
             WorkspaceLock.start({second_plan, name: unique_name("contender")})

    refute File.exists?(second_root)
    assert Git.run!(repo, ["status", "--porcelain=v1", "--untracked-files=all"]) == repo_status
    assert Git.run!(repo, ["show-ref"]) == refs_before
  end

  test "rejects recovery when the repo symlink itself is replaced" do
    %{repo: repo, root: root, config: config_path} = initialized_managed_workspace()
    link = Path.join(root, "repo")
    old_inode = File.lstat!(link).inode

    replace_symlink_with_distinct_inode(link, repo, old_inode)

    {:ok, config} = Config.load(config_path, workspace: root, repo: repo)
    assert {:error, {:workspace_identity_mismatch, _, _}} = Workspace.plan(config)
  end

  test "rejects recovery when the Managed Repo directory inode changes" do
    %{repo: repo, root: root, config: config_path} = initialized_managed_workspace()
    original_inode = File.stat!(repo).inode
    moved = repo <> "-moved"
    File.rename!(repo, moved)
    {:ok, _copied} = File.cp_r(moved, repo)
    refute File.stat!(repo).inode == original_inode

    {:ok, config} = Config.load(config_path, workspace: root, repo: repo)
    assert {:error, {:workspace_identity_mismatch, _, _}} = Workspace.plan(config)
  end

  test "rejects recovery when the Git common directory changes" do
    %{repo: repo, root: root, config: config_path} = initialized_managed_workspace()
    old_git = Path.join(repo, ".git")
    new_git = repo <> "-git-common"
    File.rename!(old_git, new_git)
    File.write!(old_git, "gitdir: #{new_git}\n")
    assert Git.clean?(repo)

    {:ok, config} = Config.load(config_path, workspace: root, repo: repo)
    assert {:error, {:workspace_identity_mismatch, _, _}} = Workspace.plan(config)
  end

  defp initialized_managed_workspace do
    repo = AlignmentFixtures.git_repo()
    root = missing_workspace()
    config_path = CampaignFixtures.config_file()
    {:ok, config} = Config.load(config_path, workspace: root, repo: repo)
    {:ok, plan} = Workspace.plan(config)
    name = unique_name("identity")
    {:ok, owner} = WorkspaceLock.start_link({plan, name: name})
    workspace = WorkspaceLock.workspace(name)
    File.touch!(workspace.database)
    GenServer.stop(owner)
    %{repo: config.repo, root: config.workspace, config: config_path}
  end

  defp replace_symlink_with_distinct_inode(link, target, old_inode) do
    replacement = link <> ".replacement-#{System.unique_integer([:positive])}"
    File.ln_s!(target, replacement)
    refute File.lstat!(replacement).inode == old_inode
    File.rename!(replacement, link)
  end

  defp missing_workspace do
    root = CampaignFixtures.workspace()
    File.rmdir!(root)
    root
  end

  defp unique_name(prefix),
    do: String.to_atom("#{prefix}_#{System.unique_integer([:positive])}")
end
