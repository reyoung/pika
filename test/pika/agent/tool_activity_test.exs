defmodule Pika.Agent.ToolActivityTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.ToolActivity
  alias Pika.AgentBackend.Event

  test "normalizes command lifecycle events into one durable activity" do
    item = %{
      "id" => "command-42",
      "type" => "commandExecution",
      "command" => "sed -n '1,120p' lib/pika/agent/actor.ex",
      "status" => "inProgress"
    }

    started =
      Event.new(:tool_started, :codex_app_server, "session-1", %{
        turn_id: "turn-1",
        data: %{item: item, stage: :started}
      })

    assert {:ok, running} = ToolActivity.from_event(started)
    assert running["id"] == "command-42"
    assert running["kind"] == "command"
    assert running["name"] == "Read files"
    assert running["status"] == "running"
    assert running["summary"] == item["command"]
    assert running["command_ref"] =~ ~r/^[0-9a-f]{64}$/

    completed = %{
      started
      | type: :tool_completed,
        data: %{item: %{item | "status" => "completed"}, stage: :completed}
    }

    assert {:ok, finished} = ToolActivity.from_event(completed)
    assert finished["id"] == running["id"]
    assert finished["command_ref"] == running["command_ref"]
    assert finished["status"] == "completed"
  end

  test "ignores messages and Pika MCP calls already recorded by the domain layer" do
    message = Event.new(:message_completed, :codex_app_server, "session-1")
    assert :ignore = ToolActivity.from_event(message)

    pika_mcp =
      Event.new(:tool_completed, :codex_app_server, "session-1", %{
        data: %{
          item: %{
            "id" => "mcp-1",
            "type" => "mcpToolCall",
            "server" => "pika",
            "tool" => "ask_questions",
            "status" => "completed"
          }
        }
      })

    assert :ignore = ToolActivity.from_event(pika_mcp)
  end
end
