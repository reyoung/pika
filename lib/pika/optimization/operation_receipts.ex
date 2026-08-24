defmodule Pika.Optimization.OperationReceipts do
  @moduledoc "Work-scoped idempotency boundary for every v2 Agent command."

  alias Pika.Repo

  @optimization_id "optimization"

  @type identity :: %{
          required(:role) => String.t(),
          required(:work_kind) => String.t() | atom(),
          required(:work_id) => String.t()
        }

  @spec run(identity(), String.t(), String.t(), term(), (-> {:ok, term()} | {:error, term()})) ::
          {:ok, term(), :executed | :replayed} | {:error, term()}
  def run(identity, operation, idempotency_key, request, callback)
      when is_map(identity) and is_binary(operation) and is_binary(idempotency_key) and
             is_function(callback, 0) do
    with :ok <- validate_identity(identity),
         :ok <- validate_key(idempotency_key) do
      request_sha256 = digest(request)

      case Repo.transaction(fn ->
             case lookup(identity, operation, idempotency_key) do
               nil -> execute(identity, operation, idempotency_key, request_sha256, callback)
               {^request_sha256, response_json} -> {:replayed, Jason.decode!(response_json)}
               {_different_sha, _response_json} -> Repo.rollback(:idempotency_conflict)
             end
           end) do
        {:ok, {:executed, response}} -> {:ok, response, :executed}
        {:ok, {:replayed, response}} -> {:ok, response, :replayed}
        {:error, {:operation_failed, reason}} -> {:error, reason}
        {:error, reason} -> {:error, reason}
      end
    end
  rescue
    error -> {:error, {:operation_receipt_failed, Exception.message(error)}}
  end

  defp execute(identity, operation, idempotency_key, request_sha256, callback) do
    case callback.() do
      {:ok, value} ->
        response = value |> Pika.JSONSafe.json_safe() |> Jason.encode!() |> Jason.decode!()
        now = System.system_time(:microsecond)

        Repo.query!(
          """
          INSERT INTO operation_receipts(
            optimization_id, role, work_kind, work_id, operation, idempotency_key,
            request_sha256, response_json, created_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
          """,
          [
            @optimization_id,
            identity.role,
            to_string(identity.work_kind),
            identity.work_id,
            operation,
            idempotency_key,
            request_sha256,
            Jason.encode!(response),
            now
          ]
        )

        {:executed, response}

      {:error, reason} ->
        Repo.rollback({:operation_failed, reason})

      other ->
        Repo.rollback({:operation_failed, {:invalid_operation_result, other}})
    end
  end

  defp lookup(identity, operation, idempotency_key) do
    case Repo.query!(
           """
           SELECT request_sha256, response_json FROM operation_receipts
           WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
             AND operation = ? AND idempotency_key = ?
           """,
           [
             @optimization_id,
             identity.role,
             to_string(identity.work_kind),
             identity.work_id,
             operation,
             idempotency_key
           ]
         ).rows do
      [[request_sha256, response_json]] -> {request_sha256, response_json}
      [] -> nil
    end
  end

  defp validate_identity(%{role: role, work_kind: work_kind, work_id: work_id})
       when is_binary(role) and role != "" and (is_binary(work_kind) or is_atom(work_kind)) and
              is_binary(work_id) and work_id != "",
       do: :ok

  defp validate_identity(_identity), do: {:error, :invalid_operation_identity}

  defp validate_key(key) do
    if String.trim(key) != "" and byte_size(key) <= 256,
      do: :ok,
      else: {:error, :invalid_idempotency_key}
  end

  defp digest(value) do
    value
    |> canonical()
    |> :erlang.term_to_binary([:deterministic])
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  defp canonical(value) when is_map(value) do
    value
    |> Enum.map(fn {key, item} -> {to_string(key), canonical(item)} end)
    |> Enum.sort_by(&elem(&1, 0))
    |> then(&{:map, &1})
  end

  defp canonical(value) when is_list(value), do: {:list, Enum.map(value, &canonical/1)}
  defp canonical(value) when is_tuple(value), do: value |> Tuple.to_list() |> canonical()
  defp canonical(value) when is_atom(value), do: {:atom, Atom.to_string(value)}
  defp canonical(value), do: value
end
