defmodule Pika.Agent.RecoveryContextTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, RecoveryContext}
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  @initial_sha String.duplicate("a", 40)

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-v2-recovery-context-#{System.unique_integer([:positive])}"
      )

    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    File.mkdir_p!(workspace)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(repo, workspace))
    assert {:ok, config} = Config.load(config_path)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()

    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, @initial_sha)

    on_exit(fn -> File.rm_rf!(root) end)
    %{workspace: workspace}
  end

  test "journals normalized Turns across Sessions and rebuilds repeated recovery directories", %{
    workspace: workspace
  } do
    assert {:ok, first} =
             ConversationJournal.start_session(
               "iteration",
               :attempt,
               "1",
               %{"backend" => "codex"},
               "system one",
               "context one"
             )

    assert {:ok, first_turn} =
             ConversationJournal.start_turn(first.id, [
               %{"role" => "user", "content" => "optimize"}
             ])

    assert {:ok, _turn} =
             ConversationJournal.append_output(first_turn.id, %{
               "role" => "assistant",
               "content" => "working"
             })

    assert {:ok, _turn} =
             ConversationJournal.append_mcp_call(first_turn.id, %{
               "name" => "finish_iteration",
               "status" => "error",
               "summary" => "missing benchmark"
             })

    assert {:ok, _turn} =
             ConversationJournal.finish_turn(first_turn.id, "completed_without_terminal_mcp")

    assert {:ok, _session} = ConversationJournal.interrupt_session(first.id, "backend exited")

    assert {:ok, second} =
             ConversationJournal.start_session(
               "iteration",
               :attempt,
               "1",
               %{"backend" => "cursor"},
               "system two",
               "context two"
             )

    assert second.session_sequence == 2
    assert {:ok, second_turn} = ConversationJournal.start_turn(second.id, [])

    assert {:ok, _turn} =
             ConversationJournal.append_output(second_turn.id, %{
               "role" => "assistant",
               "content" => "resumed"
             })

    assert {:ok, _session} = ConversationJournal.interrupt_session(second.id, "host crash")

    state = %{
      status: "iterating",
      git_repo: Path.join(workspace, "attempts/000001/repo"),
      required_operations: ["finish_iteration"]
    }

    assert {:ok, recovery1} = RecoveryContext.build(workspace, second.id, state)
    assert recovery1.directory =~ "recovery-01"
    assert {:ok, recovery2} = RecoveryContext.build(workspace, second.id, state)
    assert recovery2.directory =~ "recovery-02"

    turns =
      recovery2.messages_file
      |> File.stream!()
      |> Enum.map(&Jason.decode!/1)

    assert length(turns) == 2
    assert Enum.map(turns, & &1["session_sequence"]) == [1, 2]
    assert Enum.at(turns, 0)["mcp_calls"] |> hd() |> Map.fetch!("name") == "finish_iteration"
    assert Enum.at(turns, 1)["ended_reason"] == "interrupted"

    recovery_state = recovery2.state_file |> File.read!() |> Jason.decode!()
    assert recovery_state["recovery_sequence"] == 2
    assert recovery_state["backend_session_id"] == second.id
    assert recovery_state["previous_ended_reason"] == "host crash"
    assert recovery_state["required_operations"] == ["finish_iteration"]
    assert ConversationJournal.session(second.id).recovery_sequence == 2
  end

  defp yaml(repo, workspace) do
    """
    version: 2
    repo: #{repo}
    workspace: #{workspace}
    agents:
      baseline_alignment:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      baseline_verify:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      iteration:
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
