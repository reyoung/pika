defmodule Pika.Attempt.Scheduler do
  @moduledoc "Allocates integer Attempts, applies Iteration capacity, and owns stale FIFO refresh."

  alias Pika.Attempt.Workspace
  alias Pika.Baseline.TargetSnapshot
  alias Pika.Optimization.{Config, Persistence, StopPolicy}
  alias Pika.Repo

  @optimization_id "optimization"
  @pending_statuses ~w(ready_for_integration refreshing_iteration integrating)

  @spec spawn_available(Config.t()) :: {:ok, [map()]} | {:error, term()}
  def spawn_available(%Config{} = config) do
    with :ok <- require_optimizing(),
         {:ok, context} <- base_context(),
         slots <-
           available_slots(length(config.iteration.agents))
           |> limit_by_stop_policy(),
         true <- spawn_allowed?(config.iteration.max_pending_attempts) do
      spawn_slots(slots, config, context, [])
    else
      false -> {:ok, []}
      {:error, _reason} = error -> error
    end
  end

  @spec project_work() :: [map()]
  def project_work do
    Repo.query!(
      """
      SELECT id, status, slot_index, current_iteration_round, base_sha,
             sampling_revision_id, guidance_revision_id, work_relative_path, branch
      FROM attempts
      WHERE optimization_id = ? AND status IN ('iterating', 'refreshing_iteration')
      ORDER BY slot_index, id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn row ->
      attempt = attempt(row)

      %{
        role_id: "iteration",
        work_kind: :attempt,
        work_id: to_string(attempt.id),
        attempt: attempt
      }
    end)
  end

  @spec next_queue_action() :: {:none | :waiting, map() | nil} | {:refresh | :integrate, map()}
  def next_queue_action do
    case queue_head() do
      nil ->
        {:none, nil}

      %{status: status} = attempt when status in ["refreshing_iteration", "integrating"] ->
        {:waiting, attempt}

      %{status: "ready_for_integration"} = attempt ->
        {:ok, best} = current_best()

        if attempt.base_sha == best.sha do
          {:integrate, attempt}
        else
          {:ok, refreshed} = refresh_attempt(attempt, best)
          {:refresh, refreshed}
        end
    end
  end

  @spec pending_count() :: non_neg_integer()
  def pending_count do
    placeholders = Enum.map_join(@pending_statuses, ",", fn _ -> "?" end)

    [[count]] =
      Repo.query!(
        "SELECT COUNT(*) FROM attempts WHERE optimization_id = ? AND status IN (#{placeholders})",
        [@optimization_id | @pending_statuses]
      ).rows

    count
  end

  @spec add_guidance(String.t()) :: {:ok, map()} | {:error, term()}
  def add_guidance(body) when is_binary(body) do
    body = String.trim(body)

    if body == "" do
      {:error, :guidance_body_required}
    else
      now = now_us()

      case Repo.transaction(fn ->
             [[sequence]] =
               Repo.query!(
                 "SELECT COALESCE(MAX(sequence), -1) + 1 FROM guidance_revisions WHERE optimization_id = ?",
                 [@optimization_id]
               ).rows

             Repo.query!(
               """
               INSERT INTO guidance_revisions(optimization_id, sequence, body, created_at)
               VALUES (?, ?, ?, ?)
               """,
               [@optimization_id, sequence, body, now]
             )

             [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
             %{id: id, sequence: sequence, body: body, created_at: now}
           end) do
        {:ok, guidance} -> {:ok, guidance}
        {:error, reason} -> {:error, {:guidance_creation_failed, reason}}
      end
    end
  end

  defp spawn_slots([], _config, _context, attempts), do: {:ok, Enum.reverse(attempts)}

  defp spawn_slots([slot | rest], config, context, attempts) do
    if spawn_allowed?(config.iteration.max_pending_attempts) do
      case create_attempt(slot, context, config.workspace, config.repo) do
        {:ok, attempt} -> spawn_slots(rest, config, context, [attempt | attempts])
        {:error, reason} -> {:error, reason}
      end
    else
      {:ok, Enum.reverse(attempts)}
    end
  end

  defp create_attempt(slot_index, context, workspace_root, best_repo) do
    with {:ok, attempt_id} <- Persistence.allocate_attempt_id() do
      paths = Workspace.paths(workspace_root, attempt_id)

      result =
        with :ok <- insert_preparing(attempt_id, slot_index, context, paths),
             {:ok, _paths} <-
               Workspace.prepare(
                 best_repo,
                 workspace_root,
                 attempt_id,
                 context.best.sha,
                 context.target_root
               ),
             {:ok, attempt} <- mark_iterating(attempt_id) do
          {:ok, attempt}
        end

      case result do
        {:ok, _attempt} = success ->
          success

        {:error, reason} = error ->
          reject_preparation(attempt_id, reason)
          error
      end
    end
  end

  defp insert_preparing(attempt_id, slot_index, context, paths) do
    now = now_us()

    result =
      Repo.transaction(fn ->
        Repo.query!(
          """
          INSERT INTO attempts(
            id, optimization_id, status, work_relative_path, branch, slot_index,
            base_best_revision, base_sha, sampling_revision_id, guidance_revision_id,
            current_iteration_round, inserted_at, updated_at
          ) VALUES (?, ?, 'preparing', ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
          """,
          [
            attempt_id,
            @optimization_id,
            paths.relative_root,
            paths.branch,
            slot_index,
            context.best.sequence,
            context.best.sha,
            context.sampling.id,
            context.guidance && context.guidance.id,
            now,
            now
          ]
        )

        Repo.query!(
          """
          INSERT INTO iteration_rounds(attempt_id, round, kind, base_sha, status, created_at)
          VALUES (?, 1, 'initial', ?, 'queued', ?)
          """,
          [attempt_id, context.best.sha, now]
        )

        append_event(
          "attempt",
          to_string(attempt_id),
          "attempt_preparing",
          %{slot_index: slot_index},
          now
        )
      end)

    case result do
      {:ok, _value} -> :ok
      {:error, reason} -> {:error, {:attempt_insert_failed, reason}}
    end
  end

  defp mark_iterating(attempt_id) do
    now = now_us()

    result =
      Repo.transaction(fn ->
        Repo.query!(
          "UPDATE attempts SET status = 'iterating', updated_at = ? WHERE id = ? AND status = 'preparing'",
          [now, attempt_id]
        )

        Repo.query!(
          "UPDATE iteration_rounds SET status = 'running' WHERE attempt_id = ? AND round = 1",
          [attempt_id]
        )

        append_event("attempt", to_string(attempt_id), "attempt_iterating", %{}, now)
        fetch_attempt!(attempt_id)
      end)

    case result do
      {:ok, attempt} -> {:ok, attempt}
      {:error, reason} -> {:error, {:attempt_start_failed, reason}}
    end
  end

  defp reject_preparation(attempt_id, reason) do
    Repo.query!(
      """
      UPDATE attempts
      SET status = 'rejected', failure_reason = ?, updated_at = ?
      WHERE optimization_id = ? AND id = ? AND status = 'preparing'
      """,
      [inspect(reason), now_us(), @optimization_id, attempt_id]
    )
  end

  defp refresh_attempt(attempt, best) do
    now = now_us()
    next_round = attempt.current_iteration_round + 1

    result =
      Repo.transaction(fn ->
        Repo.query!(
          """
          UPDATE attempts
          SET status = 'refreshing_iteration', base_best_revision = ?, base_sha = ?,
              current_iteration_round = ?, candidate_sha = NULL, updated_at = ?
          WHERE id = ? AND status = 'ready_for_integration'
          """,
          [best.sequence, best.sha, next_round, now, attempt.id]
        )

        Repo.query!(
          """
          INSERT INTO iteration_rounds(attempt_id, round, kind, base_sha, status, created_at)
          VALUES (?, ?, 'stale_refresh', ?, 'running', ?)
          """,
          [attempt.id, next_round, best.sha, now]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "attempt_stale_refreshing",
          %{old_base_sha: attempt.base_sha, best_sha: best.sha, round: next_round},
          now
        )

        fetch_attempt!(attempt.id)
      end)

    case result do
      {:ok, refreshed} -> {:ok, refreshed}
      {:error, reason} -> {:error, {:attempt_refresh_failed, reason}}
    end
  end

  defp base_context do
    with {:ok, best} <- current_best(),
         {:ok, sampling} <- current_sampling(),
         {:ok, target_root} <- current_target_root() do
      {:ok,
       %{best: best, sampling: sampling, guidance: current_guidance(), target_root: target_root}}
    end
  end

  defp current_best do
    case Repo.query!(
           """
           SELECT sequence, sha, baseline_revision_id
           FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[sequence, sha, baseline_revision_id]] ->
        {:ok, %{sequence: sequence, sha: sha, baseline_revision_id: baseline_revision_id}}

      [] ->
        {:error, :best_revision_missing}
    end
  end

  defp current_sampling do
    case Repo.query!(
           """
           SELECT id, sequence, baseline_revision_id
           FROM sampling_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[id, sequence, baseline_revision_id]] ->
        {:ok, %{id: id, sequence: sequence, baseline_revision_id: baseline_revision_id}}

      [] ->
        {:error, :sampling_revision_missing}
    end
  end

  defp current_guidance do
    case Repo.query!(
           """
           SELECT id, sequence, body FROM guidance_revisions
           WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[id, sequence, body]] -> %{id: id, sequence: sequence, body: body}
      [] -> nil
    end
  end

  defp current_target_root do
    case Repo.query!(
           """
           SELECT ts.id, ts.relative_path
           FROM baseline_revisions br
           JOIN target_snapshots ts ON ts.id = br.target_snapshot_id
           WHERE br.optimization_id = ? AND br.status = 'accepted'
           ORDER BY br.revision DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[snapshot_id, relative_path]] ->
        with :ok <- TargetSnapshot.verify(snapshot_id) do
          workspace = Persistence.current().workspace_canonical_path
          {:ok, Path.join(workspace, relative_path)}
        end

      [] ->
        {:error, :target_snapshot_missing}
    end
  end

  defp available_slots(concurrency) do
    used =
      Repo.query!(
        """
        SELECT slot_index FROM attempts
        WHERE optimization_id = ? AND status IN ('iterating', 'refreshing_iteration')
        """,
        [@optimization_id]
      ).rows
      |> List.flatten()
      |> MapSet.new()

    0..(concurrency - 1) |> Enum.reject(&MapSet.member?(used, &1))
  end

  defp limit_by_stop_policy(slots) do
    case StopPolicy.remaining_attempt_capacity() do
      :unlimited -> slots
      remaining -> Enum.take(slots, remaining)
    end
  end

  defp spawn_allowed?(0), do: true
  defp spawn_allowed?(limit), do: pending_count() < limit

  defp require_optimizing do
    case Persistence.current() do
      %{status: "optimizing"} -> :ok
      %{status: status} -> {:error, {:optimization_not_spawning, status}}
      nil -> {:error, :optimization_not_initialized}
    end
  end

  defp queue_head do
    case Repo.query!(
           """
           SELECT id, status, slot_index, current_iteration_round, base_sha,
                  sampling_revision_id, guidance_revision_id, work_relative_path, branch
           FROM attempts
           WHERE optimization_id = ?
             AND status IN ('ready_for_integration', 'refreshing_iteration', 'integrating')
           ORDER BY id LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [row] -> attempt(row)
      [] -> nil
    end
  end

  defp fetch_attempt!(attempt_id) do
    [row] =
      Repo.query!(
        """
        SELECT id, status, slot_index, current_iteration_round, base_sha,
               sampling_revision_id, guidance_revision_id, work_relative_path, branch
        FROM attempts WHERE id = ?
        """,
        [attempt_id]
      ).rows

    attempt(row)
  end

  defp attempt([
         id,
         status,
         slot_index,
         round,
         base_sha,
         sampling_revision_id,
         guidance_revision_id,
         work_relative_path,
         branch
       ]) do
    %{
      id: id,
      status: status,
      slot_index: slot_index,
      current_iteration_round: round,
      base_sha: base_sha,
      sampling_revision_id: sampling_revision_id,
      guidance_revision_id: guidance_revision_id,
      work_relative_path: work_relative_path,
      branch: branch
    }
  end

  defp append_event(aggregate_type, aggregate_id, event_type, payload, now) do
    Repo.query!(
      """
      INSERT INTO domain_events(
        event_id, optimization_id, aggregate_type, aggregate_id, event_type, payload_json, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        @optimization_id,
        aggregate_type,
        aggregate_id,
        event_type,
        Jason.encode!(payload),
        now
      ]
    )
  end

  defp now_us, do: System.system_time(:microsecond)
end
