defmodule Pika.Stage0.ReferenceCatalogTest do
  use ExUnit.Case, async: true

  alias Pika.Stage0.{Git, ReferenceCatalog}
  alias Pika.Test.Stage0Fixtures

  test "contains the 16 default-selected Atrex references" do
    entries = ReferenceCatalog.entries()
    assert length(entries) == 16
    assert Enum.all?(entries, & &1.selected)
    assert Enum.uniq(Enum.map(entries, & &1.id)) == Enum.map(entries, & &1.id)
  end

  test "resolves full default-branch SHAs and reports every selected failure" do
    source = Stage0Fixtures.git_repo()
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
    source = Stage0Fixtures.git_repo()
    setup_source = Stage0Fixtures.git_repo()
    setup = Path.join(Stage0Fixtures.temp_dir("pika-reference-root"), "setup")
    Git.run!(setup_source, ["worktree", "add", setup])
    sha = Git.run!(source, ["rev-parse", "HEAD"])
    branch = Git.run!(source, ["branch", "--show-current"])
    frozen = %{entry("local", source, true) | status: :resolved, branch: branch, sha: sha}

    assert {:ok, [materialized]} = ReferenceCatalog.materialize_selected(setup, [frozen])
    assert materialized.sha == sha
    assert Git.run!(Path.join(setup, "ref/local"), ["rev-parse", "HEAD"]) == sha
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
end
