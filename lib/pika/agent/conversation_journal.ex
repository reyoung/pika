defmodule Pika.Agent.ConversationJournal do
  @moduledoc "SQLite authority for normalized v2 Backend Sessions and conversation Turns."

  alias Pika.Repo

  @optimization_id "optimization"

  @spec allocate_session_id() :: String.t()
  def allocate_session_id, do: Ecto.UUID.generate()

  @spec start_session(String.t(), atom() | String.t(), String.t(), map(), String.t(), String.t()) ::
          {:ok, map()} | {:error, term()}
  def start_session(
        role,
        work_kind,
        work_id,
        backend_config,
        system_prompt,
        context_contents,
        opts \\ []
      )
      when is_binary(role) and is_binary(work_id) and is_map(backend_config) and
             is_binary(system_prompt) and is_binary(context_contents) and is_list(opts) do
    work_kind = to_string(work_kind)
    now = now_us()
    id = Keyword.get(opts, :id, allocate_session_id())
    recovery_sequence = Keyword.get(opts, :recovery_sequence, 0)

    transaction =
      Repo.transaction(fn ->
        [[sequence]] =
          Repo.query!(
            """
            SELECT COALESCE(MAX(session_sequence), 0) + 1
            FROM agent_sessions
            WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
            """,
            [@optimization_id, role, work_kind, work_id]
          ).rows

        Repo.query!(
          """
          INSERT INTO agent_sessions(
            id, optimization_id, role, work_kind, work_id, session_sequence,
            backend_config_json, system_prompt_sha256, context_sha256,
            status, recovery_sequence, started_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?)
          """,
          [
            id,
            @optimization_id,
            role,
            work_kind,
            work_id,
            sequence,
            Jason.encode!(backend_config),
            sha256(system_prompt),
            sha256(context_contents),
            recovery_sequence,
            now
          ]
        )

        session(id)
      end)

    finish(transaction, :session_start_failed)
  end

  @spec start_turn(String.t(), [map()]) :: {:ok, map()} | {:error, term()}
  def start_turn(session_id, input_messages)
      when is_binary(session_id) and is_list(input_messages) do
    now = now_us()

    transaction =
      Repo.transaction(fn ->
        session = session(session_id)

        if session == nil or session.status not in ["running", "awaiting_report"] do
          Repo.rollback(:session_not_running)
        end

        [[sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(turn_sequence), 0) + 1 FROM conversation_turns WHERE session_id = ?",
            [session_id]
          ).rows

        Repo.query!(
          """
          INSERT INTO conversation_turns(
            optimization_id, role, work_kind, work_id, session_id, session_sequence,
            turn_sequence, input_messages_json, output_messages_json, mcp_calls_json,
            partial, started_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', 1, ?)
          """,
          [
            @optimization_id,
            session.role,
            session.work_kind,
            session.work_id,
            session_id,
            session.session_sequence,
            sequence,
            Jason.encode!(input_messages),
            now
          ]
        )

        [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
        turn(id)
      end)

    finish(transaction, :turn_start_failed)
  end

  @spec bind_provider(String.t(), String.t()) :: {:ok, map()} | {:error, term()}
  def bind_provider(session_id, provider_session_id)
      when is_binary(session_id) and is_binary(provider_session_id) do
    case Repo.query!(
           "UPDATE agent_sessions SET provider_session_id = ? WHERE id = ? AND status = 'running'",
           [provider_session_id, session_id]
         ).num_rows do
      1 -> {:ok, session(session_id)}
      0 -> {:error, :session_not_running}
    end
  end

  @spec append_output(pos_integer(), map()) :: {:ok, map()} | {:error, term()}
  def append_output(turn_id, message) when is_integer(turn_id) and is_map(message),
    do: append(turn_id, "output_messages_json", message)

  @spec stream_output(pos_integer(), map()) :: {:ok, map()} | {:error, term()}
  def stream_output(turn_id, message) when is_integer(turn_id) and is_map(message) do
    case Repo.query!(
           """
           UPDATE conversation_turns
           SET output_messages_json = ?
           WHERE id = ? AND partial = 1
           """,
           [Jason.encode!([message]), turn_id]
         ).num_rows do
      1 -> {:ok, turn(turn_id)}
      0 -> {:error, :turn_not_open}
    end
  end

  @spec append_mcp_call(pos_integer(), map()) :: {:ok, map()} | {:error, term()}
  def append_mcp_call(turn_id, call) when is_integer(turn_id) and is_map(call),
    do: append(turn_id, "mcp_calls_json", call)

  @spec finish_turn(pos_integer(), String.t()) :: {:ok, map()} | {:error, term()}
  def finish_turn(turn_id, ended_reason) when is_integer(turn_id) and is_binary(ended_reason) do
    now = now_us()

    case Repo.query!(
           """
           UPDATE conversation_turns
           SET ended_reason = ?, partial = 0, ended_at = ?
           WHERE id = ? AND partial = 1
           """,
           [ended_reason, now, turn_id]
         ).num_rows do
      1 -> {:ok, turn(turn_id)}
      0 -> {:error, :turn_not_open}
    end
  end

  @spec interrupt_session(String.t(), String.t()) :: {:ok, map()} | {:error, term()}
  def interrupt_session(session_id, reason) when is_binary(reason) do
    now = now_us()

    Repo.query!(
      """
      UPDATE conversation_turns
      SET ended_reason = 'interrupted', ended_at = ?
      WHERE session_id = ? AND partial = 1
      """,
      [now, session_id]
    )

    case Repo.query!(
           """
           UPDATE agent_sessions
           SET status = 'interrupted', ended_reason = ?, ended_at = ?
           WHERE id = ? AND status IN ('running', 'awaiting_report')
           """,
           [reason, now, session_id]
         ).num_rows do
      1 -> {:ok, session(session_id)}
      0 -> {:error, :session_not_running}
    end
  end

  @spec await_followup(String.t(), String.t()) :: {:ok, map()} | {:error, term()}
  def await_followup(session_id, reason) when is_binary(session_id) and is_binary(reason) do
    case Repo.query!(
           """
           UPDATE agent_sessions
           SET status = 'awaiting_followup', ended_reason = ?, ended_at = NULL
           WHERE id = ? AND status IN ('running', 'awaiting_report')
           """,
           [reason, session_id]
         ).num_rows do
      1 -> {:ok, session(session_id)}
      0 -> {:error, :session_not_awaiting_followup}
    end
  end

  @spec resume_followup(String.t()) :: {:ok, map()} | {:error, term()}
  def resume_followup(session_id) when is_binary(session_id) do
    case Repo.query!(
           """
           UPDATE agent_sessions
           SET status = 'running', ended_reason = NULL, ended_at = NULL
           WHERE id = ? AND status = 'awaiting_followup'
           """,
           [session_id]
         ).num_rows do
      1 -> {:ok, session(session_id)}
      0 -> {:error, :session_not_awaiting_followup}
    end
  end

  @spec complete_session(String.t(), String.t(), String.t()) :: {:ok, map()} | {:error, term()}
  def complete_session(session_id, status, reason)
      when is_binary(session_id) and status in ["completed", "failed"] and is_binary(reason) do
    now = now_us()

    case Repo.query!(
           """
           UPDATE agent_sessions
           SET status = ?, ended_reason = ?, ended_at = ?
           WHERE id = ? AND status IN ('running', 'awaiting_report', 'awaiting_followup')
           """,
           [status, reason, now, session_id]
         ).num_rows do
      1 -> {:ok, session(session_id)}
      0 -> {:error, :session_not_completable}
    end
  end

  @spec interrupt_open_work_sessions(String.t(), atom() | String.t(), String.t(), String.t()) ::
          :ok
  def interrupt_open_work_sessions(role, work_kind, work_id, reason) do
    now = now_us()
    work_kind = to_string(work_kind)

    Repo.query!(
      """
      UPDATE conversation_turns
      SET ended_reason = 'interrupted', ended_at = ?
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ? AND partial = 1
      """,
      [now, @optimization_id, role, work_kind, work_id]
    )

    Repo.query!(
      """
      UPDATE agent_sessions
      SET status = 'interrupted', ended_reason = ?, ended_at = ?
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
        AND status IN ('running', 'awaiting_report', 'awaiting_followup')
      """,
      [reason, now, @optimization_id, role, work_kind, work_id]
    )

    :ok
  end

  @spec work_turns(String.t(), String.t(), String.t()) :: [map()]
  def work_turns(role, work_kind, work_id) do
    Repo.query!(
      """
      SELECT id, session_id, session_sequence, turn_sequence, input_messages_json,
             output_messages_json, mcp_calls_json, ended_reason, partial, started_at, ended_at
      FROM conversation_turns
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
      ORDER BY session_sequence, turn_sequence
      """,
      [@optimization_id, role, to_string(work_kind), work_id]
    ).rows
    |> Enum.map(&turn/1)
  end

  @spec work_sessions(String.t(), atom() | String.t(), String.t()) :: [map()]
  def work_sessions(role, work_kind, work_id) do
    Repo.query!(
      """
      SELECT id, role, work_kind, work_id, session_sequence, backend_config_json,
             system_prompt_sha256, context_sha256, provider_session_id, status,
             recovery_sequence, ended_reason, started_at, ended_at
      FROM agent_sessions
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
      ORDER BY session_sequence
      """,
      [@optimization_id, role, to_string(work_kind), work_id]
    ).rows
    |> Enum.map(&session_row/1)
  end

  @spec session(String.t()) :: map() | nil
  def session(id) do
    case Repo.query!(
           """
           SELECT id, role, work_kind, work_id, session_sequence, backend_config_json,
                  system_prompt_sha256, context_sha256, provider_session_id, status,
                  recovery_sequence, ended_reason, started_at, ended_at
           FROM agent_sessions WHERE id = ?
           """,
           [id]
         ).rows do
      [row] -> session_row(row)
      [] -> nil
    end
  end

  @spec next_recovery_sequence(String.t()) :: {:ok, pos_integer()} | {:error, term()}
  def next_recovery_sequence(session_id) do
    case Repo.transaction(fn ->
           case Repo.query!(
                  "SELECT recovery_sequence FROM agent_sessions WHERE id = ?",
                  [session_id]
                ).rows do
             [[sequence]] ->
               next = sequence + 1

               Repo.query!(
                 "UPDATE agent_sessions SET recovery_sequence = ? WHERE id = ?",
                 [next, session_id]
               )

               next

             [] ->
               Repo.rollback(:session_not_found)
           end
         end) do
      {:ok, sequence} -> {:ok, sequence}
      {:error, reason} -> {:error, reason}
    end
  end

  defp append(turn_id, column, value) do
    case turn(turn_id) do
      nil ->
        {:error, :turn_not_found}

      %{partial: false} ->
        {:error, :turn_not_open}

      turn ->
        values = Map.fetch!(turn, column_key(column)) ++ [value]

        Repo.query!("UPDATE conversation_turns SET #{column} = ? WHERE id = ?", [
          Jason.encode!(values),
          turn_id
        ])

        {:ok, turn(turn_id)}
    end
  end

  defp turn(id) when is_integer(id) do
    case Repo.query!(
           """
           SELECT id, session_id, session_sequence, turn_sequence, input_messages_json,
                  output_messages_json, mcp_calls_json, ended_reason, partial, started_at, ended_at
           FROM conversation_turns WHERE id = ?
           """,
           [id]
         ).rows do
      [row] -> turn(row)
      [] -> nil
    end
  end

  defp turn([
         id,
         session_id,
         session_sequence,
         turn_sequence,
         input,
         output,
         mcp_calls,
         ended_reason,
         partial,
         started_at,
         ended_at
       ]) do
    %{
      id: id,
      session_id: session_id,
      session_sequence: session_sequence,
      turn: turn_sequence,
      input_messages: Jason.decode!(input),
      output_messages: Jason.decode!(output),
      mcp_calls: Jason.decode!(mcp_calls),
      ended_reason: ended_reason,
      partial: partial in [1, true, "1", "true"],
      started_at: started_at,
      ended_at: ended_at
    }
  end

  defp session_row([
         id,
         role,
         work_kind,
         work_id,
         sequence,
         backend_config,
         system_prompt_sha256,
         context_sha256,
         provider_session_id,
         status,
         recovery_sequence,
         ended_reason,
         started_at,
         ended_at
       ]) do
    %{
      id: id,
      role: role,
      work_kind: work_kind,
      work_id: work_id,
      session_sequence: sequence,
      backend_config: Jason.decode!(backend_config),
      system_prompt_sha256: system_prompt_sha256,
      context_sha256: context_sha256,
      provider_session_id: provider_session_id,
      status: status,
      recovery_sequence: recovery_sequence,
      ended_reason: ended_reason,
      started_at: started_at,
      ended_at: ended_at
    }
  end

  defp column_key("output_messages_json"), do: :output_messages
  defp column_key("mcp_calls_json"), do: :mcp_calls

  defp finish({:ok, value}, _tag), do: {:ok, value}
  defp finish({:error, reason}, tag), do: {:error, {tag, reason}}
  defp sha256(value), do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)
  defp now_us, do: System.system_time(:microsecond)
end
