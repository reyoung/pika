defmodule Pika.Attempt.History do
  @moduledoc "Materializes terminal Attempt conversation and summary journals."

  alias Pika.Agent.ConversationJournal
  alias Pika.FileSystem

  @spec record(Path.t(), pos_integer(), map()) :: :ok | {:error, term()}
  def record(attempt_root, attempt_id, summary) when is_map(summary) do
    turns = ConversationJournal.work_turns("iteration", "attempt", to_string(attempt_id))

    messages =
      Enum.map_join(turns, "", fn turn ->
        Jason.encode!(%{
          turn: turn.turn,
          session_sequence: turn.session_sequence,
          session_id: turn.session_id,
          input_messages: turn.input_messages,
          output_messages: turn.output_messages,
          mcp_calls: turn.mcp_calls,
          ended_reason: turn.ended_reason
        }) <> "\n"
      end)

    with :ok <- FileSystem.atomic_write(Path.join(attempt_root, "message.jsonl"), messages),
         :ok <- append_summary(Path.join(attempt_root, "summary.jsonl"), summary) do
      :ok
    end
  end

  defp append_summary(path, summary) do
    with {:ok, io} <- File.open(path, [:append, :binary]),
         :ok <- IO.binwrite(io, Jason.encode!(summary) <> "\n"),
         :ok <- :file.sync(io),
         :ok <- File.close(io) do
      :ok
    end
  end
end
