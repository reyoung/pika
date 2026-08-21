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

    File.write!(Path.join(root, "kernel/bench.py"), "changed\n")
    assert {:error, :protected_digest_changed} = Harness.verify_digest(root, harness)

    File.write!(Path.join(root, "kernel/bench.py"), "print('bench')\n")
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
               oracle_path: "link.py",
               correctness_paths: ["../outside.py"],
               benchmark_path: "link.py",
               protected_paths: []
             })
  end

  test "streams a hash-verified bounded Reference source preview" do
    root = AlignmentFixtures.temp_dir("pika-reference-review")
    attrs = AlignmentFixtures.create_harness(root)
    content = "def reference(x):\n    return x\n" <> String.duplicate("# detail\n", 32)
    File.write!(Path.join(root, "kernel/reference.py"), content)
    assert {:ok, _harness} = Harness.validate(root, attrs)

    expected_sha = :crypto.hash(:sha256, content) |> Base.encode16(case: :lower)

    assert {:ok, review} =
             Harness.source_review(root, "kernel/reference.py",
               preview_bytes: 48,
               expected_sha: expected_sha
             )

    assert review.path == "kernel/reference.py"
    assert review.content == binary_part(content, 0, 48)
    assert review.preview_bytes == 48
    assert review.size == byte_size(content)
    assert review.truncated

    assert review.sha256 == expected_sha

    File.write!(Path.join(root, "kernel/reference.py"), "changed\n")

    assert {:error, :source_sha_changed} =
             Harness.source_review(root, "kernel/reference.py", expected_sha: expected_sha)
  end

  test "keeps a valid UTF-8 preview when its byte limit splits a character" do
    root = AlignmentFixtures.temp_dir("pika-reference-review-utf8")
    attrs = AlignmentFixtures.create_harness(root)
    content = String.duplicate("a", 47) <> "界\n"
    File.write!(Path.join(root, "kernel/reference.py"), content)
    assert {:ok, _harness} = Harness.validate(root, attrs)

    assert {:ok, review} =
             Harness.source_review(root, "kernel/reference.py", preview_bytes: 48)

    assert review.content == String.duplicate("a", 47)
    assert review.truncated
  end

  test "rejects a Reference reached through a symlinked directory" do
    root = AlignmentFixtures.temp_dir("pika-reference-review-symlink")
    _attrs = AlignmentFixtures.create_harness(root)
    external = AlignmentFixtures.temp_dir("pika-reference-review-external")
    File.write!(Path.join(external, "reference.py"), "secret\n")
    File.rm_rf!(Path.join(root, "kernel"))
    File.ln_s!(external, Path.join(root, "kernel"))

    assert {:error, {:invalid_harness_file, "kernel/reference.py"}} =
             Harness.source_review(root, "kernel/reference.py")
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
    assert {:error, :outside_artifact_root} = ArtifactStore.register(root, "setup/1/a.txt")

    assert {:error, :sha256_mismatch} =
             ArtifactStore.register(root, "artifacts/a.txt", %{sha256: "bad"})

    assert {:ok, %{size: 5, relative_path: "artifacts/a.txt"}} =
             ArtifactStore.register(root, "artifacts/a.txt")
  end
end
