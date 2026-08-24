defmodule Pika.ProgressSummary.Lifecycle do
  @moduledoc "Owns durable v2 Progress Summary scheduling, snapshots, retries, and results."

  alias Pika.FileSystem
  alias Pika.Optimization.{Config, FileContract, Persistence}
  alias Pika.ProgressSummary.Snapshot
  alias Pika.Repo

  @optimization_id "optimization"
  @active_statuses ~w(preparing requested running)
  @max_summary_bytes 64 * 1024

  @spec tick(Config.t(), DateTime.t()) :: {:ok, term()} | {:error, term()}
  def tick(config, now \\ DateTime.utc_now())

  def tick(%Config{progress_summary: nil}, %DateTime{}), do: {:ok, :disabled}

  def tick(%Config{} = config, %DateTime{} = now) do
    now_us = DateTime.to_unix(now, :microsecond)
    [[due, next_due_at]] = summary_schedule()

    cond do
      is_nil(next_due_at) ->
        next = now_us + interval_us(config)

        Repo.query!(
          "UPDATE optimizations SET progress_summary_next_due_at = ?, updated_at = ? WHERE id = ?",
          [next, now_us, @optimization_id]
        )

        {:ok, :scheduled}

      due?(due) or now_us >= next_due_at ->
        case active_request() do
          nil -> reserve_and_materialize(config, now)
          request -> coalesce_tick(request, config, now_us, next_due_at)
        end

      true ->
        {:ok, :not_due}
    end
  end

  @spec project_work() :: [map()]
  def project_work do
    case active_request() do
      nil ->
        []

      request when request.status in ["requested", "running"] ->
        [
          %{
            role_id: "progress_summary",
            work_kind: :progress_summary_request,
            work_id: to_string(request.id),
            request: request
          }
        ]

      _preparing ->
        []
    end
  end

  @spec start(pos_integer()) :: {:ok, map()} | {:error, term()}
  def start(request_id) do
    now = now_us()

    case Repo.query!(
           """
           UPDATE progress_summary_requests
           SET status = 'running', attempt_sequence = attempt_sequence + 1, updated_at = ?
           WHERE id = ? AND status = 'requested' AND attempt_sequence < max_attempts
           """,
           [now, request_id]
         ).num_rows do
      1 -> {:ok, request!(request_id)}
      0 -> {:error, {:progress_summary_not_startable, request_id}}
    end
  end

  @spec fail_attempt(pos_integer(), term()) :: {:ok, map()} | {:error, term()}
  def fail_attempt(request_id, reason) do
    now = now_us()

    case Repo.transaction(fn ->
           request = request!(request_id)

           if request.status != "running" do
             Repo.rollback({:progress_summary_not_running, request_id})
           end

           {status, event_type} =
             if request.attempt_sequence < request.max_attempts,
               do: {"requested", "progress_summary_retry_requested"},
               else: {"failed", "progress_summary_failed"}

           Repo.query!(
             """
             UPDATE progress_summary_requests
             SET status = ?, failure_reason = ?, updated_at = ?, completed_at = ?
             WHERE id = ? AND status = 'running'
             """,
             [
               status,
               inspect(reason),
               now,
               if(status == "failed", do: now, else: nil),
               request_id
             ]
           )

           append_event(to_string(request_id), event_type, %{reason: inspect(reason)}, now)
           request!(request_id)
         end) do
      {:ok, request} -> {:ok, request}
      {:error, reason} -> {:error, reason}
    end
  end

  @spec submit(pos_integer(), Path.t(), Config.t(), DateTime.t()) ::
          {:ok, map()} | {:error, term()}
  def submit(request_id, summary_path, config, now \\ DateTime.utc_now())

  def submit(request_id, "summary.md", %Config{} = config, %DateTime{} = now) do
    request = request!(request_id)
    root = request_root(request)

    with :ok <- require_running(request),
         {:ok, contents, receipt} <-
           FileContract.read(root, "summary.md", max_bytes: @max_summary_bytes),
         :ok <- validate_summary(contents),
         {:ok, summary} <- persist_summary(request, receipt, now),
         {:ok, next} <- maybe_materialize_due(config, now) do
      {:ok, %{summary: summary, next_request: next}}
    end
  end

  def submit(_request_id, _summary_path, %Config{}, %DateTime{}),
    do: {:error, :progress_summary_path_must_be_summary_md}

  @spec latest() :: map() | nil
  def latest do
    case Repo.query!(
           """
           SELECT ps.id, ps.request_id, ps.summary_relative_path, ps.summary_sha256,
                  ps.completed_at, pr.sequence
           FROM progress_summaries ps
           JOIN progress_summary_requests pr ON pr.id = ps.request_id
           WHERE ps.optimization_id = ?
           ORDER BY pr.sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[id, request_id, path, sha256, completed_at, sequence]] ->
        %{
          id: id,
          request_id: request_id,
          sequence: sequence,
          summary_relative_path: path,
          summary_sha256: sha256,
          completed_at: completed_at
        }

      [] ->
        nil
    end
  end

  @spec fetch(pos_integer()) :: {:ok, map()} | {:error, term()}
  def fetch(request_id) do
    case request(request_id) do
      nil -> {:error, {:progress_summary_request_not_found, request_id}}
      value -> {:ok, value}
    end
  end

  @spec workdir(pos_integer() | map()) :: Path.t()
  def workdir(request_id) when is_integer(request_id), do: request_id |> request!() |> workdir()

  def workdir(request) when is_map(request) do
    Path.join(Persistence.current().workspace_canonical_path, request.relative_directory)
  end

  defp reserve_and_materialize(config, now) do
    case reserve_request(config, now) do
      {:ok, request} ->
        with :ok <- materialize(request, now),
             {:ok, request} <- mark_requested(request.id) do
          {:ok, {:requested, request}}
        else
          {:error, reason} = error ->
            fail_materialization(request.id, reason)
            error
        end

      {:error, _reason} = error ->
        error
    end
  end

  defp reserve_request(config, now) do
    now_us = DateTime.to_unix(now, :microsecond)

    case Repo.transaction(fn ->
           if active_request() != nil do
             Repo.rollback(:progress_summary_request_already_active)
           end

           [[sequence]] =
             Repo.query!(
               "SELECT COALESCE(MAX(sequence), 0) + 1 FROM progress_summary_requests WHERE optimization_id = ?",
               [@optimization_id]
             ).rows

           [[cursor]] = Repo.query!("SELECT COALESCE(MAX(id), 0) FROM conversation_turns").rows

           previous_summary_id =
             case latest() do
               nil -> nil
               summary -> summary.id
             end

           relative_directory = relative_directory(config, now, sequence)

           Repo.query!(
             """
             INSERT INTO progress_summary_requests(
               optimization_id, sequence, status, attempt_sequence, max_attempts,
               snapshot_cursor, previous_summary_id, relative_directory,
               created_at, updated_at
             ) VALUES (?, ?, 'preparing', 0, ?, ?, ?, ?, ?, ?)
             """,
             [
               @optimization_id,
               sequence,
               config.progress_summary.max_followups + 1,
               cursor,
               previous_summary_id,
               relative_directory,
               now_us,
               now_us
             ]
           )

           [[request_id]] = Repo.query!("SELECT last_insert_rowid()").rows
           [[_due, next_due_at]] = summary_schedule()

           next_due_at =
             if is_nil(next_due_at) or next_due_at <= now_us,
               do: now_us + interval_us(config),
               else: next_due_at

           Repo.query!(
             """
             UPDATE optimizations
             SET progress_summary_due = 0, progress_summary_next_due_at = ?, updated_at = ?
             WHERE id = ?
             """,
             [next_due_at, now_us, @optimization_id]
           )

           append_event(
             to_string(request_id),
             "progress_summary_requested",
             %{sequence: sequence, snapshot_cursor: cursor},
             now_us
           )

           request!(request_id)
         end) do
      {:ok, request} -> {:ok, request}
      {:error, reason} -> {:error, {:progress_summary_reservation_failed, reason}}
    end
  end

  defp materialize(request, now) do
    root = request_root(request)
    previous = previous_summary(request.previous_summary_id)
    previous_cursor = if previous, do: previous.snapshot_cursor, else: 0

    with :ok <- File.mkdir_p(root),
         :ok <-
           FileSystem.atomic_write(
             Path.join(root, "status.json"),
             Snapshot.build(request.snapshot_cursor, now) |> Jason.encode!(pretty: true)
           ),
         :ok <-
           FileSystem.atomic_write(
             Path.join(root, "messages.jsonl"),
             messages_jsonl(previous_cursor, request.snapshot_cursor)
           ),
         :ok <- write_previous_summary(root, previous),
         :ok <- freeze_inputs(root, previous) do
      :ok
    else
      {:error, reason} -> {:error, {:progress_summary_materialization_failed, reason}}
    end
  end

  defp mark_requested(request_id) do
    now = now_us()

    case Repo.query!(
           "UPDATE progress_summary_requests SET status = 'requested', updated_at = ? WHERE id = ? AND status = 'preparing'",
           [now, request_id]
         ).num_rows do
      1 -> {:ok, request!(request_id)}
      0 -> {:error, :progress_summary_request_not_preparing}
    end
  end

  defp fail_materialization(request_id, reason) do
    now = now_us()

    Repo.transaction(fn ->
      Repo.query!(
        """
        UPDATE progress_summary_requests
        SET status = 'failed', failure_reason = ?, updated_at = ?, completed_at = ?
        WHERE id = ? AND status = 'preparing'
        """,
        [inspect(reason), now, now, request_id]
      )

      Repo.query!(
        "UPDATE optimizations SET progress_summary_due = 1, updated_at = ? WHERE id = ?",
        [now, @optimization_id]
      )
    end)

    :ok
  end

  defp coalesce_tick(request, config, now_us, next_due_at) do
    next_due_at =
      if now_us >= next_due_at, do: now_us + interval_us(config), else: next_due_at

    Repo.query!(
      """
      UPDATE optimizations
      SET progress_summary_due = 1, progress_summary_next_due_at = ?, updated_at = ?
      WHERE id = ?
      """,
      [next_due_at, now_us, @optimization_id]
    )

    {:ok, {:coalesced, request}}
  end

  defp persist_summary(request, receipt, now) do
    now_us = DateTime.to_unix(now, :microsecond)
    relative_path = Path.join(request.relative_directory, receipt.relative_path)

    case Repo.transaction(fn ->
           current = request!(request.id)

           if current.status != "running" do
             Repo.rollback({:progress_summary_not_running, request.id})
           end

           Repo.query!(
             """
             INSERT INTO progress_summaries(
               optimization_id, request_id, summary_relative_path, summary_sha256, completed_at
             ) VALUES (?, ?, ?, ?, ?)
             """,
             [@optimization_id, request.id, relative_path, receipt.sha256, now_us]
           )

           [[summary_id]] = Repo.query!("SELECT last_insert_rowid()").rows

           Repo.query!(
             """
             UPDATE progress_summary_requests
             SET status = 'completed', failure_reason = NULL, updated_at = ?, completed_at = ?
             WHERE id = ? AND status = 'running'
             """,
             [now_us, now_us, request.id]
           )

           append_event(
             to_string(request.id),
             "progress_summary_completed",
             %{summary_id: summary_id, summary_sha256: receipt.sha256},
             now_us
           )

           latest()
         end) do
      {:ok, summary} -> {:ok, summary}
      {:error, reason} -> {:error, {:progress_summary_submit_failed, reason}}
    end
  end

  defp maybe_materialize_due(%Config{progress_summary: nil}, _now), do: {:ok, nil}

  defp maybe_materialize_due(config, now) do
    case tick(config, now) do
      {:ok, {:requested, request}} -> {:ok, request}
      {:ok, _status} -> {:ok, nil}
      {:error, reason} -> {:error, reason}
    end
  end

  defp messages_jsonl(after_cursor, through_cursor) do
    Repo.query!(
      """
      SELECT id, role, work_kind, work_id, session_id, session_sequence, turn_sequence,
             input_messages_json, output_messages_json, mcp_calls_json,
             ended_reason, partial, started_at, ended_at
      FROM conversation_turns
      WHERE optimization_id = ? AND id > ? AND id <= ?
      ORDER BY id
      """,
      [@optimization_id, after_cursor, through_cursor]
    ).rows
    |> Enum.map_join("", fn [
                              id,
                              role,
                              work_kind,
                              work_id,
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
                            ] ->
      Jason.encode!(%{
        cursor: id,
        role: role,
        work_kind: work_kind,
        work_id: work_id,
        session_id: session_id,
        session_sequence: session_sequence,
        turn: turn_sequence,
        input_messages: Jason.decode!(input),
        output_messages: Jason.decode!(output),
        mcp_calls: Jason.decode!(mcp_calls),
        ended_reason: ended_reason,
        partial: truthy?(partial),
        started_at: started_at,
        ended_at: ended_at
      }) <> "\n"
    end)
  end

  defp write_previous_summary(_root, nil), do: :ok

  defp write_previous_summary(root, previous) do
    workspace = Persistence.current().workspace_canonical_path

    case File.read(Path.join(workspace, previous.summary_relative_path)) do
      {:ok, contents} -> FileSystem.atomic_write(Path.join(root, "previous-summary.md"), contents)
      {:error, reason} -> {:error, {:previous_summary_unreadable, reason}}
    end
  end

  defp freeze_inputs(root, previous) do
    paths = [Path.join(root, "status.json"), Path.join(root, "messages.jsonl")]
    paths = if previous, do: [Path.join(root, "previous-summary.md") | paths], else: paths
    FileSystem.freeze_files(paths)
  end

  defp previous_summary(nil), do: nil

  defp previous_summary(id) do
    case Repo.query!(
           """
           SELECT ps.id, ps.summary_relative_path, pr.snapshot_cursor
           FROM progress_summaries ps
           JOIN progress_summary_requests pr ON pr.id = ps.request_id
           WHERE ps.id = ?
           """,
           [id]
         ).rows do
      [[id, path, cursor]] ->
        %{id: id, summary_relative_path: path, snapshot_cursor: cursor}

      [] ->
        nil
    end
  end

  defp active_request do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    case Repo.query!(
           "SELECT id FROM progress_summary_requests WHERE optimization_id = ? AND status IN (#{placeholders}) ORDER BY sequence LIMIT 1",
           [@optimization_id | @active_statuses]
         ).rows do
      [[id]] -> request!(id)
      [] -> nil
    end
  end

  defp request!(id) do
    case request(id) do
      nil -> raise "Progress Summary Request #{id} is missing"
      request -> request
    end
  end

  defp request(id) do
    case Repo.query!(
           """
           SELECT id, sequence, status, attempt_sequence, max_attempts, snapshot_cursor,
                  previous_summary_id, relative_directory, failure_reason,
                  created_at, updated_at, completed_at
           FROM progress_summary_requests WHERE id = ?
           """,
           [id]
         ).rows do
      [
        [
          id,
          sequence,
          status,
          attempt_sequence,
          max_attempts,
          cursor,
          previous_summary_id,
          directory,
          failure_reason,
          created_at,
          updated_at,
          completed_at
        ]
      ] ->
        %{
          id: id,
          sequence: sequence,
          status: status,
          attempt_sequence: attempt_sequence,
          max_attempts: max_attempts,
          snapshot_cursor: cursor,
          previous_summary_id: previous_summary_id,
          relative_directory: directory,
          failure_reason: failure_reason,
          created_at: created_at,
          updated_at: updated_at,
          completed_at: completed_at
        }

      [] ->
        nil
    end
  end

  defp relative_directory(config, now, sequence) do
    {:ok, local} =
      DateTime.shift_zone(now, config.progress_summary.time_zone, Tz.TimeZoneDatabase)

    date = Date.to_iso8601(DateTime.to_date(local))
    time = Calendar.strftime(local, "%H%M%S")
    suffix = sequence |> Integer.to_string() |> String.pad_leading(8, "0")
    Path.join(["progress-summaries", date, "#{time}-#{suffix}"])
  end

  defp request_root(request) do
    workdir(request)
  end

  defp summary_schedule do
    Repo.query!(
      "SELECT progress_summary_due, progress_summary_next_due_at FROM optimizations WHERE id = ?",
      [@optimization_id]
    ).rows
  end

  defp require_running(%{status: "running"}), do: :ok
  defp require_running(request), do: {:error, {:progress_summary_not_running, request.id}}

  defp validate_summary(contents) do
    cond do
      not String.valid?(contents) -> {:error, :progress_summary_not_utf8}
      String.trim(contents) == "" -> {:error, :progress_summary_empty}
      true -> :ok
    end
  end

  defp append_event(request_id, event_type, payload, now) do
    Repo.query!(
      """
      INSERT INTO domain_events(
        event_id, optimization_id, aggregate_type, aggregate_id,
        event_type, payload_json, created_at
      ) VALUES (?, ?, 'progress_summary_request', ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        @optimization_id,
        request_id,
        event_type,
        Jason.encode!(payload),
        now
      ]
    )
  end

  defp interval_us(config), do: config.progress_summary.interval_ms * 1_000
  defp due?(value), do: value in [1, true, "1", "true"]
  defp truthy?(value), do: value in [1, true, "1", "true"]
  defp now_us, do: System.system_time(:microsecond)
end
