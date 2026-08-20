defmodule Pika.ReferenceCatalogTest do
  use ExUnit.Case, async: true

  alias Pika.{Git, ReferenceCatalog}
  alias Pika.Test.AlignmentFixtures

  test "contains the 16 default-selected Atrex references" do
    entries = ReferenceCatalog.entries()
    assert length(entries) == 16
    assert Enum.all?(entries, & &1.selected)
    assert Enum.all?(entries, &(&1.origin == :builtin))
    assert Enum.uniq(Enum.map(entries, & &1.id)) == Enum.map(entries, & &1.id)
  end

  test "builds a selected Campaign Reference Project from a user Git URL" do
    assert {:ok, entry} =
             ReferenceCatalog.new_user_entry(
               %{
                 "url" => "  https://github.com/example/fast-kernels.git  ",
                 "description" => "  Experimental kernels  "
               },
               ReferenceCatalog.entries()
             )

    assert entry == %{
             id: "fast-kernels",
             url: "https://github.com/example/fast-kernels.git",
             description: "Experimental kernels",
             origin: :user,
             selected: true,
             status: :unresolved,
             sha: nil,
             branch: nil
           }
  end

  test "accepts explicit safe IDs, SSH locations, and absolute repository paths" do
    assert {:ok, ssh_entry} =
             ReferenceCatalog.new_user_entry(%{
               id: "FA4.local",
               url: "git@github.com:example/flash-attention.git"
             })

    assert ssh_entry.id == "FA4.local"
    assert ssh_entry.description == "User-provided Git repository"

    assert {:ok, local_entry} =
             ReferenceCatalog.new_user_entry(%{url: "/srv/git/private-kernels.git"})

    assert local_entry.id == "private-kernels"
  end

  test "rejects unsafe paths, unsupported locations, and duplicate projects" do
    existing = ReferenceCatalog.entries()

    assert {:error, {:invalid_reference_project, :id, :invalid_format}} =
             ReferenceCatalog.new_user_entry(
               %{id: "../escape", url: "https://example.com/repo.git"},
               existing
             )

    assert {:error, {:invalid_reference_project, :url, :invalid_format}} =
             ReferenceCatalog.new_user_entry(%{url: "--upload-pack=evil"}, existing)

    assert {:error, {:invalid_reference_project, :url, :invalid_format}} =
             ReferenceCatalog.new_user_entry(%{url: "ftp://example.com/repo.git"}, existing)

    assert {:error, {:invalid_reference_project, :url, :invalid_format}} =
             ReferenceCatalog.new_user_entry(
               %{url: "https://token@example.com/private.git"},
               existing
             )

    assert {:error, {:invalid_reference_project, :repository, :invalid_format}} =
             ReferenceCatalog.new_user_entry("https://example.com/repo.git", existing)

    assert {:error, {:duplicate_reference_project, :id, "CUTLASS"}} =
             ReferenceCatalog.new_user_entry(
               %{id: "CUTLASS", url: "https://example.com/other.git"},
               existing
             )

    assert {:error, {:duplicate_reference_project, :url, _url}} =
             ReferenceCatalog.new_user_entry(
               %{id: "cutlass-copy", url: "https://github.com/NVIDIA/cutlass.git/"},
               existing
             )
  end

  test "resolves full default-branch SHAs and reports every selected failure" do
    source = AlignmentFixtures.git_repo()
    sha = Git.run!(source, ["rev-parse", "HEAD"])

    entries = [
      entry("local", source, true),
      entry("broken", Path.join(source, "missing"), true),
      entry("ignored", Path.join(source, "also-missing"), false)
    ]

    assert {:error, [local, broken, ignored]} = ReferenceCatalog.resolve_selected(entries)
    assert local.status == :resolved
    assert local.sha == sha
    assert is_binary(local.branch)
    assert broken.status == :error
    assert ignored.status == :unresolved
  end

  test "does not refresh a Reference Project that is already frozen" do
    sha = String.duplicate("a", 40)

    frozen = %{
      entry("frozen", "/repository/that/does/not/exist", true)
      | status: :resolved,
        sha: sha,
        branch: "main"
    }

    assert {:ok, [unchanged]} = ReferenceCatalog.resolve_selected([frozen])
    assert unchanged == frozen
  end

  test "materializes one Workspace checkout and links it into a worktree without Git state" do
    source = AlignmentFixtures.git_repo()
    setup_source = AlignmentFixtures.git_repo()
    gitmodules = "[submodule \"user-owned\"]\n\tpath = vendor/user-owned\n\turl = ../user-owned\n"
    File.write!(Path.join(setup_source, ".gitmodules"), gitmodules)
    Git.run!(setup_source, ["add", ".gitmodules"])
    Git.run!(setup_source, ["commit", "-m", "Keep user submodule metadata"])
    root = AlignmentFixtures.temp_dir("pika-reference-root")
    setup = Path.join(root, "setup")
    Git.run!(setup_source, ["worktree", "add", setup])
    sha = Git.run!(source, ["rev-parse", "HEAD"])
    branch = Git.run!(source, ["branch", "--show-current"])
    frozen = %{entry("local", source, true) | status: :resolved, branch: branch, sha: sha}

    assert {:ok, [materialized]} =
             ReferenceCatalog.materialize_selected(root, [frozen], link_into: setup)

    assert materialized.sha == sha
    assert Git.run!(Path.join(root, "refs/local"), ["rev-parse", "HEAD"]) == sha
    assert Git.run!(Path.join(setup, "ref/local"), ["rev-parse", "HEAD"]) == sha
    assert File.lstat!(Path.join(setup, "ref/local")).type == :symlink
    assert File.read_link!(Path.join(setup, "ref/local")) == Path.join(root, "refs/local")
    assert Git.run!(setup, ["ls-files", "--stage", "--", "ref/local"]) == ""
    assert File.read!(Path.join(setup, ".gitmodules")) == gitmodules
    assert Git.clean?(setup)

    exclude = Git.run!(setup, ["rev-parse", "--git-path", "info/exclude"])
    assert File.read!(Path.expand(exclude, setup)) =~ "/ref/"
  end

  test "clones selected References concurrently and reports their progress" do
    first_source = AlignmentFixtures.git_repo()
    second_source = AlignmentFixtures.git_repo()
    setup_source = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-reference-progress")
    setup = Path.join(root, "setup")
    Git.run!(setup_source, ["worktree", "add", setup])
    test_pid = self()

    entries = [
      frozen_entry("first", first_source, true),
      entry("ignored", Path.join(first_source, "missing"), false),
      frozen_entry("second", second_source, true)
    ]

    materialization =
      Task.async(fn ->
        ReferenceCatalog.materialize_selected(root, entries,
          link_into: setup,
          on_progress: fn
            %{status: :materializing} = progress ->
              send(test_pid, {:started, self(), progress})

              receive do
                :continue -> :ok
              end

            progress ->
              send(test_pid, {:progress, progress})
          end
        )
      end)

    assert_receive {:started, first_worker, %{id: first_id, completed: 0, total: 2}}
    assert_receive {:started, second_worker, %{id: second_id, completed: 0, total: 2}}
    assert first_worker != second_worker
    assert MapSet.new([first_id, second_id]) == MapSet.new(["first", "second"])

    send(first_worker, :continue)
    send(second_worker, :continue)

    assert {:ok, materialized} = Task.await(materialization, 10_000)
    assert Enum.map(materialized, & &1.id) == ["first", "ignored", "second"]

    assert_receive {:progress, first_progress}
    assert_receive {:progress, second_progress}

    assert Enum.sort([first_progress.completed, second_progress.completed]) == [1, 2]
    assert Enum.all?([first_progress, second_progress], &(&1.status == :resolved))
    assert MapSet.new([first_progress.id, second_progress.id]) == MapSet.new(["first", "second"])
    refute_receive {:progress, %{id: "ignored"}}
    assert File.lstat!(Path.join(setup, "ref/first")).type == :symlink
    assert File.lstat!(Path.join(setup, "ref/second")).type == :symlink
    refute File.exists?(Path.join(root, "refs/ignored"))
  end

  test "reuses one Workspace checkout across worktrees and removes stale managed links" do
    source = AlignmentFixtures.git_repo()
    repo = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-reference-shared")
    first = Path.join(root, "attempts/first")
    second = Path.join(root, "attempts/second")
    File.mkdir_p!(Path.dirname(first))
    Git.run!(repo, ["worktree", "add", "-b", "first", first])
    Git.run!(repo, ["worktree", "add", "-b", "second", second])
    frozen = frozen_entry("shared", source, true)

    assert {:ok, [_]} =
             ReferenceCatalog.materialize_selected(root, [frozen], link_into: first)

    checkout = Path.join(root, "refs/shared")
    checkout_inode = File.stat!(checkout).inode

    assert {:ok, [_]} =
             ReferenceCatalog.materialize_selected(root, [frozen], link_into: second)

    assert File.stat!(checkout).inode == checkout_inode
    assert File.read_link!(Path.join(first, "ref/shared")) == checkout
    assert File.read_link!(Path.join(second, "ref/shared")) == checkout

    assert :ok =
             ReferenceCatalog.link_selected(root, first, [%{frozen | selected: false}])

    refute File.exists?(Path.join(first, "ref/shared"))
    assert File.exists?(Path.join(second, "ref/shared"))
    assert File.dir?(checkout)
  end

  test "never replaces user-owned content that conflicts with an injected link" do
    source = AlignmentFixtures.git_repo()
    repo = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-reference-conflict")
    worktree = Path.join(root, "setup")
    Git.run!(repo, ["worktree", "add", worktree])
    conflict = Path.join(worktree, "ref/shared")
    File.mkdir_p!(conflict)
    marker = Path.join(conflict, "keep.txt")
    File.write!(marker, "user owned\n")
    frozen = frozen_entry("shared", source, true)

    assert {:error, [failed]} =
             ReferenceCatalog.materialize_selected(root, [frozen], link_into: worktree)

    assert failed.status == :error
    assert failed.description =~ "reference_link_conflict"
    assert File.read!(marker) == "user owned\n"
    assert File.lstat!(conflict).type == :directory
    assert File.dir?(Path.join(root, "refs/shared"))
  end

  test "detects mutation of a shared Reference Checkout instead of silently reusing it" do
    source = AlignmentFixtures.git_repo()
    root = AlignmentFixtures.temp_dir("pika-reference-dirty")
    frozen = frozen_entry("shared", source, true)

    assert {:ok, [_]} = ReferenceCatalog.materialize_selected(root, [frozen])
    checkout = Path.join(root, "refs/shared")
    tracked = Path.join(checkout, "README.md")
    original = File.read!(tracked)
    File.write!(tracked, original <> "mutated\n")

    assert {:error, [failed]} = ReferenceCatalog.materialize_selected(root, [frozen])
    assert failed.status == :error
    assert failed.description =~ "reference_checkout_dirty"
    assert File.read!(tracked) == original <> "mutated\n"
  end

  defp entry(id, url, selected) do
    %{
      id: id,
      url: url,
      description: id,
      selected: selected,
      status: :unresolved,
      sha: nil,
      branch: nil
    }
  end

  defp frozen_entry(id, source, selected) do
    %{
      entry(id, source, selected)
      | status: :resolved,
        sha: Git.run!(source, ["rev-parse", "HEAD"]),
        branch: Git.run!(source, ["branch", "--show-current"])
    }
  end
end
