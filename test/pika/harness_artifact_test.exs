defmodule Pika.HarnessArtifactTest do
  use ExUnit.Case, async: true

  alias Pika.{Git, Harness}
  alias Pika.Alignment.ArtifactStore
  alias Pika.Test.AlignmentFixtures

  test "protected digest detects additions, deletions, renames, and content changes" do
    root = AlignmentFixtures.temp_dir("pika-harness")
    attrs = AlignmentFixtures.create_harness(root)
    assert {:ok, harness} = Harness.validate(root, attrs)
    assert :ok = Harness.verify_digest(root, harness)

    File.write!(Path.join(root, "kernel/reference.py"), "changed\n")
    assert {:error, :protected_digest_changed} = Harness.verify_digest(root, harness)

    File.write!(Path.join(root, "kernel/reference.py"), "def reference(x): return x\n")
    File.write!(Path.join(root, "kernel/new_guard.py"), "assert True\n")

    assert {:ok, expanded} =
             Harness.validate(root, %{
               attrs
               | "protected_paths" => attrs["protected_paths"] ++ ["kernel/new_guard.py"]
             })

    refute expanded.digest == harness.digest

    File.rm!(Path.join(root, "kernel/test_correctness.py"))
    assert {:error, :protected_digest_changed} = Harness.verify_digest(root, harness)

    File.rename!(
      Path.join(root, "kernel/bench.py"),
      Path.join(root, "kernel/benchmark_renamed.py")
    )

    assert {:error, :protected_digest_changed} = Harness.verify_digest(root, harness)
  end

  test "rejects path escape and symlinks" do
    root = AlignmentFixtures.temp_dir("pika-harness-safe")
    outside = Path.join(System.tmp_dir!(), "outside-#{System.unique_integer([:positive])}")
    File.write!(outside, "secret")
    File.ln_s!(outside, Path.join(root, "link.py"))

    assert {:error, _} =
             Harness.validate(root, %{
               reference_path: "link.py",
               correctness_paths: ["../outside.py"],
               benchmark_path: "link.py",
               protected_paths: []
             })
  end

  test "rejects a candidate commit that changes a protected Harness path" do
    repo = AlignmentFixtures.git_repo()
    attrs = AlignmentFixtures.create_harness(repo)
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "protected harness"])
    base_sha = Git.run!(repo, ["rev-parse", "HEAD"])
    assert {:ok, harness} = Harness.validate(repo, attrs)

    File.write!(Path.join(repo, "kernel/bench.py"), "print('changed')\n")
    Git.run!(repo, ["add", "kernel/bench.py"])
    Git.run!(repo, ["commit", "-m", "candidate changes harness"])
    candidate_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    assert {:error, {:protected_paths_changed, ["kernel/bench.py"]}} =
             Harness.verify_candidate(repo, base_sha, candidate_sha, harness)
  end

  test "Artifact Store rejects absolute paths, escape and caller hash mismatch" do
    root = AlignmentFixtures.temp_dir("pika-artifacts")
    File.mkdir_p!(Path.join(root, "artifacts"))
    File.write!(Path.join(root, "artifacts/a.txt"), "hello")

    assert {:error, :absolute_path_forbidden} = ArtifactStore.register(root, "/tmp/a")
    assert {:error, :path_escape} = ArtifactStore.register(root, "../a")

    assert {:error, :sha256_mismatch} =
             ArtifactStore.register(root, "artifacts/a.txt", %{sha256: "bad"})

    assert {:ok, %{size: 5, relative_path: "artifacts/a.txt"}} =
             ArtifactStore.register(root, "artifacts/a.txt")
  end
end
