defmodule Pika.AttemptWorkspaceTest do
  use ExUnit.Case, async: true

  alias Pika.{AttemptWorkspace, Git}
  alias Pika.Test.AlignmentFixtures

  test "candidate Patch excludes forced Reference symlinks but keeps user .gitmodules changes" do
    repo = AlignmentFixtures.git_repo()
    base_sha = Git.run!(repo, ["rev-parse", "HEAD"])
    File.write!(Path.join(repo, ".gitmodules"), "# user-owned change\n")
    File.mkdir_p!(Path.join(repo, "ref"))
    File.ln_s!(System.tmp_dir!(), Path.join(repo, "ref/forced-reference"))
    Git.run!(repo, ["add", ".gitmodules"])
    Git.run!(repo, ["add", "-f", "ref/forced-reference"])
    Git.run!(repo, ["commit", "-m", "Candidate with forced Reference link"])
    candidate_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    assert {:ok, patch} =
             AttemptWorkspace.patch(
               %{repo: repo},
               %{base_sha: base_sha},
               candidate_sha
             )

    assert patch =~ ".gitmodules"
    assert patch =~ "user-owned change"
    refute patch =~ "ref/forced-reference"
    refute patch =~ System.tmp_dir!()
  end
end
