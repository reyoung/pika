defmodule Pika.Followup.Lifecycle do
  @moduledoc "Owns durable v2 target Follow-up requests, generator retries, delivery, and exhaustion."

  alias Pika.Agent.ConversationJournal
  alias Pika.Attempt.History
  alias Pika.FileSystem
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  @optimization_id "optimization"
  @active_statuses ~w(requested generating generator_running generated delivered target_turn_running)

  @target_contracts %{
    "baseline_verify" => %{
      work_kind: "baseline_revision",
      required_operation: "finish_baseline_verification",
      generator_role: "baseline_verify_followup"
    },
    "iteration" => %{
      work_kind: "attempt",
      required_operation: "finish_iteration",
      generator_role: "iteration_followup"
    },
    "integration" => %{
      work_kind: "attempt",
      required_operation: "finish_integration",
      generator_role: "integration_followup"
    }
  }

  @spec request(Config.t(), String.t(), String.t(), map()) :: {:ok, map()} | {:error, term()}
  def request(%Config{} = config, target_session_id, required_operation, state \\ %{})
      when is_binary(target_session_id) and is_binary(required_operation) and is_map(state) do
    with session when is_map(session) <- ConversationJournal.session(target_session_id),
         {:ok, contract} <- target_contract(session),
         :ok <- require_operation(contract, required_operation),
         {:ok, policy} <- policy(config, session.role, contract.generator_role),
         {:ok, request} <- reserve_request(session, required_operation, policy),
         :ok <- materialize(request, state),
         {:ok, request} <- activate_or_exhaust(request) do
      {:ok, request}
    else
      nil -> {:error, :target_session_not_found}
      {:error, _reason} = error -> error
    end
  end

  @spec project_work() :: [map()]
  def project_work do
    Repo.query!(
      """
      SELECT id FROM followup_requests
      WHERE optimization_id = ? AND status IN ('generating', 'generator_running')
      ORDER BY id
      """,
      [@optimization_id]
    ).rows
    |> List.flatten()
    |> Enum.map(fn id ->
      request = fetch!(id)

      %{
        role_id: request.generator_role,
        work_kind: generator_work_kind(request.generator_role),
        work_id: to_string(request.id),
        request: request
      }
    end)
  end

  @spec start_generator(pos_integer()) :: {:ok, map()} | {:error, term()}
  def start_generator(request_id) do
    now = now_us()

    case Repo.query!(
           """
           UPDATE followup_requests
           SET status = 'generator_running',
               generator_attempt_sequence = generator_attempt_sequence + 1,
               updated_at = ?
           WHERE id = ? AND status = 'generating'
             AND generator_attempt_sequence < generator_max_attempts
           """,
           [now, request_id]
         ).num_rows do
      1 -> {:ok, fetch!(request_id)}
      0 -> {:error, {:followup_generator_not_startable, request_id}}
    end
  end

  @spec submit_message(pos_integer(), String.t()) :: {:ok, map()} | {:error, term()}
  def submit_message(request_id, message) when is_binary(message) do
    message = String.trim(message)

    cond do
      message == "" ->
        {:error, :followup_message_required}

      byte_size(message) > 16 * 1024 ->
        {:error, :followup_message_too_large}

      true ->
        now = now_us()

        case Repo.query!(
               """
               UPDATE followup_requests
               SET status = 'generated', message = ?, failure_reason = NULL, updated_at = ?
               WHERE id = ? AND status = 'generator_running'
               """,
               [message, now, request_id]
             ).num_rows do
          1 -> {:ok, fetch!(request_id)}
          0 -> {:error, {:followup_generator_not_running, request_id}}
        end
    end
  end

  @spec generator_failed(pos_integer(), term()) :: {:ok, map()} | {:error, term()}
  def generator_failed(request_id, reason) do
    now = now_us()

    case Repo.transaction(fn ->
           request = fetch!(request_id)

           if request.status != "generator_running" do
             Repo.rollback({:followup_generator_not_running, request_id})
           end

           if request.generator_attempt_sequence < request.generator_max_attempts do
             Repo.query!(
               """
               UPDATE followup_requests
               SET status = 'generating', failure_reason = ?, updated_at = ?
               WHERE id = ? AND status = 'generator_running'
               """,
               [inspect(reason), now, request_id]
             )

             append_event(
               request,
               "followup_generator_retry_requested",
               %{reason: inspect(reason)},
               now
             )

             fetch!(request_id)
           else
             exhaust(request, "follow_up_generator_exhausted", now)
           end
         end) do
      {:ok, request} ->
        maybe_record_exhaustion(request)
        {:ok, request}

      {:error, reason} ->
        {:error, reason}
    end
  end

  @spec deliver(pos_integer()) :: {:ok, map()} | {:error, term()}
  def deliver(request_id) do
    now = now_us()

    case Repo.transaction(fn ->
           request = fetch!(request_id)

           if request.status != "generated" or not is_binary(request.message) do
             Repo.rollback({:followup_not_deliverable, request_id})
           end

           case ConversationJournal.resume_followup(request.target_session_id) do
             {:ok, _session} -> :ok
             {:error, reason} -> Repo.rollback(reason)
           end

           Repo.query!(
             """
             UPDATE followup_requests
             SET status = 'delivered', delivered_at = ?, updated_at = ?
             WHERE id = ? AND status = 'generated'
             """,
             [now, now, request_id]
           )

           append_event(request, "followup_delivered", %{}, now)
           fetch!(request_id)
         end) do
      {:ok, request} -> {:ok, request}
      {:error, reason} -> {:error, {:followup_delivery_failed, reason}}
    end
  end

  @spec target_turn_started(pos_integer()) :: {:ok, map()} | {:error, term()}
  def target_turn_started(request_id) do
    now = now_us()

    case Repo.query!(
           "UPDATE followup_requests SET status = 'target_turn_running', updated_at = ? WHERE id = ? AND status = 'delivered'",
           [now, request_id]
         ).num_rows do
      1 -> {:ok, fetch!(request_id)}
      0 -> {:error, {:followup_not_delivered, request_id}}
    end
  end

  @spec target_turn_terminal(pos_integer()) :: {:ok, map()} | {:error, term()}
  def target_turn_terminal(request_id) do
    finish_target_request(request_id, "target_terminal")
  end

  @spec target_turn_incomplete(pos_integer(), Config.t(), map()) ::
          {:ok, map()} | {:error, term()}
  def target_turn_incomplete(request_id, %Config{} = config, state \\ %{}) do
    with {:ok, completed} <- finish_target_request(request_id, "target_incomplete"),
         {:ok, next} <-
           request(
             config,
             completed.target_session_id,
             completed.required_operation,
             state
           ) do
      {:ok, next}
    end
  end

  @spec fetch(pos_integer()) :: {:ok, map()} | {:error, term()}
  def fetch(request_id) do
    case request_row(request_id) do
      nil -> {:error, {:followup_request_not_found, request_id}}
      request -> {:ok, request}
    end
  end

  @spec active_for_target(String.t(), atom() | String.t(), String.t()) :: map() | nil
  def active_for_target(role, work_kind, work_id) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    case Repo.query!(
           """
           SELECT id FROM followup_requests
           WHERE optimization_id = ? AND target_role = ? AND target_work_kind = ?
             AND target_work_id = ? AND status IN (#{placeholders})
           ORDER BY id LIMIT 1
           """,
           [@optimization_id, role, to_string(work_kind), work_id | @active_statuses]
         ).rows do
      [[id]] -> fetch!(id)
      [] -> nil
    end
  end

  @spec retarget(pos_integer(), String.t()) :: {:ok, map()} | {:error, term()}
  def retarget(request_id, new_session_id) do
    now = now_us()

    case Repo.transaction(fn ->
           request = fetch!(request_id)

           if request.status not in @active_statuses do
             Repo.rollback({:followup_not_retargetable, request.status})
           end

           status =
             if request.status in ["delivered", "target_turn_running"],
               do: "generated",
               else: request.status

           Repo.query!(
             """
             UPDATE followup_requests
             SET target_session_id = ?, status = ?, delivered_at = NULL, updated_at = ?
             WHERE id = ?
             """,
             [new_session_id, status, now, request_id]
           )

           case ConversationJournal.await_followup(
                  new_session_id,
                  "recovered_while_followup_pending"
                ) do
             {:ok, _session} -> :ok
             {:error, reason} -> Repo.rollback(reason)
           end

           append_event(request, "followup_retargeted", %{session_id: new_session_id}, now)
           fetch!(request_id)
         end) do
      {:ok, request} -> {:ok, request}
      {:error, reason} -> {:error, {:followup_retarget_failed, reason}}
    end
  end

  @spec workdir(map() | pos_integer()) :: Path.t()
  def workdir(request_id) when is_integer(request_id), do: request_id |> fetch!() |> workdir()

  def workdir(request) when is_map(request) do
    Path.join(Persistence.current().workspace_canonical_path, request.relative_directory)
  end

  defp reserve_request(session, required_operation, policy) do
    now = now_us()

    case Repo.transaction(fn ->
           if active_request(session.role, session.work_kind, session.work_id) do
             Repo.rollback(:followup_request_already_active)
           end

           [[sequence]] =
             Repo.query!(
               """
               SELECT COALESCE(MAX(target_followup_sequence), 0) + 1
               FROM followup_requests
               WHERE optimization_id = ? AND target_role = ?
                 AND target_work_kind = ? AND target_work_id = ?
               """,
               [@optimization_id, session.role, session.work_kind, session.work_id]
             ).rows

           status =
             cond do
               sequence > policy.target_max_followups -> "requested"
               policy.generator_role == nil -> "generated"
               true -> "generating"
             end

           message = if policy.generator_role == nil, do: "继续", else: nil

           relative_directory =
             Path.join([
               "follow-ups",
               session.role,
               safe_segment(session.work_id),
               sequence |> Integer.to_string() |> String.pad_leading(6, "0")
             ])

           Repo.query!(
             """
             INSERT INTO followup_requests(
               optimization_id, target_role, target_work_kind, target_work_id,
               target_session_id, target_followup_sequence, generator_attempt_sequence,
               target_max_followups, generator_max_attempts, generator_role,
               required_operation, status, message, relative_directory,
               created_at, updated_at
             ) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)
             """,
             [
               @optimization_id,
               session.role,
               session.work_kind,
               session.work_id,
               session.id,
               sequence,
               policy.target_max_followups,
               policy.generator_max_attempts,
               policy.generator_role,
               required_operation,
               status,
               message,
               relative_directory,
               now,
               now
             ]
           )

           [[request_id]] = Repo.query!("SELECT last_insert_rowid()").rows

           case ConversationJournal.await_followup(
                  session.id,
                  "completed_without_#{required_operation}"
                ) do
             {:ok, _session} -> :ok
             {:error, reason} -> Repo.rollback(reason)
           end

           request = fetch!(request_id)
           append_event(request, "followup_requested", %{}, now)
           request
         end) do
      {:ok, request} -> {:ok, request}
      {:error, reason} -> {:error, {:followup_reservation_failed, reason}}
    end
  end

  defp materialize(request, state) do
    root = workdir(request)

    turns =
      ConversationJournal.work_turns(
        request.target_role,
        request.target_work_kind,
        request.target_work_id
      )

    messages =
      Enum.map_join(turns, "", fn turn ->
        Jason.encode!(%{
          turn: turn.turn,
          session_sequence: turn.session_sequence,
          session_id: turn.session_id,
          started_at: turn.started_at,
          input_messages: turn.input_messages,
          output_messages: turn.output_messages,
          mcp_calls: turn.mcp_calls,
          ended_reason: turn.ended_reason || if(turn.partial, do: "interrupted", else: nil)
        }) <> "\n"
      end)

    state =
      Map.merge(state, %{
        schema_version: 1,
        target_role: request.target_role,
        target_work_kind: request.target_work_kind,
        target_work_id: request.target_work_id,
        target_session_id: request.target_session_id,
        required_operation: request.required_operation,
        target_followups_used: request.target_followup_sequence,
        target_followups_remaining:
          max(request.target_max_followups - request.target_followup_sequence, 0),
        generator_attempts_used: request.generator_attempt_sequence,
        generator_max_attempts: request.generator_max_attempts
      })

    with :ok <- File.mkdir_p(root),
         :ok <- FileSystem.atomic_write(Path.join(root, "messages.jsonl"), messages),
         :ok <-
           FileSystem.atomic_write(
             Path.join(root, "state.json"),
             Jason.encode!(state, pretty: true)
           ),
         :ok <-
           FileSystem.freeze_files([
             Path.join(root, "messages.jsonl"),
             Path.join(root, "state.json")
           ]) do
      :ok
    else
      {:error, reason} -> {:error, {:followup_materialization_failed, reason}}
    end
  end

  defp activate_or_exhaust(request) do
    if request.target_followup_sequence > request.target_max_followups do
      now = now_us()

      case Repo.transaction(fn -> exhaust(request, "follow_up_exhausted", now) end) do
        {:ok, exhausted} ->
          maybe_record_exhaustion(exhausted)
          {:ok, exhausted}

        {:error, reason} ->
          {:error, {:followup_exhaustion_failed, reason}}
      end
    else
      {:ok, request}
    end
  end

  defp exhaust(request, reason, now) do
    Repo.query!(
      """
      UPDATE followup_requests
      SET status = 'exhausted', failure_reason = ?, updated_at = ?, completed_at = ?
      WHERE id = ? AND status IN ('requested', 'generating', 'generator_running')
      """,
      [reason, now, now, request.id]
    )

    Repo.query!(
      """
      UPDATE agent_sessions
      SET status = 'failed', ended_reason = ?, ended_at = ?
      WHERE id = ? AND status IN ('running', 'awaiting_report', 'awaiting_followup')
      """,
      [reason, now, request.target_session_id]
    )

    apply_target_exhaustion(request, reason, now)
    append_event(request, "followup_exhausted", %{reason: reason}, now)
    fetch!(request.id)
  end

  defp apply_target_exhaustion(%{target_role: "baseline_verify"} = request, reason, now) do
    Repo.query!(
      "UPDATE baseline_revisions SET status = 'failed', terminal_reason = ?, updated_at = ? WHERE id = ? AND status = 'verifying'",
      [reason, now, integer_id(request.target_work_id)]
    )

    Repo.query!(
      "UPDATE optimizations SET status = 'failed', stop_reason = ?, updated_at = ? WHERE id = ?",
      [reason, now, @optimization_id]
    )
  end

  defp apply_target_exhaustion(%{target_role: role} = request, reason, now)
       when role in ["iteration", "integration"] do
    attempt_id = integer_id(request.target_work_id)

    Repo.query!(
      """
      UPDATE attempts
      SET status = 'rejected', outcome = 'rejected', failure_reason = ?,
          summary = COALESCE(summary, ?), updated_at = ?
      WHERE id = ? AND status NOT IN ('accepted', 'rejected', 'cancelled')
      """,
      [reason, reason, now, attempt_id]
    )

    if role == "integration" do
      Repo.query!(
        "UPDATE integration_runs SET status = 'rejected', outcome = 'rejected', updated_at = ? WHERE attempt_id = ? AND status NOT IN ('accepted', 'rejected')",
        [now, attempt_id]
      )

      Repo.query!(
        "UPDATE operation_intents SET state = 'aborted', updated_at = ? WHERE owner_type = 'attempt' AND owner_id = ? AND state = 'pending'",
        [now, to_string(attempt_id)]
      )
    end
  end

  defp maybe_record_exhaustion(%{status: "exhausted", target_role: role} = request)
       when role in ["iteration", "integration"] do
    attempt_id = integer_id(request.target_work_id)
    workspace = Persistence.current().workspace_canonical_path
    name = attempt_id |> Integer.to_string() |> String.pad_leading(6, "0")
    root = Path.join([workspace, "attempts", name])

    History.record(root, attempt_id, %{
      attempt_id: attempt_id,
      summary: "follow_up_exhausted",
      status: "rejected",
      outcome: "rejected",
      failure_reason: request.failure_reason
    })
  end

  defp maybe_record_exhaustion(_request), do: :ok

  defp finish_target_request(request_id, status) do
    now = now_us()

    case Repo.query!(
           """
           UPDATE followup_requests
           SET status = ?, updated_at = ?, completed_at = ?
           WHERE id = ? AND status = 'target_turn_running'
           """,
           [status, now, now, request_id]
         ).num_rows do
      1 -> {:ok, fetch!(request_id)}
      0 -> {:error, {:followup_target_turn_not_running, request_id}}
    end
  end

  defp target_contract(session) do
    case Map.fetch(@target_contracts, session.role) do
      {:ok, %{work_kind: work_kind} = contract} when work_kind == session.work_kind ->
        {:ok, contract}

      {:ok, contract} ->
        {:error, {:followup_target_work_kind_mismatch, contract.work_kind, session.work_kind}}

      :error ->
        {:error, {:followup_unsupported_target_role, session.role}}
    end
  end

  defp require_operation(%{required_operation: operation}, operation), do: :ok

  defp require_operation(contract, operation),
    do: {:error, {:followup_required_operation_mismatch, contract.required_operation, operation}}

  defp policy(config, "baseline_verify", generator_role),
    do:
      followup_policy(
        config.baseline_verify.max_followups,
        config.baseline_verify_followup,
        generator_role
      )

  defp policy(config, "iteration", generator_role),
    do: followup_policy(config.iteration.max_followups, config.iteration_followup, generator_role)

  defp policy(config, "integration", generator_role),
    do:
      followup_policy(
        config.integration.max_followups,
        config.integration_followup,
        generator_role
      )

  defp followup_policy(target_max, nil, _generator_role) do
    {:ok, %{target_max_followups: target_max, generator_max_attempts: 0, generator_role: nil}}
  end

  defp followup_policy(target_max, followup, generator_role) do
    {:ok,
     %{
       target_max_followups: target_max,
       generator_max_attempts: followup.generator_max_attempts,
       generator_role: generator_role
     }}
  end

  defp active_request(role, work_kind, work_id) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    Repo.query!(
      """
      SELECT id FROM followup_requests
      WHERE optimization_id = ? AND target_role = ? AND target_work_kind = ?
        AND target_work_id = ? AND status IN (#{placeholders})
      LIMIT 1
      """,
      [@optimization_id, role, work_kind, work_id | @active_statuses]
    ).rows != []
  end

  defp fetch!(id) do
    case request_row(id) do
      nil -> raise "Follow-up Request #{id} is missing"
      request -> request
    end
  end

  defp request_row(id) do
    case Repo.query!(
           """
           SELECT id, target_role, target_work_kind, target_work_id, target_session_id,
                  target_followup_sequence, generator_attempt_sequence,
                  target_max_followups, generator_max_attempts, generator_role,
                  required_operation, status, message, relative_directory, failure_reason,
                  created_at, updated_at, delivered_at, completed_at
           FROM followup_requests WHERE id = ?
           """,
           [id]
         ).rows do
      [
        [
          id,
          target_role,
          target_work_kind,
          target_work_id,
          target_session_id,
          target_followup_sequence,
          generator_attempt_sequence,
          target_max_followups,
          generator_max_attempts,
          generator_role,
          required_operation,
          status,
          message,
          relative_directory,
          failure_reason,
          created_at,
          updated_at,
          delivered_at,
          completed_at
        ]
      ] ->
        %{
          id: id,
          target_role: target_role,
          target_work_kind: target_work_kind,
          target_work_id: target_work_id,
          target_session_id: target_session_id,
          target_followup_sequence: target_followup_sequence,
          generator_attempt_sequence: generator_attempt_sequence,
          target_max_followups: target_max_followups,
          generator_max_attempts: generator_max_attempts,
          generator_role: generator_role,
          required_operation: required_operation,
          status: status,
          message: message,
          relative_directory: relative_directory,
          failure_reason: failure_reason,
          created_at: created_at,
          updated_at: updated_at,
          delivered_at: delivered_at,
          completed_at: completed_at
        }

      [] ->
        nil
    end
  end

  defp append_event(request, event_type, payload, now) do
    Repo.query!(
      """
      INSERT INTO domain_events(
        event_id, optimization_id, aggregate_type, aggregate_id,
        event_type, payload_json, created_at
      ) VALUES (?, ?, 'followup_request', ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        @optimization_id,
        to_string(request.id),
        event_type,
        Jason.encode!(payload),
        now
      ]
    )
  end

  defp safe_segment(value) do
    if Regex.match?(~r/^[A-Za-z0-9_.-]+$/, value),
      do: value,
      else: Base.url_encode64(value, padding: false)
  end

  defp generator_work_kind("baseline_verify_followup"), do: :baseline_followup_request
  defp generator_work_kind("iteration_followup"), do: :attempt_followup_request
  defp generator_work_kind("integration_followup"), do: :integration_followup_request

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} -> id
      _other -> raise "invalid integer Work ID: #{inspect(value)}"
    end
  end

  defp now_us, do: System.system_time(:microsecond)
end
