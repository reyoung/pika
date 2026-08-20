defmodule Pika.Alignment.FakeE2ETest do
  use ExUnit.Case, async: false

  alias Pika.Alignment.{Campaign, Workspace}
  alias Pika.Test.AlignmentFixtures

  test "a unified fake Backend drives Alignment through Baseline" do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    repo = AlignmentFixtures.git_repo()
    {:ok, workspace} = Workspace.prepare(repo)
    skill_dir = Path.join(workspace.root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)

    File.write!(
      Path.join(skill_dir, "SKILL.md"),
      "---\nname: ncu-report-skill\ndescription: test\n---\n"
    )

    {:ok, pid} =
      Campaign.start(
        workspace: workspace,
        backend: :codex_app_server,
        backend_module: Pika.Test.AlignmentAgentBackend,
        start_backend: true,
        resolve_references: false,
        mcp_url: "http://127.0.0.1:1/mcp",
        skill: %{
          name: "ncu-report-skill",
          url: "test",
          sha: String.duplicate("f", 40),
          path: skill_dir
        }
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)

    assert eventually(fn -> not is_nil(Campaign.snapshot().backend_session_id) end)
    assert Campaign.snapshot().status == :drafting_spec
    assert Campaign.snapshot().active_turn_id == nil
    refute Enum.any?(Campaign.snapshot().messages, &(&1.role == :agent))

    reference_id = Campaign.snapshot().references |> List.first() |> Map.fetch!(:id)
    assert :ok = Campaign.toggle_reference(reference_id)
    assert :ok = Campaign.toggle_reference(reference_id)
    Process.sleep(20)
    assert Campaign.snapshot().active_turn_id == nil
    refute Enum.any?(Campaign.snapshot().messages, &(&1.role == :agent))

    assert :ok =
             Campaign.send_message("请帮我定义并优化这个 Kernel；先和我对齐计算边界、Shapes 与 Metrics。")

    assert eventually(fn -> Campaign.snapshot().status == :awaiting_confirmation end)
    assert {:ok, reference_review} = Campaign.reference_review()
    evidence_digest = Campaign.snapshot().reference_review_evidence.digest
    assert :ok = Campaign.confirm_spec(reference_review.sha256, evidence_digest)

    assert Enum.any?(Campaign.snapshot().messages, fn message ->
             message.role == :user and message.content == "确认 Campaign Spec v1，并建立 Baseline。"
           end)

    assert eventually(fn -> Campaign.snapshot().status == :optimizing end, 250)
    snapshot = Campaign.snapshot()
    assert snapshot.baseline.summary == "fake end-to-end baseline"
    assert snapshot.required_operations == []
    assert Workspace.verify_source_unchanged(workspace) == :ok
  end

  defp eventually(fun, attempts \\ 100)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.(),
      do: true,
      else:
        (
          Process.sleep(20)
          eventually(fun, attempts - 1)
        )
  end

  defp eventually(_fun, 0), do: false
end
