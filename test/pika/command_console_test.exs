defmodule Pika.CommandConsoleTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend.Event
  alias Pika.CommandConsole

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-command-console-#{System.unique_integer([:positive])}")

    on_exit(fn -> File.rm_rf(root) end)
    %{root: root}
  end

  test "persists Codex deltas and completion metadata without duplicating aggregate output", %{
    root: root
  } do
    started =
      event(:tool_started, :codex_app_server, "session", "turn", %{
        item: %{"id" => "command-1", "type" => "commandExecution", "command" => "echo hello"}
      })

    delta =
      event(:command_output, :codex_app_server, "session", "turn", %{
        "itemId" => "command-1",
        "delta" => "hello\n"
      })

    completed =
      event(:tool_completed, :codex_app_server, "session", "turn", %{
        item: %{
          "id" => "command-1",
          "type" => "commandExecution",
          "command" => "echo hello",
          "status" => "completed",
          "exitCode" => 0,
          "durationMs" => 12,
          "aggregatedOutput" => "hello\n"
        }
      })

    assert {:ok, ref} = CommandConsole.capture(started, root, "/workspace")
    assert {:ok, ^ref} = CommandConsole.capture(delta, root, "/workspace")
    assert {:ok, ^ref} = CommandConsole.capture(completed, root, "/workspace")
    assert {:ok, console} = CommandConsole.load(ref, roots: [root])
    assert console.command == "echo hello"
    assert console.cwd == "/workspace"
    assert console.output == "hello\n"
    assert console.status == "completed"
    assert console.exit_code == 0
    assert console.duration_ms == 12
  end

  test "Cursor terminal snapshots replace previous output and redact secrets", %{root: root} do
    started =
      event(:tool_started, :cursor_acp, "session", "turn", %{
        "sessionUpdate" => "tool_call",
        "toolCallId" => "tool-1",
        "title" => "Run command"
      })

    first =
      event(:command_output, :cursor_acp, "session", "turn", %{
        "command_id" => "tool-1",
        "terminalId" => "terminal-1",
        "update_mode" => "replace",
        "output" => "TOKEN=super-secret\nstep 1"
      })

    second =
      event(:command_output, :cursor_acp, "session", "turn", %{
        "command_id" => "tool-1",
        "terminalId" => "terminal-1",
        "update_mode" => "replace",
        "output" => "TOKEN=super-secret\nstep 2"
      })

    assert {:ok, ref} = CommandConsole.capture(started, root, "/workspace")
    assert {:ok, ^ref} = CommandConsole.capture(first, root, "/workspace")
    assert {:ok, ^ref} = CommandConsole.capture(second, root, "/workspace")
    assert {:ok, console} = CommandConsole.load(ref, roots: [root])
    assert console.output == "TOKEN=[REDACTED]\nstep 2"
  end

  test "rejects refs that could escape the artifact directory" do
    assert {:error, :invalid_ref} = CommandConsole.load("../../secret", roots: ["/tmp"])
  end

  defp event(type, backend, session_id, turn_id, data) do
    Event.new(type, backend, session_id, %{turn_id: turn_id, data: data})
  end
end
