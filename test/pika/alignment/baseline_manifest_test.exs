defmodule Pika.Alignment.BaselineManifestTest do
  use ExUnit.Case, async: true

  alias Pika.Alignment.BaselineManifest
  alias Pika.Test.AlignmentFixtures

  test "loads one local manifest without registering every referenced file first" do
    root = AlignmentFixtures.temp_dir("pika-baseline-manifest")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("a", 40)

    AlignmentFixtures.write_baseline_artifacts(workspace, sha, String.duplicate("b", 40))
    relative_path = AlignmentFixtures.write_baseline_manifest(workspace, sha)

    assert {:ok, manifest} = BaselineManifest.load(root, relative_path)
    assert manifest.schema_version == 2
    assert manifest.target_snapshot_id == "target-fixture"
    assert manifest.candidate_sha == sha
    assert manifest.samples_artifact == "artifacts/baseline/samples.jsonl"
    assert manifest.manifest_artifact.relative_path == relative_path
    assert manifest.manifest_artifact.size > 0
  end

  test "accepts a Baseline manifest without optional profiler evidence" do
    root = AlignmentFixtures.temp_dir("pika-baseline-manifest-no-profiler")
    workspace = %{root: root, artifacts: Path.join(root, "artifacts")}
    sha = String.duplicate("d", 40)

    AlignmentFixtures.write_baseline_artifacts(workspace, sha, String.duplicate("e", 40))

    relative_path =
      AlignmentFixtures.write_baseline_manifest(workspace, sha, "no profiler", profiler: false)

    assert {:ok, manifest} = BaselineManifest.load(root, relative_path)
    assert manifest.profiler_artifact == nil
    assert manifest.profiler_dependencies == []
  end

  test "rejects references outside the Artifact Workspace" do
    root = AlignmentFixtures.temp_dir("pika-baseline-manifest-escape")
    manifest_path = Path.join(root, "artifacts/baseline/manifest.json")
    File.mkdir_p!(Path.dirname(manifest_path))

    File.write!(
      manifest_path,
      Jason.encode!(%{
        "schema_version" => 2,
        "target_snapshot_id" => "target-fixture",
        "candidate_sha" => String.duplicate("c", 40),
        "summary" => "escape",
        "samples_artifact" => "../samples.jsonl",
        "correctness_artifact" => "artifacts/baseline/correctness.json"
      })
    )

    assert {:error, :invalid_baseline_artifact_path} =
             BaselineManifest.load(root, "artifacts/baseline/manifest.json")
  end
end
