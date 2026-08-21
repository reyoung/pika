defmodule Pika.TargetSnapshotTest do
  use ExUnit.Case, async: true

  alias Pika.{Git, TargetSnapshot}
  alias Pika.Test.AlignmentFixtures

  test "keeps a frozen Target while Development advances and replaces it only for an explicit Target change" do
    root = AlignmentFixtures.temp_dir("pika-target-snapshot")
    setup = Path.join(root, "setup-repo")
    initialize_repo(setup)

    write_implementation(setup, "kernel/reference.py", "def target(x): return x\n")
    write_implementation(setup, "kernel/development.py", "def candidate(x): return x\n")
    first_development_sha = commit(setup, "initial implementations")
    spec = %{AlignmentFixtures.spec() | "reference_ids" => []}

    assert {:ok, first} =
             TargetSnapshot.prepare(root, 1, setup, first_development_sha, spec, [])

    assert :ok = TargetSnapshot.link(root, setup, first)
    assert :ok = TargetSnapshot.verify(root, first)
    assert File.read!(Path.join(setup, "target/kernel/reference.py")) =~ "target"
    assert Git.clean?(setup)

    write_implementation(setup, "kernel/development.py", "def candidate(x): return x + 1\n")
    second_development_sha = commit(setup, "advance Development")

    assert {:ok, reused} =
             TargetSnapshot.prepare(root, 2, setup, second_development_sha, spec, [],
               inherited_snapshot: first
             )

    assert reused.id == first.id
    assert reused.revision == 1
    assert reused.source_sha == first_development_sha
    assert reused.development_sha == second_development_sha

    assert File.read!(
             Path.join(TargetSnapshot.checkout_path(root, reused), "kernel/reference.py")
           ) ==
             "def target(x): return x\n"

    write_implementation(setup, "kernel/fa4_reference.py", "def fa4_target(x): return x\n")
    third_development_sha = commit(setup, "define a different Target")

    changed_spec =
      put_in(
        spec,
        ["implementations", "optimization_target", "entrypoint"],
        "kernel/fa4_reference.py"
      )

    assert {:ok, changed} =
             TargetSnapshot.prepare(root, 2, setup, third_development_sha, changed_spec, [],
               inherited_snapshot: reused
             )

    refute changed.id == first.id
    assert changed.revision == 2
    assert changed.source_sha == third_development_sha

    assert File.read!(Path.join(TargetSnapshot.checkout_path(root, changed), changed.entrypoint)) ==
             "def fa4_target(x): return x\n"
  end

  test "freezes an external Reference Project independently from Development" do
    root = AlignmentFixtures.temp_dir("pika-reference-target")
    setup = Path.join(root, "setup-repo")
    reference = Path.join([root, "refs", "external-target"])
    initialize_repo(setup)
    initialize_repo(reference)

    write_implementation(setup, "kernel/development.py", "def candidate(x): return x\n")
    development_sha = commit(setup, "Development")
    write_implementation(reference, "kernels/target.py", "def target(x): return x\n")
    reference_sha = commit(reference, "external Target")

    spec =
      AlignmentFixtures.spec()
      |> Map.put("reference_ids", ["external-target"])
      |> put_in(
        ["implementations", "optimization_target"],
        %{
          "source" => %{
            "kind" => "reference_project",
            "reference_id" => "external-target"
          },
          "entrypoint" => "kernels/target.py"
        }
      )

    references = [
      %{id: "external-target", status: :resolved, sha: reference_sha}
    ]

    assert {:ok, snapshot} =
             TargetSnapshot.prepare(root, 1, setup, development_sha, spec, references)

    assert snapshot.source_kind == "reference_project"
    assert snapshot.source_reference_id == "external-target"
    assert snapshot.source_sha == reference_sha
    assert snapshot.development_sha == development_sha
    assert :ok = TargetSnapshot.verify(root, snapshot)

    assert {:ok, same_content} =
             TargetSnapshot.prepare(root, 2, setup, development_sha, spec, references)

    assert same_content.id == snapshot.id
    assert same_content.digest == snapshot.digest
    assert same_content.revision == 2
  end

  defp initialize_repo(path) do
    File.mkdir_p!(path)
    Git.run!(path, ["init"])
    Git.run!(path, ["config", "user.name", "Pika Test"])
    Git.run!(path, ["config", "user.email", "pika@example.invalid"])
  end

  defp write_implementation(repo, relative_path, body) do
    path = Path.join(repo, relative_path)
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, body)
  end

  defp commit(repo, message) do
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", message])
    Git.run!(repo, ["rev-parse", "HEAD"])
  end
end
