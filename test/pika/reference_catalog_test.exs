defmodule Pika.ReferenceCatalogTest do
  use ExUnit.Case, async: true

  alias Pika.{Git, ReferenceCatalog}
  alias Pika.Test.AlignmentFixtures

  test "contains the 16 default-selected Atrex references" do
    entries = ReferenceCatalog.entries()
    assert length(entries) == 16
    assert Enum.all?(entries, & &1.selected)
    assert Enum.uniq(Enum.map(entries, & &1.id)) == Enum.map(entries, & &1.id)
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

  test "materializes the already-frozen SHA without refreshing it" do
    source = AlignmentFixtures.git_repo()
    setup_source = AlignmentFixtures.git_repo()
    setup = Path.join(AlignmentFixtures.temp_dir("pika-reference-root"), "setup")
    Git.run!(setup_source, ["worktree", "add", setup])
    sha = Git.run!(source, ["rev-parse", "HEAD"])
    branch = Git.run!(source, ["branch", "--show-current"])
    frozen = %{entry("local", source, true) | status: :resolved, branch: branch, sha: sha}

    assert {:ok, [materialized]} = ReferenceCatalog.materialize_selected(setup, [frozen])
    assert materialized.sha == sha
    assert Git.run!(Path.join(setup, "ref/local"), ["rev-parse", "HEAD"]) == sha
  end

  test "reports materialization progress for each selected Reference" do
    first_source = AlignmentFixtures.git_repo()
    second_source = AlignmentFixtures.git_repo()
    setup_source = AlignmentFixtures.git_repo()
    setup = Path.join(AlignmentFixtures.temp_dir("pika-reference-progress"), "setup")
    Git.run!(setup_source, ["worktree", "add", setup])
    test_pid = self()

    entries = [
      frozen_entry("first", first_source, true),
      entry("ignored", Path.join(first_source, "missing"), false),
      frozen_entry("second", second_source, true)
    ]

    assert {:ok, _materialized} =
             ReferenceCatalog.materialize_selected(setup, entries,
               on_progress: &send(test_pid, {:progress, &1})
             )

    assert_receive {:progress, %{id: "first", completed: 0, total: 2, status: :materializing}}

    assert_receive {:progress, %{id: "first", completed: 1, total: 2, status: :resolved}}

    assert_receive {:progress, %{id: "second", completed: 1, total: 2, status: :materializing}}

    assert_receive {:progress, %{id: "second", completed: 2, total: 2, status: :resolved}}
    refute_receive {:progress, %{id: "ignored"}}
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
