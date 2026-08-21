defmodule Pika.Agent.OperationReceipts do
  @moduledoc "Persistent work-scoped idempotency boundary for Agent Role commands."

  alias Pika.Agent.Role.{Invocation, Work}
  alias Pika.Repo

  def run(
        %Work{} = work,
        role_id,
        %Invocation{} = invocation,
        request_sha256,
        operation
      )
      when is_function(operation, 0) do
    run(work, role_id, invocation, request_sha256, operation, fn response ->
      {:ok, response}
    end)
  end

  def run(
        %Work{} = work,
        role_id,
        %Invocation{} = invocation,
        request_sha256,
        operation,
        replay
      )
      when is_function(operation, 0) and is_function(replay, 1) do
    transaction =
      Repo.transaction(fn ->
        case lookup(work, role_id, invocation) do
          nil -> execute_and_record(work, role_id, invocation, request_sha256, operation)
          {^request_sha256, response_json} ->
            replay_recorded(response_json, replay)
          {_other_sha256, _response_json} -> Repo.rollback(:idempotency_conflict)
        end
      end)

    case transaction do
      {:ok, {:ok, value}} ->
        {:ok, value}

      {:error, {:operation_failed, reason}} ->
        {:error, reason}

      {:error, reason} ->
        {:error, reason}
    end
  rescue
    error -> {:error, {:operation_receipt_failed, Exception.message(error)}}
  end

  defp lookup(work, role_id, invocation) do
    case Repo.query!(
           """
           SELECT request_sha256, response_json
           FROM agent_operation_receipts
           WHERE campaign_id = ? AND role = ? AND work_kind = ? AND work_id = ?
             AND operation = ? AND idempotency_key = ?
           """,
           identity(work, role_id, invocation)
         ).rows do
      [[request_sha256, response_json]] -> {request_sha256, response_json}
      [] -> nil
    end
  end

  defp execute_and_record(work, role_id, invocation, request_sha256, operation) do
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
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
          """,
          identity(work, role_id, invocation) ++ [request_sha256, response_json, now]
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

  defp identity(work, role_id, invocation) do
    [
      work.campaign_id,
      role_id,
      Atom.to_string(work.kind),
      work.id,
      invocation.operation,
      invocation.idempotency_key
    ]
  end
end
