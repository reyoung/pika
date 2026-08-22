defmodule Pika.AgentFollowupStore do
  @moduledoc false

  alias Pika.Agent.Role.Work
  alias Pika.Repo

  @pending ~w(requested running completed)

  def request(%Work{} = target, target_session_id, required_operations, context) do
    id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)

    transaction =
      Repo.transaction(fn ->
        case pending_for_session(target_session_id) do
          nil ->
            Repo.query!(
              """
              INSERT INTO agent_followup_requests(
                id, campaign_id, target_role, target_work_kind, target_work_id,
                target_session_id, required_operations_json, context_json, status,
                created_at, updated_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'requested', ?, ?)
              """,
              [
                id,
                target.campaign_id,
                target.role_id,
                Atom.to_string(target.kind),
                target.id,
                target_session_id,
                Jason.encode!(required_operations),
                Jason.encode!(Pika.JSONSafe.json_safe(context)),
                now,
                now
              ]
            )

            request_row("WHERE id = ?", [id])

          request ->
            request
        end
      end)

    finish(transaction)
  rescue
    error -> {:error, {:followup_request_failed, Exception.message(error)}}
  end

  def runnable_work(campaign_id) do
    Repo.query!(
      "SELECT id FROM agent_followup_requests WHERE campaign_id = ? AND status IN ('requested', 'running') ORDER BY created_at",
      [campaign_id]
    ).rows
    |> Enum.map(fn [id] ->
      %Work{
        role_id: "integration_followup",
        kind: :agent_followup,
        id: id,
        campaign_id: campaign_id
      }
    end)
  end

  def get(id) do
    case request_row("WHERE id = ?", [id]) do
      nil -> {:error, :followup_request_not_found}
      request -> {:ok, request}
    end
  end

  def mark_running(id), do: transition(id, ~w(requested running), "running", nil)

  def submit(id, message) when is_binary(message) do
    message = String.trim(message)

    if message == "",
      do: {:error, :followup_message_required},
      else: transition(id, ~w(requested running), "completed", message)
  end

  def submit(_id, _message), do: {:error, :followup_message_required}

  def consume(target_session_id) do
    now = System.system_time(:microsecond)

    transaction =
      Repo.transaction(fn ->
        case request_row(
               "WHERE target_session_id = ? AND status = 'completed' ORDER BY created_at LIMIT 1",
               [target_session_id]
             ) do
          nil ->
            nil

          request ->
            Repo.query!(
              "UPDATE agent_followup_requests SET status = 'delivered', delivered_at = ?, updated_at = ? WHERE id = ? AND status = 'completed'",
              [now, now, request.id]
            )

            request.message
        end
      end)

    finish(transaction)
  end

  defp pending_for_session(session_id) do
    placeholders = Enum.map_join(@pending, ",", fn _ -> "?" end)

    request_row(
      "WHERE target_session_id = ? AND status IN (#{placeholders}) ORDER BY created_at LIMIT 1",
      [session_id | @pending]
    )
  end

  defp transition(id, from, status, message) do
    now = System.system_time(:microsecond)
    placeholders = Enum.map_join(from, ",", fn _ -> "?" end)
    completed_at = if status == "completed", do: now, else: nil

    transaction =
      Repo.transaction(fn ->
        Repo.query!(
          "UPDATE agent_followup_requests SET status = ?, message = COALESCE(?, message), completed_at = COALESCE(?, completed_at), updated_at = ? WHERE id = ? AND status IN (#{placeholders})",
          [status, message, completed_at, now, id | from]
        )

        request_row("WHERE id = ?", [id])
      end)

    finish(transaction)
  end

  defp request_row(where, params) do
    case Repo.query!(
           """
           SELECT id, campaign_id, target_role, target_work_kind, target_work_id,
                  target_session_id, required_operations_json, context_json, message,
                  status, created_at, updated_at, completed_at, delivered_at
           FROM agent_followup_requests #{where}
           """,
           params
         ).rows do
      [
        [
          id,
          campaign_id,
          role,
          kind,
          work_id,
          session_id,
          required,
          context,
          message,
          status,
          created_at,
          updated_at,
          completed_at,
          delivered_at
        ]
        | _
      ] ->
        %{
          id: id,
          campaign_id: campaign_id,
          target_role: role,
          target_work_kind: kind,
          target_work_id: work_id,
          target_session_id: session_id,
          required_operations: Jason.decode!(required),
          context: Jason.decode!(context),
          message: message,
          status: status,
          created_at: created_at,
          updated_at: updated_at,
          completed_at: completed_at,
          delivered_at: delivered_at
        }

      [] ->
        nil
    end
  end

  defp finish({:ok, result}) do
    if not is_nil(Process.whereis(Pika.PubSub)) and is_map(result) do
      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        Pika.Persistence.topic(result.campaign_id),
        {:campaign_updated, %{campaign_id: result.campaign_id}}
      )
    end

    {:ok, result}
  end

  defp finish({:error, reason}), do: {:error, reason}
end
