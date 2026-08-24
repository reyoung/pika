defmodule Pika.Agent.RecoveryContext do
  @moduledoc "Rebuilds immutable recovery-N files from the SQLite Conversation Journal."

  alias Pika.Agent.ConversationJournal
  alias Pika.FileSystem

  @spec build(Path.t(), String.t(), map()) :: {:ok, map()} | {:error, term()}
  def build(workspace_root, session_id, state)
      when is_binary(workspace_root) and is_binary(session_id) and is_map(state) do
    with session when is_map(session) <- ConversationJournal.session(session_id),
         {:ok, sequence} <- ConversationJournal.next_recovery_sequence(session_id),
         {:ok, directory} <- recovery_directory(workspace_root, session_id, sequence),
         turns <-
           ConversationJournal.work_turns(session.role, session.work_kind, session.work_id),
         :ok <- write_messages(Path.join(directory, "messages.jsonl"), turns),
         :ok <- write_state(Path.join(directory, "recovery.json"), session, state, sequence) do
      {:ok,
       %{
         sequence: sequence,
         directory: directory,
         messages_file: Path.join(directory, "messages.jsonl"),
         state_file: Path.join(directory, "recovery.json")
       }}
    else
      nil -> {:error, :session_not_found}
      {:error, _reason} = error -> error
    end
  end

  defp recovery_directory(workspace_root, session_id, sequence) do
    directory =
      Path.join([
        workspace_root,
        "agent-sessions",
        session_id,
        "recovery-#{sequence |> Integer.to_string() |> String.pad_leading(2, "0")}"
      ])

    case File.mkdir_p(directory) do
      :ok -> {:ok, directory}
      {:error, reason} -> {:error, {:recovery_directory_failed, reason}}
    end
  end

  defp write_messages(path, turns) do
    contents =
      Enum.map_join(turns, "", fn turn ->
        Jason.encode!(%{
          turn: turn.turn,
          session_sequence: turn.session_sequence,
          session_id: turn.session_id,
          started_at: turn.started_at,
          input_messages: turn.input_messages,
          output_messages: turn.output_messages,
          mcp_calls: turn.mcp_calls,
          ended_reason: turn.ended_reason || if(turn.partial, do: "interrupted", else: nil)
        }) <> "\n"
      end)

    FileSystem.atomic_write(path, contents)
  end

  defp write_state(path, session, state, sequence) do
    contents =
      state
      |> Map.merge(%{
        schema_version: 1,
        recovery_sequence: sequence,
        role: session.role,
        work_kind: session.work_kind,
        work_id: session.work_id,
        backend_session_id: session.id,
        previous_ended_reason: session.ended_reason
      })
      |> Jason.encode!()

    FileSystem.atomic_write(path, contents)
  end
end
