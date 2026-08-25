defmodule Pika.Test.FakeJSONLProvider do
  def run do
    protocol = System.get_env("PIKA_FAKE_PROTOCOL") || "codex"

    IO.stream(:stdio, :line)
    |> Enum.reduce(%{protocol: protocol, active_turn: nil, prompt_id: nil}, &handle_line/2)
  end

  defp handle_line(line, state) do
    case Jason.decode(line) do
      {:ok, message} -> handle(message, state)
      _ -> state
    end
  end

  defp handle(message, %{protocol: "codex"} = state), do: handle_codex(message, state)
  defp handle(message, %{protocol: "cursor"} = state), do: handle_cursor(message, state)

  defp handle_codex(%{"id" => id, "method" => "initialize"}, state) do
    respond(id, %{"serverInfo" => %{"name" => "fake-codex", "version" => "1"}})
    state
  end

  defp handle_codex(%{"id" => id, "method" => method}, state)
       when method in ["skills/extraRoots/set", "skills/list"] do
    respond(id, %{"data" => []})
    state
  end

  defp handle_codex(%{"id" => id, "method" => "thread/start"}, state) do
    thread_id = "fake-thread"
    respond(id, %{"thread" => %{"id" => thread_id}})
    notify("thread/started", %{"thread" => %{"id" => thread_id}})
    state
  end

  defp handle_codex(%{"id" => id, "method" => "thread/resume", "params" => params}, state) do
    thread_id = params["threadId"]
    respond(id, %{"thread" => %{"id" => thread_id, "turns" => []}})
    notify("thread/started", %{"thread" => %{"id" => thread_id}})
    state
  end

  defp handle_codex(%{"id" => id, "method" => "turn/start", "params" => params}, state) do
    turn_id = "fake-turn-#{id}"
    respond(id, %{"turn" => %{"id" => turn_id, "status" => "inProgress", "items" => []}})
    notify("turn/started", %{"threadId" => params["threadId"], "turn" => %{"id" => turn_id}})

    if contains?(params["input"], "hold") do
      %{state | active_turn: turn_id}
    else
      complete_codex_turn(params["threadId"], turn_id, "fake complete")
      %{state | active_turn: nil}
    end
  end

  defp handle_codex(%{"id" => id, "method" => "turn/steer", "params" => params}, state) do
    respond(id, %{"turnId" => state.active_turn})
    complete_codex_turn(params["threadId"], state.active_turn, "steered")
    %{state | active_turn: nil}
  end

  defp handle_codex(%{"id" => id, "method" => "turn/interrupt", "params" => params}, state) do
    respond(id, %{})

    notify("turn/completed", %{
      "threadId" => params["threadId"],
      "turn" => %{"id" => params["turnId"], "status" => "interrupted", "items" => []}
    })

    %{state | active_turn: nil}
  end

  defp handle_codex(_message, state), do: state

  defp handle_cursor(%{"id" => id, "method" => "initialize"}, state) do
    close = if System.get_env("PIKA_FAKE_CURSOR_CLOSE") == "1", do: %{"close" => %{}}, else: %{}

    respond(id, %{
      "protocolVersion" => 1,
      "agentCapabilities" => %{
        "loadSession" => true,
        "mcpCapabilities" => %{"http" => true, "sse" => true},
        "sessionCapabilities" => close
      },
      "authMethods" => []
    })

    state
  end

  defp handle_cursor(%{"id" => id, "method" => "session/new"}, state) do
    respond(id, %{"sessionId" => "fake-cursor-session"})
    state
  end

  defp handle_cursor(%{"id" => id, "method" => "session/load"}, state) do
    respond(id, %{})
    state
  end

  defp handle_cursor(%{"id" => id, "method" => "session/set_config_option"}, state) do
    respond(id, %{})
    state
  end

  defp handle_cursor(%{"id" => id, "method" => "session/prompt", "params" => params}, state) do
    if contains?(params["prompt"], "raw-output") do
      notify_cursor(%{
        "sessionUpdate" => "tool_call",
        "toolCallId" => "fake-command",
        "kind" => "execute",
        "title" => "`echo hello`",
        "rawInput" => %{"command" => "echo hello"},
        "status" => "pending"
      })

      notify_cursor(%{
        "sessionUpdate" => "tool_call_update",
        "toolCallId" => "fake-command",
        "status" => "completed",
        "rawOutput" => %{"content" => "hello\n"}
      })
    end

    notify_cursor(%{
      "sessionUpdate" => "agent_message_chunk",
      "content" => %{"type" => "text", "text" => "fake"}
    })

    if contains?(params["prompt"], "hold") do
      %{state | prompt_id: id, active_turn: id}
    else
      respond(id, %{"stopReason" => "end_turn"})
      %{state | prompt_id: nil, active_turn: nil}
    end
  end

  defp handle_cursor(%{"method" => "session/cancel"}, %{prompt_id: prompt_id} = state)
       when not is_nil(prompt_id) do
    respond(prompt_id, %{"stopReason" => "cancelled"})
    %{state | prompt_id: nil, active_turn: nil}
  end

  defp handle_cursor(%{"id" => id, "method" => "session/close"}, state) do
    respond(id, %{})
    state
  end

  defp handle_cursor(_message, state), do: state

  defp complete_codex_turn(thread_id, turn_id, text) do
    item_id = "fake-item"

    notify("item/started", %{
      "threadId" => thread_id,
      "turnId" => turn_id,
      "item" => %{"id" => item_id, "type" => "agentMessage", "text" => ""}
    })

    notify("item/agentMessage/delta", %{
      "threadId" => thread_id,
      "turnId" => turn_id,
      "itemId" => item_id,
      "delta" => text <> " (streamed)"
    })

    notify("item/completed", %{
      "threadId" => thread_id,
      "turnId" => turn_id,
      "item" => %{
        "id" => item_id,
        "type" => "agentMessage",
        "phase" => "final_answer",
        "text" => text
      }
    })

    notify("turn/completed", %{
      "threadId" => thread_id,
      "turn" => %{"id" => turn_id, "status" => "completed", "items" => []}
    })
  end

  defp contains?(blocks, needle) do
    blocks
    |> Enum.map_join(" ", fn block -> block["text"] || "" end)
    |> String.contains?(needle)
  end

  defp notify_cursor(update),
    do:
      notify("session/update", %{"sessionId" => "fake-cursor-session", "update" => update}, true)

  defp respond(id, result), do: output(%{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp notify(method, params, jsonrpc \\ false),
    do:
      output(
        Map.merge(
          %{"method" => method, "params" => params},
          if(jsonrpc, do: %{"jsonrpc" => "2.0"}, else: %{})
        )
      )

  defp output(message), do: IO.puts(Jason.encode!(message))
end

Pika.Test.FakeJSONLProvider.run()
