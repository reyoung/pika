defmodule Pika.Test.FakeCursorHeadless do
  def run do
    args = System.argv()
    log_args(args)

    cond do
      args == ["status"] -> status()
      args == ["create-chat"] -> create_chat()
      "-p" in args -> run_turn(args)
      true -> System.halt(2)
    end
  end

  defp status do
    if System.get_env("PIKA_FAKE_CURSOR_AUTH") == "failed" do
      IO.puts(:stderr, "Not authenticated")
      System.halt(1)
    else
      IO.puts("Logged in as pika-test@example.com")
    end
  end

  defp create_chat do
    id =
      System.get_env("PIKA_FAKE_CURSOR_CHAT_ID") ||
        :crypto.strong_rand_bytes(16) |> Base.encode16(case: :lower)

    IO.puts(id)
  end

  defp run_turn(args) do
    session_id = option(args, "--resume")
    prompt = List.last(args)

    output(%{
      "type" => "system",
      "subtype" => "init",
      "apiKeySource" => "login",
      "cwd" => File.cwd!(),
      "session_id" => session_id,
      "unknown_future_field" => true
    })

    output(%{
      "type" => "user",
      "message" => %{"role" => "user", "content" => [%{"type" => "text", "text" => prompt}]},
      "session_id" => session_id
    })

    if String.contains?(prompt, "hold") do
      Process.sleep(:infinity)
    end

    if String.contains?(prompt, "failure") do
      assistant(session_id, "partial")
      IO.puts(:stderr, "Usage limit exceeded; quota exhausted (unstructured fake stderr)")
      System.halt(7)
    end

    assistant(session_id, "Hello")

    if String.contains?(prompt, "tools") do
      tool_event(session_id, "started", %{
        "shellToolCall" => %{"args" => %{"command" => "echo hello"}}
      })

      tool_event(session_id, "completed", %{
        "shellToolCall" => %{
          "args" => %{"command" => "echo hello"},
          "result" => %{"success" => %{"stdout" => "hello\n"}}
        }
      })
    end

    output(%{
      "type" => "future_cursor_event",
      "payload" => %{"safe_to_ignore" => true},
      "session_id" => session_id
    })

    final = if String.contains?(prompt, "diverge"), do: "Correct final", else: "Hello world"

    result = %{
      "type" => "result",
      "subtype" => "success",
      "is_error" => false,
      "result" => final,
      "session_id" => session_id,
      "request_id" => "fake-request"
    }

    result =
      if String.contains?(prompt, "usage"),
        do: Map.put(result, "usage", %{"inputTokens" => 17, "outputTokens" => 3}),
        else: result

    output(result)
  end

  defp assistant(session_id, text) do
    output(%{
      "type" => "assistant",
      "message" => %{"role" => "assistant", "content" => [%{"type" => "text", "text" => text}]},
      "session_id" => session_id
    })
  end

  defp tool_event(session_id, subtype, tool_call) do
    output(%{
      "type" => "tool_call",
      "subtype" => subtype,
      "call_id" => "fake-command",
      "tool_call" => tool_call,
      "session_id" => session_id
    })
  end

  defp option(args, name) do
    case Enum.find_index(args, &(&1 == name)) do
      nil -> nil
      index -> Enum.at(args, index + 1)
    end
  end

  defp log_args(args) do
    if path = System.get_env("PIKA_FAKE_CURSOR_ARGV_LOG") do
      File.write!(path, Jason.encode!(args) <> "\n", [:append])
    end
  end

  defp output(message), do: IO.puts(Jason.encode!(message))
end

Pika.Test.FakeCursorHeadless.run()
