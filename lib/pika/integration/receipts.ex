defmodule Pika.Integration.Lifecycle.Receipts do
  @moduledoc false

  alias Pika.Integration.Lifecycle.{Command, Identity}
  alias Pika.Repo

  def run(%Identity{} = identity, %Command{} = command, operation, replay)
      when is_function(operation, 0) and is_function(replay, 1) do
    request_sha256 = request_sha256(command)

    transaction =
      Repo.transaction(fn ->
        case lookup(identity, command) do
          nil ->
            execute_and_record(identity, command, request_sha256, operation)

          {^request_sha256, response_json} ->
            replay_recorded(response_json, replay)

          {_other_sha256, _response_json} ->
            Repo.rollback(:idempotency_conflict)
        end
      end)

    case transaction do
      {:ok, {:ok, value}} -> {:ok, value}
      {:error, {:operation_failed, reason}} -> {:error, reason}
      {:error, reason} -> {:error, reason}
    end
  rescue
    error -> {:error, {:operation_receipt_failed, Exception.message(error)}}
  end

  defp lookup(identity, command) do
    case Repo.query!(
           """
           SELECT request_sha256, response_json
           FROM agent_operation_receipts
           WHERE campaign_id = ? AND role = 'integration' AND work_kind = 'attempt'
             AND work_id = ? AND operation = ? AND idempotency_key = ?
           """,
           command_identity(identity, command)
         ).rows do
      [[request_sha256, response_json]] -> {request_sha256, response_json}
      [] -> nil
    end
  end

  defp execute_and_record(identity, command, request_sha256, operation) do
    case operation.() do
      {:ok, value} ->
        value = Pika.JSONSafe.json_safe(value)
        response_json = Jason.encode!(value)
        now = System.system_time(:microsecond)

        Repo.query!(
          """
          INSERT INTO agent_operation_receipts(
            campaign_id, role, work_kind, work_id, operation, idempotency_key,
            request_sha256, response_json, created_at
          ) VALUES (?, 'integration', 'attempt', ?, ?, ?, ?, ?, ?)
          """,
          command_identity(identity, command) ++ [request_sha256, response_json, now]
        )

        {:ok, value}

      {:error, reason} ->
        Repo.rollback({:operation_failed, reason})
    end
  end

  defp replay_recorded(response_json, replay) do
    case replay.(Jason.decode!(response_json)) do
      {:ok, value} -> {:ok, value}
      {:error, reason} -> Repo.rollback({:operation_failed, reason})
    end
  end

  defp request_sha256(command) do
    Jason.encode!(%{
      operation: Atom.to_string(command.operation),
      arguments: Pika.JSONSafe.json_safe(command.params)
    })
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  defp command_identity(identity, command) do
    [
      identity.campaign_id,
      identity.attempt_id,
      Atom.to_string(command.operation),
      command.idempotency_key
    ]
  end
end
