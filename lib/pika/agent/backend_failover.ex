defmodule Pika.Agent.BackendFailover do
  @moduledoc "Persistent, per-Work availability ledger for a Backend Fallback Chain."

  alias Pika.Agent.{BackendSelection, Work}
  alias Pika.AgentBackend.Failure
  alias Pika.Optimization.Config.Agent
  alias Pika.Repo

  @optimization_id "optimization"

  @spec select(Agent.t(), Work.t(), non_neg_integer()) ::
          {:ok, BackendSelection.t()} | {:blocked, map()}
  def select(%Agent{} = configured, %Work{} = work, now \\ System.system_time(:second)) do
    chain = Agent.chain(configured)
    digest = Agent.chain_sha256(configured)
    activate_chain(work, digest)
    blocked = blocked_indexes(work, digest, now)

    case Enum.with_index(chain) |> Enum.find(fn {_endpoint, index} -> index not in blocked end) do
      {endpoint, index} ->
        {:ok,
         %BackendSelection{
           endpoint: endpoint,
           chain_index: index,
           chain_sha256: digest,
           chain_length: length(chain)
         }}

      nil ->
        {:blocked, blocked_work(work, digest, length(chain), now)}
    end
  end

  @spec block(Work.t(), BackendSelection.t(), Failure.t(), String.t()) ::
          :ok | {:error, term()}
  def block(
        %Work{} = work,
        %BackendSelection{} = selection,
        %Failure{} = failure,
        session_id
      )
      when is_binary(session_id) do
    if Failure.eligible?(failure) do
      now_us = System.system_time(:microsecond)
      work_kind = to_string(work.kind)
      reason = "backend_failover:#{failure.category}:#{failure.code || "unknown"}"
      failure_map = Failure.to_map(failure)

      case Repo.transaction(fn ->
             Repo.query!(
               """
               UPDATE conversation_turns
               SET ended_reason = 'interrupted', partial = 0, ended_at = ?
               WHERE session_id = ? AND partial = 1
               """,
               [now_us, session_id]
             )

             Repo.query!(
               """
               UPDATE agent_sessions
               SET status = 'interrupted', ended_reason = ?, ended_at = ?
               WHERE id = ? AND status IN ('running', 'awaiting_report', 'awaiting_followup')
               """,
               [reason, now_us, session_id]
             )

             Repo.query!(
               """
               UPDATE agent_backend_failures SET active = 0, updated_at = ?
               WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
                 AND chain_sha256 != ?
               """,
               [
                 now_us,
                 @optimization_id,
                 work.role_id,
                 work_kind,
                 work.id,
                 selection.chain_sha256
               ]
             )

             Repo.query!(
               """
               INSERT INTO agent_backend_failures(
                 optimization_id, role, work_kind, work_id, chain_sha256, chain_length,
                 endpoint_index, backend, category, provider_code, message, retry_at,
                 details_json, active, failed_at, updated_at, cleared_at
               ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, NULL)
               ON CONFLICT(optimization_id, role, work_kind, work_id, chain_sha256, endpoint_index)
               DO UPDATE SET
                 chain_length = excluded.chain_length,
                 backend = excluded.backend,
                 category = excluded.category,
                 provider_code = excluded.provider_code,
                 message = excluded.message,
                 retry_at = excluded.retry_at,
                 details_json = excluded.details_json,
                 active = 1,
                 failed_at = excluded.failed_at,
                 updated_at = excluded.updated_at,
                 cleared_at = NULL
               """,
               [
                 @optimization_id,
                 work.role_id,
                 work_kind,
                 work.id,
                 selection.chain_sha256,
                 selection.chain_length,
                 selection.chain_index,
                 to_string(selection.endpoint.backend),
                 to_string(failure.category),
                 if(is_nil(failure.code), do: nil, else: to_string(failure.code)),
                 failure.message,
                 failure.retry_at,
                 Jason.encode!(failure_map.details),
                 now_us,
                 now_us
               ]
             )
           end) do
        {:ok, _value} -> :ok
        {:error, reason} -> {:error, reason}
      end
    else
      {:error, :failure_not_eligible_for_failover}
    end
  end

  @spec retry(Work.t(), Agent.t()) :: :ok
  def retry(%Work{} = work, %Agent{} = configured) do
    digest = Agent.chain_sha256(configured)
    now = System.system_time(:microsecond)

    Repo.query!(
      """
      UPDATE agent_backend_failures
      SET active = 0, cleared_at = ?, updated_at = ?
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
        AND chain_sha256 = ? AND active = 1 AND cleared_at IS NULL
      """,
      [
        now,
        now,
        @optimization_id,
        work.role_id,
        to_string(work.kind),
        work.id,
        digest
      ]
    )

    :ok
  end

  @spec blocked_works(non_neg_integer()) :: [map()]
  def blocked_works(now \\ System.system_time(:second)) do
    active_failures(now)
    |> Enum.group_by(&{&1.role, &1.work_kind, &1.work_id, &1.chain_sha256})
    |> Enum.flat_map(fn {{role, work_kind, work_id, digest}, failures} ->
      chain_length = failures |> List.first() |> Map.fetch!(:chain_length)
      indexes = failures |> Enum.map(& &1.endpoint_index) |> Enum.uniq()

      if length(indexes) >= chain_length do
        [
          %{
            role: role,
            work_kind: work_kind,
            work_id: work_id,
            chain_sha256: digest,
            chain_length: chain_length,
            next_retry_at:
              failures |> Enum.map(& &1.retry_at) |> Enum.reject(&is_nil/1) |> min_or_nil(),
            failures: Enum.sort_by(failures, & &1.endpoint_index)
          }
        ]
      else
        []
      end
    end)
    |> Enum.sort_by(&{&1.role, &1.work_kind, &1.work_id})
  end

  @spec recovering_failover?(Work.t()) :: boolean()
  def recovering_failover?(%Work{} = work) do
    case Repo.query!(
           """
           SELECT ended_reason FROM agent_sessions
           WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
           ORDER BY session_sequence DESC LIMIT 1
           """,
           [@optimization_id, work.role_id, to_string(work.kind), work.id]
         ).rows do
      [["backend_failover:" <> _rest]] -> true
      _other -> false
    end
  end

  defp activate_chain(work, digest) do
    Repo.query!(
      """
      UPDATE agent_backend_failures
      SET active = CASE WHEN chain_sha256 = ? THEN 1 ELSE 0 END
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
        AND cleared_at IS NULL
      """,
      [digest, @optimization_id, work.role_id, to_string(work.kind), work.id]
    )

    :ok
  end

  defp blocked_indexes(work, digest, now) do
    Repo.query!(
      """
      SELECT endpoint_index FROM agent_backend_failures
      WHERE optimization_id = ? AND role = ? AND work_kind = ? AND work_id = ?
        AND chain_sha256 = ? AND active = 1 AND cleared_at IS NULL
        AND (retry_at IS NULL OR retry_at > ?)
      """,
      [@optimization_id, work.role_id, to_string(work.kind), work.id, digest, now]
    ).rows
    |> List.flatten()
  end

  defp blocked_work(work, digest, chain_length, now) do
    failures =
      active_failures(now)
      |> Enum.filter(
        &(&1.role == work.role_id and &1.work_kind == to_string(work.kind) and
            &1.work_id == work.id and &1.chain_sha256 == digest)
      )
      |> Enum.sort_by(& &1.endpoint_index)

    %{
      role: work.role_id,
      work_kind: to_string(work.kind),
      work_id: work.id,
      chain_sha256: digest,
      chain_length: chain_length,
      next_retry_at:
        failures |> Enum.map(& &1.retry_at) |> Enum.reject(&is_nil/1) |> min_or_nil(),
      failures: failures
    }
  end

  defp active_failures(now) do
    Repo.query!(
      """
      SELECT role, work_kind, work_id, chain_sha256, chain_length, endpoint_index,
             backend, category, provider_code, message, retry_at, details_json, failed_at
      FROM agent_backend_failures
      WHERE optimization_id = ? AND active = 1 AND cleared_at IS NULL
        AND (retry_at IS NULL OR retry_at > ?)
      ORDER BY role, work_kind, work_id, endpoint_index
      """,
      [@optimization_id, now]
    ).rows
    |> Enum.map(fn [
                     role,
                     work_kind,
                     work_id,
                     digest,
                     chain_length,
                     endpoint_index,
                     backend,
                     category,
                     code,
                     message,
                     retry_at,
                     details,
                     failed_at
                   ] ->
      %{
        role: role,
        work_kind: work_kind,
        work_id: work_id,
        chain_sha256: digest,
        chain_length: chain_length,
        endpoint_index: endpoint_index,
        backend: backend,
        category: category,
        provider_code: code,
        message: message,
        retry_at: retry_at,
        details: Jason.decode!(details),
        failed_at: failed_at
      }
    end)
  end

  defp min_or_nil([]), do: nil
  defp min_or_nil(values), do: Enum.min(values)
end
