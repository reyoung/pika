defmodule Pika.Stage0.HarnessArtifactTest do
  use ExUnit.Case, async: true

  alias Pika.Stage0.{ArtifactStore, Harness}
  alias Pika.Test.Stage0Fixtures

  test "computes a stable protected digest and detects content changes" do
    root = Stage0Fixtures.temp_dir("pika-harness")
    attrs = Stage0Fixtures.create_harness(root)
    assert {:ok, harness} = Harness.validate(root, attrs)
    assert :ok = Harness.verify_digest(root, harness)

    File.write!(Path.join(root, "kernel/reference.py"), "changed\n")
    assert {:error, :protected_digest_changed} = Harness.verify_digest(root, harness)
  end

  test "rejects path escape and symlinks" do
    root = Stage0Fixtures.temp_dir("pika-harness-safe")
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

  test "Artifact Store rejects absolute paths, escape and caller hash mismatch" do
    root = Stage0Fixtures.temp_dir("pika-artifacts")
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
