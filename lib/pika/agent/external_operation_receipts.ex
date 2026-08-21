defmodule Pika.Agent.ExternalOperationReceipts do
  @moduledoc """
  Work-scoped receipts for compatibility adapters whose serialized domain owner is another process.

  Unlike `OperationReceipts`, this module does not hold a SQLite transaction while calling that
  process. The domain owner remains responsible for serializing and durably committing its action;
  this adapter then records the stable Role Work receipt before returning to the Backend Session.
  """

  alias Pika.Agent.Role.{Invocation, Work}
  alias Pika.Repo

  def run(work, role_id, invocation, request_sha256, operation),
    do: run(work, role_id, invocation, request_sha256, operation, &{:ok, &1})

  def run(
        %Work{} = work,
        role_id,
        %Invocation{} = invocation,
        request_sha256,
        operation,
        replay
      )
      when is_function(operation, 0) and is_function(replay, 1) do
    case lookup(work, role_id, invocation) do
      nil -> execute_and_record(work, role_id, invocation, request_sha256, operation, replay)
      {^request_sha256, response_json} -> replay.(Jason.decode!(response_json))
      {_other_sha256, _response_json} -> {:error, :idempotency_conflict}
    end
  rescue
    error -> {:error, {:operation_receipt_failed, Exception.message(error)}}
  end

  defp execute_and_record(work, role_id, invocation, request_sha256, operation, replay) do
    with {:ok, value} <- operation.() do
      value = Pika.JSONSafe.json_safe(value)
      response_json = Jason.encode!(value)

      Repo.query!(
        """
        INSERT INTO agent_operation_receipts(
          campaign_id, role, work_kind, work_id, operation, idempotency_key,
          request_sha256, response_json, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(campaign_id, role, work_kind, work_id, operation, idempotency_key)
        DO NOTHING
        """,
        identity(work, role_id, invocation) ++
          [request_sha256, response_json, System.system_time(:microsecond)]
      )

      case lookup(work, role_id, invocation) do
        {^request_sha256, ^response_json} -> {:ok, value}
        {^request_sha256, stored_json} -> replay.(Jason.decode!(stored_json))
        {_other_sha256, _stored_json} -> {:error, :idempotency_conflict}
        nil -> {:error, :operation_receipt_missing}
      end
    end
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
