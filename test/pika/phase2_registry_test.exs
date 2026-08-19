defmodule Pika.Phase2RegistryTest do
  use ExUnit.Case, async: true

  alias Pika.Phase2.Registry
  alias Pika.Stage0.Git
  alias Pika.Test.Stage0Fixtures

  test "restores the frozen Skill SHA and refuses a changed checkout" do
    root = Stage0Fixtures.temp_dir("pika-phase2-skill")
    path = Path.join(root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(path)
    Git.run!(path, ["init"])
    Git.run!(path, ["config", "user.name", "Pika Test"])
    Git.run!(path, ["config", "user.email", "pika@example.invalid"])
    File.write!(Path.join(path, "SKILL.md"), "---\nname: ncu-report-skill\n---\n")
    Git.run!(path, ["add", "SKILL.md"])
    Git.run!(path, ["commit", "-m", "skill"])
    sha = Git.run!(path, ["rev-parse", "HEAD"])

    durable = %{
      skill: %{
        name: "ncu-report-skill",
        url: "https://example.invalid/ncu-report-skill.git",
        branch: "main",
        sha: sha
      }
    }

    assert {:ok, restored} = Registry.skill(root, durable)
    assert restored.sha == sha
    assert restored.branch == "main"
    assert restored.path == path

    File.write!(Path.join(path, "SKILL.md"), "changed\n")
    Git.run!(path, ["add", "SKILL.md"])
    Git.run!(path, ["commit", "-m", "changed"])

    assert {:error, {:frozen_skill_missing_or_changed, ^sha}} = Registry.skill(root, durable)
  end
end
