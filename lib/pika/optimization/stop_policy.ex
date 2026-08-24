defmodule Pika.Optimization.StopPolicy do
  @moduledoc "Evaluates the reviewed Baseline stopping contract before new Attempt creation."

  alias Pika.Optimization.Persistence
  alias Pika.Repo

  @optimization_id "optimization"

  @spec evaluate(DateTime.t()) :: :continue | {:drain, String.t()} | {:error, term()}
  def evaluate(now \\ DateTime.utc_now()) do
    with {:ok, stopping} <- stopping_policy() do
      [[attempt_count]] =
        Repo.query!("SELECT COUNT(*) FROM attempts WHERE optimization_id = ?", [@optimization_id]).rows

      optimization = Persistence.current()

      elapsed_seconds =
        div(DateTime.to_unix(now, :microsecond) - optimization.inserted_at, 1_000_000)

      attempt_due =
        is_integer(stopping["max_attempts"]) and attempt_count >= stopping["max_attempts"]

      duration_due =
        is_integer(stopping["max_duration_seconds"]) and
          elapsed_seconds >= stopping["max_duration_seconds"]

      case stopping["mode"] do
        "manual" ->
          :continue

        "attempt_limit" ->
          if(attempt_due, do: {:drain, "attempt_limit"}, else: :continue)

        "duration" ->
          if(duration_due, do: {:drain, "duration_limit"}, else: :continue)

        "attempt_or_duration" ->
          cond do
            attempt_due -> {:drain, "attempt_limit"}
            duration_due -> {:drain, "duration_limit"}
            true -> :continue
          end
      end
    end
  end

  @spec apply(DateTime.t()) :: :ok | {:error, term()}
  def apply(now \\ DateTime.utc_now()) do
    case evaluate(now) do
      :continue -> :ok
      {:drain, reason} -> mark_draining(reason)
      {:error, _reason} = error -> error
    end
  end

  @spec remaining_attempt_capacity() :: :unlimited | non_neg_integer()
  def remaining_attempt_capacity do
    with {:ok, stopping} <- stopping_policy() do
      case stopping["max_attempts"] do
        max_attempts when is_integer(max_attempts) ->
          [[attempt_count]] =
            Repo.query!("SELECT COUNT(*) FROM attempts WHERE optimization_id = ?", [
              @optimization_id
            ]).rows

          max(max_attempts - attempt_count, 0)

        nil ->
          :unlimited
      end
    else
      _error -> :unlimited
    end
  end

  defp stopping_policy do
    case Repo.query!(
           """
           SELECT work_relative_path FROM baseline_revisions
           WHERE optimization_id = ? AND status = 'accepted'
           ORDER BY revision DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[relative_path]] ->
        path =
          Path.join([
            Persistence.current().workspace_canonical_path,
            relative_path,
            "baseline-definition.json"
          ])

        with {:ok, contents} <- File.read(path),
             {:ok, definition} <- Jason.decode(contents),
             stopping when is_map(stopping) <- definition["stopping"] do
          {:ok, stopping}
        else
          {:error, reason} -> {:error, {:stopping_policy_unreadable, reason}}
          _other -> {:error, :stopping_policy_missing}
        end

      [] ->
        {:error, :accepted_baseline_missing}
    end
  end

  defp mark_draining(reason) do
    now = System.system_time(:microsecond)

    case Repo.transaction(fn ->
           Repo.query!(
             """
             UPDATE optimizations
             SET status = 'draining', stop_reason = ?, updated_at = ?
             WHERE id = ? AND status = 'optimizing'
             """,
             [reason, now, @optimization_id]
           )

           Repo.query!(
             """
             INSERT INTO domain_events(
               event_id, optimization_id, aggregate_type, aggregate_id,
               event_type, payload_json, created_at
             ) VALUES (?, ?, 'optimization', ?, 'optimization_draining', ?, ?)
             """,
             [
               Ecto.UUID.generate(),
               @optimization_id,
               @optimization_id,
               Jason.encode!(%{reason: reason}),
               now
             ]
           )
         end) do
      {:ok, _value} -> :ok
      {:error, reason} -> {:error, {:stop_policy_transition_failed, reason}}
    end
  end
end
