defmodule Pika.Stage0.FakeE2ETest do
  use ExUnit.Case, async: false

  alias Pika.Stage0.{Campaign, Workspace}
  alias Pika.Test.Stage0Fixtures

  test "a unified fake Backend drives Alignment through Baseline" do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    repo = Stage0Fixtures.git_repo()
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
        backend_module: Pika.Test.Stage0AgentBackend,
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

    assert eventually(fn -> Campaign.snapshot().status == :awaiting_confirmation end)
    assert :ok = Campaign.confirm_spec()
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
