defmodule Pika.AttemptStore do
  @moduledoc false

  alias Pika.Repo

  @active_statuses ~w(queued running awaiting_report refreshing integrating interrupted)
  @terminal_statuses ~w(accepted rejected cancelled)

  def campaign_context(campaign_id) do
    with {:ok, campaign} <- campaign(campaign_id),
         {:ok, spec} <- current_spec(campaign.current_spec_revision_id),
         {:ok, sampling} <- current_sampling(campaign_id, spec.id) do
      {:ok,
       Map.merge(campaign, %{
         spec_revision: spec,
         sampling_revision: sampling,
         cases: cases(spec.id),
         sampled_case_ids: sampled_case_ids(sampling.id),
         metrics: metrics(spec.id),
         best_metrics: best_metrics(campaign_id),
         target_snapshot: target_snapshot(spec.target_snapshot_id),
         references: Jason.decode!(spec.reference_snapshot_json),
         skill: Jason.decode!(spec.skill_snapshot_json),
         protected_paths: Jason.decode!(spec.protected_paths_json),
         spec: Jason.decode!(spec.spec_json)
       })}
    end
  rescue
    error -> {:error, {:campaign_context_failed, Exception.message(error)}}
  end

  def campaign_spec_overview(campaign_id) do
    with {:ok, campaign} <- campaign(campaign_id),
         {:ok, spec} <- current_spec(campaign.current_spec_revision_id),
         {:ok, sampling} <- current_sampling(campaign_id, spec.id) do
      spec_snapshot = Jason.decode!(spec.spec_json)

      {:ok,
       %{
         revision: spec.revision,
         cases: List.wrap(spec_snapshot["benchmark_cases"]),
         metrics: List.wrap(spec_snapshot["metrics"]),
         sampling_revision: sampling,
         sampled_case_ids: sampled_case_ids(sampling.id),
         protected_paths: Jason.decode!(spec.protected_paths_json)
       }}
    end
  rescue
    error -> {:error, {:campaign_spec_overview_failed, Exception.message(error)}}
  end

  def dispatch_state(campaign_id) do
    case Repo.query!(
           "SELECT status, dispatch_gate FROM campaigns WHERE id = ?",
           [campaign_id]
         ).rows do
      [[status, dispatch_gate]] -> {:ok, %{status: status, dispatch_gate: dispatch_gate}}
      [] -> {:error, :campaign_not_found}
    end
  rescue
    error -> {:error, {:dispatch_state_failed, Exception.message(error)}}
  end

  def create_attempt(campaign_id, slot_index) do
    id = Ecto.UUID.generate()
    now = now_us()
    branch = "pika/attempt/#{id}"
    worktree = "attempts/#{id}"

    transaction =
      Repo.transaction(fn ->
        campaign = campaign_row!(campaign_id)

        cond do
          campaign.status != "optimizing" ->
            Repo.rollback({:invalid_campaign_state, campaign.status})

          campaign.dispatch_gate not in [nil, ""] ->
            Repo.rollback({:dispatch_closed, campaign.dispatch_gate})

          campaign.max_attempts && campaign.attempts_created >= campaign.max_attempts ->
            Repo.rollback(:attempt_budget_exhausted)

          slot_busy?(campaign_id, slot_index) ->
            Repo.rollback({:slot_busy, slot_index})

          true ->
            spec_id = campaign.current_spec_revision_id
            sampling = current_sampling_row!(campaign_id, spec_id)
            ordinal = campaign.attempts_created + 1

            Repo.query!(
              """
              INSERT INTO attempts(
                id, campaign_id, ordinal, spec_revision_id, slot_index,
                sampling_revision_id, status, base_sha, branch_name,
                worktree_relative_path, created_at, lock_version
              ) VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, 1)
              """,
              [
                id,
                campaign_id,
                ordinal,
                spec_id,
                slot_index,
                sampling.id,
                campaign.best_sha,
                branch,
                worktree,
                now
              ]
            )

            Repo.query!(
              "UPDATE campaigns SET attempts_created = ?, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
              [ordinal, now, campaign_id]
            )

            event =
              insert_event!(campaign_id, "attempt", id, "attempt_created", %{
                ordinal: ordinal,
                slot_index: slot_index,
                sampling_revision_id: sampling.id,
                base_sha: campaign.best_sha
              })

            {attempt_row!(id), event}
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:attempt_create_failed, Exception.message(error)}}
  end

  def mark_running(attempt_id) do
    transition_attempt(
      attempt_id,
      ~w(queued interrupted awaiting_report),
      "running",
      "attempt_running",
      %{},
      started_at: now_us(),
      resume_state: nil
    )
  end

  def mark_awaiting_report(attempt_id, missing) do
    transition_attempt(
      attempt_id,
      ~w(running awaiting_report),
      "awaiting_report",
      "attempt_awaiting_report",
      %{missing: missing}
    )
  end

  def mark_interrupted(attempt_id, reason) do
    attempt = attempt_row!(attempt_id)

    transition_attempt(
      attempt_id,
      ~w(running awaiting_report refreshing integrating interrupted),
      "interrupted",
      "attempt_interrupted",
      %{reason: inspect(reason)},
      resume_state:
        if(attempt.status == "interrupted", do: attempt.resume_state, else: attempt.status)
    )
  rescue
    error -> {:error, {:attempt_interrupt_failed, Exception.message(error)}}
  end

  def mark_cancelled(attempt_id, reason) do
    transition_attempt(
      attempt_id,
      @active_statuses ++ ["ready_for_integration"],
      "cancelled",
      "attempt_cancelled",
      %{reason: reason},
      outcome_reason: reason,
      completed_at: now_us()
    )
  end

  def update_candidate(attempt_id, candidate_sha) do
    Repo.query!("UPDATE attempts SET candidate_sha = ? WHERE id = ?", [candidate_sha, attempt_id])
    {:ok, attempt_row!(attempt_id)}
  rescue
    error -> {:error, {:candidate_update_failed, Exception.message(error)}}
  end

  def insert_session(campaign_id, identity, session, profile, required) do
    now = now_us()

    Repo.query!(
      """
      INSERT INTO agent_sessions(
        id, campaign_id, attempt_id, sync_run_id, role, slot_index, profile_json, backend,
        backend_protocol, provider_session_id, backend_capabilities_json, model,
        reasoning_effort, status, process_pid, process_started_at, mcp_token_hash,
        required_operations_json, last_turn_sequence, last_event_seq, started_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, 0, 0, ?)
      ON CONFLICT(id) DO UPDATE SET
        status = 'running', process_pid = excluded.process_pid,
        process_started_at = excluded.process_started_at,
        required_operations_json = excluded.required_operations_json,
        ended_at = NULL
      """,
      [
        session.id,
        campaign_id,
        Map.get(identity, :attempt_id),
        Map.get(identity, :sync_run_id),
        Atom.to_string(identity.role),
        identity.slot_index,
        Jason.encode!(Pika.JSONSafe.json_safe(profile)),
        to_string(session.backend),
        session.backend_protocol,
        session.backend_session_id,
        Jason.encode!(%{}),
        session.model,
        to_string(session.reasoning_effort),
        inspect(self()),
        now,
        identity.token_hash,
        Jason.encode!(Enum.sort(required)),
        now
      ]
    )

    :ok
  rescue
    error -> {:error, {:agent_session_persist_failed, Exception.message(error)}}
  end

  def update_session(session_id, status, required, attrs \\ %{}) do
    ended_at = if status in ~w(completed failed stopped interrupted), do: now_us(), else: nil

    Repo.query!(
      """
      UPDATE agent_sessions SET status = ?, required_operations_json = ?,
        last_turn_sequence = last_turn_sequence + ?, last_event_seq = last_event_seq + ?,
        ended_at = COALESCE(?, ended_at)
      WHERE id = ?
      """,
      [
        status,
        Jason.encode!(Enum.sort(required)),
        Map.get(attrs, :turn_increment, 0),
        Map.get(attrs, :event_increment, 0),
        ended_at,
        session_id
      ]
    )

    :ok
  rescue
    error -> {:error, {:agent_session_update_failed, Exception.message(error)}}
  end

  def interrupt_active_sessions_for_attempt(attempt_id) do
    interrupt_active_sessions("attempt_id = ?", [attempt_id])
  end

  def interrupt_active_sessions_for_role(campaign_id, role) do
    interrupt_active_sessions("campaign_id = ? AND role = ?", [campaign_id, to_string(role)])
  end

  defp interrupt_active_sessions(where, params) do
    Repo.query!(
      """
      UPDATE agent_sessions
      SET status = 'interrupted', ended_at = COALESCE(ended_at, ?)
      WHERE #{where} AND status IN ('starting', 'running', 'awaiting_report')
      """,
      [now_us() | params]
    )

    :ok
  rescue
    error -> {:error, {:agent_session_interrupt_failed, Exception.message(error)}}
  end

  def session(session_id) do
    case Repo.query!(
           "SELECT id, campaign_id, attempt_id, role, slot_index, backend, backend_protocol, model, reasoning_effort, status, required_operations_json, started_at, ended_at FROM agent_sessions WHERE id = ?",
           [session_id]
         ).rows do
      [row] -> {:ok, session_from_row(row)}
      [] -> {:error, :session_not_found}
    end
  end

  def sessions(campaign_id) do
    Repo.query!(
      "SELECT id, campaign_id, attempt_id, role, slot_index, backend, backend_protocol, model, reasoning_effort, status, required_operations_json, started_at, ended_at FROM agent_sessions WHERE campaign_id = ? ORDER BY started_at",
      [campaign_id]
    ).rows
    |> Enum.map(&session_from_row/1)
  end

  def active_attempts(campaign_id) do
    placeholders =
      Enum.map_join(@active_statuses ++ ["ready_for_integration"], ",", fn _ -> "?" end)

    Repo.query!(
      "SELECT #{attempt_columns()} FROM attempts WHERE campaign_id = ? AND status IN (#{placeholders}) ORDER BY ordinal",
      [campaign_id | @active_statuses ++ ["ready_for_integration"]]
    ).rows
    |> Enum.map(&attempt_from_row/1)
  end

  def unverified_attempt_count(campaign_id) do
    case Repo.query!(
           """
           SELECT COUNT(*)
           FROM attempts
           WHERE campaign_id = ?
             AND status IN ('ready_for_integration', 'refreshing', 'integrating')
           """,
           [campaign_id]
         ).rows do
      [[count]] when is_integer(count) -> {:ok, count}
      _ -> {:error, :unverified_attempt_count_unavailable}
    end
  rescue
    error -> {:error, {:unverified_attempt_count_failed, Exception.message(error)}}
  end

  def attempts(campaign_id, opts \\ []) do
    limit = Keyword.get(opts, :limit, 100)
    before = Keyword.get(opts, :before_ordinal)
    outcome = Keyword.get(opts, :outcome)

    {conditions, params} =
      [{"campaign_id = ?", campaign_id}]
      |> maybe_condition(before, "ordinal < ?")
      |> maybe_condition(outcome, "status = ?")
      |> Enum.unzip()

    Repo.query!(
      "SELECT #{attempt_columns()} FROM attempts WHERE #{Enum.join(conditions, " AND ")} ORDER BY ordinal DESC LIMIT ?",
      params ++ [limit]
    ).rows
    |> Enum.map(&attempt_from_row/1)
  end

  def attempt(attempt_id) do
    case Repo.query!("SELECT #{attempt_columns()} FROM attempts WHERE id = ?", [attempt_id]).rows do
      [row] -> {:ok, attempt_from_row(row)}
      [] -> {:error, :attempt_not_found}
    end
  end

  def accepted_attempt_by_sha(campaign_id, sha) when is_binary(sha) do
    case Repo.query!(
           "SELECT #{attempt_columns()} FROM attempts WHERE accepted_sha = ? AND campaign_id = ? AND status = 'accepted' LIMIT 1",
           [sha, campaign_id]
         ).rows do
      [row] -> {:ok, attempt_from_row(row)}
      [] -> {:error, :attempt_not_found}
    end
  rescue
    error -> {:error, {:accepted_attempt_lookup_failed, Exception.message(error)}}
  end

  def record_metrics(attempt_id, metrics, candidate_sha, source \\ "iteration") do
    now = now_us()
    attempt = attempt_row!(attempt_id)
    target_snapshot_id = target_snapshot_id!(attempt.spec_revision_id)

    Repo.transaction(fn ->
      Enum.each(metrics, fn metric ->
        [[case_id]] =
          Repo.query!(
            "SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?",
            [attempt.spec_revision_id, metric.case_id]
          ).rows

        [[metric_id]] =
          Repo.query!(
            "SELECT id FROM metric_definitions WHERE spec_revision_id = ? AND name = ?",
            [attempt.spec_revision_id, metric.metric_id]
          ).rows

        Repo.query!(
          """
          INSERT INTO attempt_metrics(
            attempt_id, benchmark_case_id, metric_definition_id, measured_sha,
            value, baseline_value, improvement_ratio, mad, noise_tolerance,
            pair_count, valid_pair_count, source, measured_at, target_snapshot_id,
            target_value, target_relative_improvement, best_relative_improvement
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
          ON CONFLICT(attempt_id, benchmark_case_id, metric_definition_id) DO UPDATE SET
            measured_sha = excluded.measured_sha, value = excluded.value,
            baseline_value = excluded.baseline_value,
            improvement_ratio = excluded.improvement_ratio, mad = excluded.mad,
            noise_tolerance = excluded.noise_tolerance,
            pair_count = excluded.pair_count, valid_pair_count = excluded.valid_pair_count,
            target_snapshot_id = excluded.target_snapshot_id,
            target_value = excluded.target_value,
            target_relative_improvement = excluded.target_relative_improvement,
            best_relative_improvement = excluded.best_relative_improvement,
            source = excluded.source, measured_at = excluded.measured_at
          """,
          [
            attempt_id,
            case_id,
            metric_id,
            candidate_sha,
            metric.value,
            metric.baseline_value,
            metric.improvement_ratio,
            metric.mad,
            metric.noise_tolerance,
            metric.pair_count,
            metric.valid_pair_count,
            source,
            now,
            target_snapshot_id,
            metric.target_value,
            metric.target_relative_improvement,
            metric.best_relative_improvement
          ]
        )
      end)

      Repo.query!("UPDATE attempts SET candidate_sha = ? WHERE id = ?", [
        candidate_sha,
        attempt_id
      ])

      insert_event!(attempt.campaign_id, "attempt", attempt_id, "attempt_metrics_recorded", %{
        candidate_sha: candidate_sha,
        source: source,
        metric_count: length(metrics)
      })
    end)
    |> publish_event_result()
  rescue
    error -> {:error, {:metrics_persist_failed, Exception.message(error)}}
  end

  def metrics_for_attempt(attempt_id) do
    Repo.query!(
      """
      SELECT bc.name, md.name, md.unit, md.direction, md.role, am.measured_sha,
        am.value, am.baseline_value, am.improvement_ratio, am.mad,
        am.noise_tolerance, am.pair_count, am.valid_pair_count, am.source, am.measured_at,
        am.target_snapshot_id, am.target_value, am.target_relative_improvement,
        am.best_relative_improvement
      FROM attempt_metrics am
      JOIN benchmark_cases bc ON bc.id = am.benchmark_case_id
      JOIN metric_definitions md ON md.id = am.metric_definition_id
      WHERE am.attempt_id = ? ORDER BY bc.ordinal, md.name
      """,
      [attempt_id]
    ).rows
    |> Enum.map(fn [
                     case_id,
                     metric_id,
                     unit,
                     direction,
                     role,
                     measured_sha,
                     value,
                     baseline,
                     improvement,
                     mad,
                     noise,
                     pairs,
                     valid,
                     source,
                     measured_at,
                     target_snapshot_id,
                     target_value,
                     target_relative_improvement,
                     best_relative_improvement
                   ] ->
      %{
        case_id: case_id,
        metric_id: metric_id,
        unit: unit,
        direction: direction,
        role: role,
        measured_sha: measured_sha,
        value: value,
        baseline_value: baseline,
        improvement_ratio: improvement,
        target_snapshot_id: target_snapshot_id,
        target_value: target_value,
        target_relative_improvement: target_relative_improvement,
        best_relative_improvement: best_relative_improvement,
        mad: mad,
        noise_tolerance: noise,
        pair_count: pairs,
        valid_pair_count: valid,
        source: source,
        measured_at: measured_at
      }
    end)
  end

  def iteration_metrics_for_attempt(attempt, workspace \\ nil) do
    persisted = metrics_for_attempt(attempt.id) |> Enum.filter(&(&1.source == "iteration"))

    if (persisted == [] and workspace) && attempt.metrics_artifact_id &&
         attempt.correctness_artifact_id do
      recover_iteration_metrics(attempt, workspace)
    else
      persisted
    end
  end

  defp recover_iteration_metrics(attempt, workspace) do
    with {:ok, samples} <- artifact_by_id(attempt.metrics_artifact_id),
         {:ok, correctness} <- artifact_by_id(attempt.correctness_artifact_id),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(workspace, correctness.relative_path),
         {:ok, spec} <- current_spec(attempt.spec_revision_id),
         {:ok, result} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: attempt.base_sha,
             candidate_sha: attempt.candidate_sha,
             target_snapshot_id: spec.target_snapshot_id,
             case_ids: sampled_case_ids(attempt.sampling_revision_id),
             metrics: metrics(attempt.spec_revision_id),
             benchmark: Jason.decode!(spec.spec_json)["benchmark"],
             best_metrics: best_metrics_for_sha(attempt.campaign_id, attempt.base_sha)
           }) do
      result
    else
      _ -> []
    end
  end

  defp artifact_by_id(id) do
    case Repo.query!("SELECT relative_path FROM artifacts WHERE id = ?", [id]).rows do
      [[relative_path]] -> {:ok, %{relative_path: relative_path}}
      [] -> {:error, :artifact_not_registered}
    end
  end

  defp best_metrics_for_sha(campaign_id, sha) do
    Repo.query!(
      """
      SELECT bc.name, md.name, bm.value, bm.noise_tolerance
      FROM best_metrics bm
      JOIN best_revisions br ON br.id = bm.best_revision_id
      JOIN benchmark_cases bc ON bc.id = bm.benchmark_case_id
      JOIN metric_definitions md ON md.id = bm.metric_definition_id
      WHERE br.id = (
        SELECT id FROM best_revisions
        WHERE campaign_id = ? AND sha = ? ORDER BY sequence DESC LIMIT 1
      )
      """,
      [campaign_id, sha]
    ).rows
    |> Map.new(fn [case_id, metric_id, value, noise] ->
      {{case_id, metric_id}, %{value: value, noise_tolerance: noise}}
    end)
  end

  defp target_snapshot_id!(spec_revision_id) do
    case Repo.query!("SELECT target_snapshot_id FROM spec_revisions WHERE id = ?", [
           spec_revision_id
         ]).rows do
      [[id]] when is_binary(id) -> id
      _ -> Repo.rollback(:target_snapshot_missing)
    end
  end

  def submit_summary(attempt_id, attrs) do
    attempt = attempt_row!(attempt_id)

    Repo.transaction(fn ->
      Repo.query!(
        """
        UPDATE attempts SET description = ?, summary = ?, modification_scope_json = ?,
          risk_json = ?, profiler_summary = ?, recommended_outcome = ? WHERE id = ?
        """,
        [
          attrs.description,
          attrs.summary,
          Jason.encode!(attrs.modification_scope),
          Jason.encode!(attrs.risks),
          attrs.profiler_summary,
          attrs.recommended_outcome,
          attempt_id
        ]
      )

      insert_event!(attempt.campaign_id, "attempt", attempt_id, "attempt_summary_submitted", %{
        description: attrs.description,
        summary: attrs.summary
      })
    end)
    |> publish_event_result()
  rescue
    error -> {:error, {:summary_persist_failed, Exception.message(error)}}
  end

  def attach_artifact(attempt_id, column, artifact_id)
      when column in ~w(patch_artifact_id plan_artifact_id correctness_artifact_id metrics_artifact_id) do
    Repo.query!("UPDATE attempts SET #{column} = ? WHERE id = ?", [artifact_id, attempt_id])
    :ok
  rescue
    error -> {:error, {:artifact_attach_failed, Exception.message(error)}}
  end

  def artifact(campaign_id, relative_path) do
    case Repo.query!(
           "SELECT id, campaign_id, owner_type, owner_id, kind, relative_path, sha256, byte_size, mime_type, metadata_json, created_at FROM artifacts WHERE campaign_id = ? AND relative_path = ?",
           [campaign_id, relative_path]
         ).rows do
      [[id, campaign_id, owner_type, owner_id, kind, path, sha, size, mime, metadata, at]] ->
        {:ok,
         %{
           id: id,
           campaign_id: campaign_id,
           owner_type: owner_type,
           owner_id: owner_id,
           kind: kind,
           relative_path: path,
           sha256: sha,
           byte_size: size,
           mime_type: mime,
           metadata: Jason.decode!(metadata),
           created_at: at
         }}

      [] ->
        {:error, :artifact_not_registered}
    end
  end

  def artifacts_for_owner(campaign_id, owner_type, owner_id) do
    Repo.query!(
      "SELECT id, campaign_id, owner_type, owner_id, kind, relative_path, sha256, byte_size, mime_type, metadata_json, created_at FROM artifacts WHERE campaign_id = ? AND owner_type = ? AND owner_id = ? ORDER BY created_at, relative_path",
      [campaign_id, owner_type, owner_id]
    ).rows
    |> Enum.map(fn [
                     id,
                     campaign_id,
                     owner_type,
                     owner_id,
                     kind,
                     path,
                     sha,
                     size,
                     mime,
                     metadata,
                     at
                   ] ->
      %{
        id: id,
        campaign_id: campaign_id,
        owner_type: owner_type,
        owner_id: owner_id,
        kind: kind,
        relative_path: path,
        sha256: sha,
        byte_size: size,
        mime_type: mime,
        metadata: Jason.decode!(metadata),
        created_at: at
      }
    end)
  end

  def attach_session_log(session_id, artifact_id) do
    Repo.query!("UPDATE agent_sessions SET log_artifact_id = ? WHERE id = ?", [
      artifact_id,
      session_id
    ])

    :ok
  rescue
    error -> {:error, {:session_log_attach_failed, Exception.message(error)}}
  end

  def complete_attempt(attempt_id, candidate_sha, expected_metric_count) do
    now = now_us()

    transaction =
      Repo.transaction(fn ->
        attempt = attempt_row!(attempt_id)
        metric_count = metric_count(attempt_id, candidate_sha)

        cond do
          attempt.status not in ~w(running awaiting_report interrupted) ->
            Repo.rollback({:invalid_attempt_state, attempt.status})

          is_nil(attempt.summary) or String.trim(attempt.summary) == "" ->
            Repo.rollback(:missing_summary)

          is_nil(attempt.patch_artifact_id) ->
            Repo.rollback(:missing_patch_artifact)

          metric_count != expected_metric_count ->
            Repo.rollback({:incomplete_metrics, metric_count, expected_metric_count})

          true ->
            Repo.query!(
              "UPDATE attempts SET status = 'ready_for_integration', candidate_sha = ?, completed_at = ? WHERE id = ?",
              [candidate_sha, now, attempt_id]
            )

            event =
              insert_event!(
                attempt.campaign_id,
                "attempt",
                attempt_id,
                "attempt_ready_for_integration",
                %{ordinal: attempt.ordinal, candidate_sha: candidate_sha}
              )

            {attempt_row!(attempt_id), event}
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:attempt_complete_failed, Exception.message(error)}}
  end

  def reject_from_iteration(attempt_id, reason) do
    now = now_us()

    transaction =
      Repo.transaction(fn ->
        attempt = attempt_row!(attempt_id)

        cond do
          attempt.status not in ~w(running awaiting_report interrupted) ->
            Repo.rollback({:invalid_attempt_state, attempt.status})

          is_nil(attempt.summary) or String.trim(attempt.summary) == "" ->
            Repo.rollback(:missing_summary)

          attempt.recommended_outcome not in ~w(skip reject) ->
            Repo.rollback({:invalid_recommended_outcome, attempt.recommended_outcome})

          true ->
            Repo.query!(
              "UPDATE attempts SET status = 'rejected', outcome_reason = ?, completed_at = ? WHERE id = ?",
              [reason, now, attempt_id]
            )

            event =
              insert_event!(attempt.campaign_id, "attempt", attempt_id, "attempt_rejected", %{
                ordinal: attempt.ordinal,
                source: "iteration",
                reason: reason
              })

            {attempt_row!(attempt_id), event}
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:attempt_reject_failed, Exception.message(error)}}
  end

  def terminal_history(campaign_id, limit) do
    query_terminal_history(campaign_id, limit: limit)
  end

  def query_terminal_history(campaign_id, opts \\ []) do
    limit = Keyword.get(opts, :limit, 100)
    before = Keyword.get(opts, :before_ordinal)
    outcome = Keyword.get(opts, :outcome)
    placeholders = Enum.map_join(@terminal_statuses, ",", fn _ -> "?" end)

    {extra_conditions, extra_params} =
      []
      |> maybe_condition(before, "ordinal < ?")
      |> maybe_condition(outcome, "status = ?")
      |> Enum.unzip()

    conditions = ["campaign_id = ?", "status IN (#{placeholders})"] ++ extra_conditions

    Repo.query!(
      "SELECT #{attempt_columns()} FROM attempts WHERE #{Enum.join(conditions, " AND ")} ORDER BY ordinal DESC LIMIT ?",
      [campaign_id | @terminal_statuses] ++ extra_params ++ [limit]
    ).rows
    |> Enum.map(&attempt_from_row/1)
  end

  def send_message(campaign_id, from_session_id, target_session_id, body, priority) do
    now = now_us()

    Repo.transaction(fn ->
      case Repo.query!(
             "SELECT campaign_id FROM agent_sessions WHERE id = ?",
             [target_session_id]
           ).rows do
        [[^campaign_id]] -> :ok
        _ -> Repo.rollback(:identity_mismatch)
      end

      id = Ecto.UUID.generate()

      Repo.query!(
        """
        INSERT INTO agent_messages(
          id, campaign_id, from_session_id, to_session_id, scope, body,
          priority, status, created_at
        ) VALUES (?, ?, ?, ?, 'direct', ?, ?, 'queued', ?)
        """,
        [id, campaign_id, from_session_id, target_session_id, body, priority, now]
      )

      event =
        insert_event!(campaign_id, "agent_message", id, "agent_message_queued", %{
          from_session_id: from_session_id,
          to_session_id: target_session_id,
          priority: priority
        })

      {%{id: id, status: "queued"}, event}
    end)
    |> publish_value_and_event()
  rescue
    error -> {:error, {:agent_message_failed, Exception.message(error)}}
  end

  def read_messages(session_id, after_sequence, limit) do
    now = now_us()

    Repo.transaction(fn ->
      rows =
        Repo.query!(
          """
          SELECT sequence, id, from_session_id, scope, body, priority, status, created_at
          FROM agent_messages
          WHERE to_session_id = ? AND sequence > ? AND status != 'acknowledged'
          ORDER BY sequence LIMIT ?
          """,
          [session_id, after_sequence, limit]
        ).rows

      sequences = Enum.map(rows, &hd/1)

      if sequences != [] do
        placeholders = Enum.map_join(sequences, ",", fn _ -> "?" end)

        Repo.query!(
          "UPDATE agent_messages SET status = 'delivered', delivered_at = COALESCE(delivered_at, ?) WHERE sequence IN (#{placeholders})",
          [now | sequences]
        )
      end

      Enum.map(rows, fn [sequence, id, from, scope, body, priority, _status, created_at] ->
        %{
          sequence: sequence,
          id: id,
          from_session_id: from,
          scope: scope,
          body: body,
          priority: priority,
          status: "delivered",
          created_at: created_at
        }
      end)
    end)
    |> unwrap_value()
  rescue
    error -> {:error, {:mailbox_read_failed, Exception.message(error)}}
  end

  def ack_messages(session_id, through_sequence) do
    now = now_us()

    {count, _} =
      Repo.update_all(
        from_message(session_id, through_sequence),
        set: [status: "acknowledged", acknowledged_at: now]
      )

    {:ok, %{acknowledged_through: through_sequence, count: count}}
  rescue
    error -> {:error, {:mailbox_ack_failed, Exception.message(error)}}
  end

  def create_guidance(campaign_id, attempt_id, parent_session_id, kind, body) do
    now = now_us()

    Repo.transaction(fn ->
      attempt = if attempt_id, do: attempt_row!(attempt_id), else: nil

      if attempt &&
           (attempt.campaign_id != campaign_id or
              attempt.status not in ~w(running awaiting_report)) do
        Repo.rollback(:btw_requires_running_attempt)
      end

      id = Ecto.UUID.generate()

      Repo.query!(
        "INSERT INTO guidance(id, campaign_id, kind, attempt_id, parent_session_id, body, status, created_at) VALUES (?, ?, ?, ?, ?, ?, 'unread', ?)",
        [id, campaign_id, kind, attempt_id, parent_session_id, body, now]
      )

      event =
        insert_event!(campaign_id, "guidance", id, "guidance_created", %{
          kind: kind,
          attempt_id: attempt_id
        })

      {%{id: id, kind: kind, attempt_id: attempt_id, body: body, status: "unread"}, event}
    end)
    |> publish_transaction()
  rescue
    error -> {:error, {:guidance_create_failed, Exception.message(error)}}
  end

  def guidance_for_attempt(campaign_id, attempt_id, created_at) do
    Repo.query!(
      """
      SELECT sequence, id, kind, attempt_id, parent_session_id, body, status, created_at
      FROM guidance
      WHERE campaign_id = ? AND (
        (kind = 'campaign' AND created_at <= ?) OR
        (kind = 'attempt' AND attempt_id = ?)
      ) ORDER BY sequence
      """,
      [campaign_id, created_at, attempt_id]
    ).rows
    |> Enum.map(fn [sequence, id, kind, target, parent, body, status, at] ->
      %{
        sequence: sequence,
        id: id,
        kind: kind,
        attempt_id: target,
        parent_session_id: parent,
        body: body,
        status: status,
        created_at: at
      }
    end)
  end

  def mark_guidance_injected(guidance_id) do
    Repo.query!(
      "UPDATE guidance SET status = 'injected', injected_at = ? WHERE id = ? AND status = 'unread'",
      [now_us(), guidance_id]
    )

    :ok
  rescue
    error -> {:error, {:guidance_update_failed, Exception.message(error)}}
  end

  def events(campaign_id, after_sequence \\ 0, limit \\ 500) do
    Repo.query!(
      "SELECT sequence, event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at FROM domain_events WHERE (aggregate_id = ? OR aggregate_id IN (SELECT id FROM attempts WHERE campaign_id = ?) OR aggregate_id IN (SELECT id FROM sync_runs WHERE campaign_id = ?)) AND sequence > ? ORDER BY sequence LIMIT ?",
      [campaign_id, campaign_id, campaign_id, after_sequence, limit]
    ).rows
    |> Enum.map(fn [sequence, id, aggregate_type, aggregate_id, type, payload, at] ->
      %{
        sequence: sequence,
        event_id: id,
        aggregate_type: aggregate_type,
        aggregate_id: aggregate_id,
        event_type: type,
        payload: Jason.decode!(payload),
        created_at: at
      }
    end)
  end

  def important_campaign_events(campaign_id) do
    Repo.query!(
      "SELECT sequence, event_id, event_type, payload_json, created_at FROM domain_events WHERE aggregate_type = 'campaign' AND aggregate_id = ? AND event_type IN ('best_advanced','sampling_advanced') ORDER BY sequence",
      [campaign_id]
    ).rows
    |> Enum.map(fn [sequence, event_id, event_type, payload, created_at] ->
      %{
        sequence: sequence,
        event_id: event_id,
        event_type: event_type,
        payload: Jason.decode!(payload),
        created_at: created_at
      }
    end)
  end

  def lookup_idempotency(session_id, tool, key, request_hash) do
    case Repo.query!(
           "SELECT request_sha256, response_json FROM idempotency_records WHERE backend_session_id = ? AND tool_name = ? AND idempotency_key = ?",
           [session_id, tool, key]
         ).rows do
      [] -> :missing
      [[^request_hash, response]] -> {:replay, decode_response(response)}
      [[_other, _response]] -> :conflict
    end
  end

  def store_idempotency(session_id, tool, key, request_hash, response) do
    Repo.query!(
      "INSERT INTO idempotency_records(backend_session_id, tool_name, idempotency_key, request_sha256, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(backend_session_id, tool_name, idempotency_key) DO NOTHING",
      [session_id, tool, key, request_hash, encode_response(response), now_us()]
    )

    :ok
  rescue
    error -> {:error, {:idempotency_persist_failed, Exception.message(error)}}
  end

  def expected_metric_count(attempt_id) do
    [[count]] =
      Repo.query!(
        """
        SELECT COUNT(*) FROM sampling_revision_cases src
        CROSS JOIN metric_definitions md
        WHERE src.sampling_revision_id = (SELECT sampling_revision_id FROM attempts WHERE id = ?)
          AND md.spec_revision_id = (SELECT spec_revision_id FROM attempts WHERE id = ?)
        """,
        [attempt_id, attempt_id]
      ).rows

    count
  end

  defp campaign(campaign_id) do
    case Repo.query!(
           "SELECT id, status, resume_state, dispatch_gate, best_sha, attempts_created, max_attempts, plan_enabled, history_limit, current_spec_revision_id, stop_mode FROM campaigns WHERE id = ?",
           [campaign_id]
         ).rows do
      [[id, status, resume, gate, best, created, max, plan, history, spec, stop_mode]] ->
        {:ok,
         %{
           campaign_id: id,
           status: status,
           resume_state: resume,
           dispatch_gate: gate,
           best_sha: best,
           attempts_created: created,
           max_attempts: max,
           plan_enabled: plan in [true, 1],
           history_limit: history,
           current_spec_revision_id: spec,
           stop_mode: stop_mode
         }}

      [] ->
        {:error, :campaign_not_found}
    end
  end

  defp current_spec(nil), do: {:error, :current_spec_missing}

  defp current_spec(spec_id) do
    case Repo.query!(
           "SELECT id, revision, spec_json, protected_paths_json, protected_digest, reference_snapshot_json, skill_snapshot_json, target_snapshot_id, development_baseline_sha FROM spec_revisions WHERE id = ?",
           [spec_id]
         ).rows do
      [[id, revision, spec, paths, digest, refs, skill, target_snapshot_id, development_sha]] ->
        {:ok,
         %{
           id: id,
           revision: revision,
           spec_json: spec,
           protected_paths_json: paths,
           protected_digest: digest,
           reference_snapshot_json: refs,
           skill_snapshot_json: skill,
           target_snapshot_id: target_snapshot_id,
           development_baseline_sha: development_sha
         }}

      [] ->
        {:error, :current_spec_not_found}
    end
  end

  defp target_snapshot(nil), do: nil

  defp target_snapshot(id) do
    case Repo.query!(
           """
           SELECT ts.id, sr.revision, ts.source_kind, ts.source_reference_id, ts.source_sha,
             ts.tree_sha, ts.entrypoint, ts.digest, ts.checkout_relative_path
           FROM target_snapshots ts
           JOIN spec_revisions sr ON sr.id = ts.spec_revision_id
           WHERE ts.id = ?
           """,
           [id]
         ).rows do
      [
        [
          id,
          revision,
          source_kind,
          reference_id,
          source_sha,
          tree_sha,
          entrypoint,
          digest,
          checkout
        ]
      ] ->
        %{
          id: id,
          revision: revision,
          source_kind: source_kind,
          source_reference_id: reference_id,
          source_sha: source_sha,
          tree_sha: tree_sha,
          entrypoint: entrypoint,
          digest: digest,
          checkout_relative_path: checkout
        }

      [] ->
        nil
    end
  end

  defp current_sampling(campaign_id, spec_id) do
    case Repo.query!(
           "SELECT id, sequence, cause, summary, created_at FROM sampling_revisions WHERE campaign_id = ? AND spec_revision_id = ? ORDER BY sequence DESC LIMIT 1",
           [campaign_id, spec_id]
         ).rows do
      [[id, sequence, cause, summary, created_at]] ->
        {:ok,
         %{id: id, sequence: sequence, cause: cause, summary: summary, created_at: created_at}}

      [] ->
        {:error, :sampling_revision_missing}
    end
  end

  defp cases(spec_id) do
    Repo.query!(
      "SELECT name, kind, shape_json, dtype_json, layout_json, frequency_weight FROM benchmark_cases WHERE spec_revision_id = ? ORDER BY ordinal",
      [spec_id]
    ).rows
    |> Enum.map(fn [id, kind, shape, dtype, layout, weight] ->
      %{
        "id" => id,
        "kind" => kind,
        "shape" => Jason.decode!(shape),
        "dtype" => Jason.decode!(dtype),
        "layout" => Jason.decode!(layout),
        "frequency_weight" => weight
      }
    end)
  end

  defp sampled_case_ids(sampling_id) do
    Repo.query!(
      "SELECT bc.name FROM sampling_revision_cases src JOIN benchmark_cases bc ON bc.id = src.benchmark_case_id WHERE src.sampling_revision_id = ? ORDER BY bc.ordinal",
      [sampling_id]
    ).rows
    |> List.flatten()
  end

  defp metrics(spec_id) do
    Repo.query!(
      "SELECT name, unit, direction, role, min_improvement_ratio, max_regression_ratio, parser_json FROM metric_definitions WHERE spec_revision_id = ? ORDER BY name",
      [spec_id]
    ).rows
    |> Enum.map(fn [id, unit, direction, role, threshold, max_regression, parser] ->
      %{
        "id" => id,
        "unit" => unit,
        "direction" => direction,
        "role" => role,
        "min_improvement_ratio" => threshold,
        "max_regression_ratio" => max_regression,
        "parser" => Jason.decode!(parser)
      }
    end)
  end

  defp best_metrics(campaign_id) do
    Repo.query!(
      """
      SELECT bc.name, md.name, bm.value, bm.noise_tolerance
      FROM best_metrics bm
      JOIN best_revisions br ON br.id = bm.best_revision_id
      JOIN benchmark_cases bc ON bc.id = bm.benchmark_case_id
      JOIN metric_definitions md ON md.id = bm.metric_definition_id
      WHERE br.id = (SELECT id FROM best_revisions WHERE campaign_id = ? ORDER BY sequence DESC LIMIT 1)
      """,
      [campaign_id]
    ).rows
    |> Map.new(fn [case_id, metric_id, value, noise] ->
      {{case_id, metric_id}, %{value: value, noise_tolerance: noise}}
    end)
  end

  defp transition_attempt(attempt_id, allowed, status, event_type, payload, attrs \\ []) do
    transaction =
      Repo.transaction(fn ->
        attempt = attempt_row!(attempt_id)

        if attempt.status not in allowed,
          do: Repo.rollback({:invalid_attempt_transition, attempt.status, status})

        columns = [status: status] ++ attrs
        assignments = Enum.map_join(columns, ", ", fn {column, _value} -> "#{column} = ?" end)
        values = Enum.map(columns, &elem(&1, 1))
        Repo.query!("UPDATE attempts SET #{assignments} WHERE id = ?", values ++ [attempt_id])
        event = insert_event!(attempt.campaign_id, "attempt", attempt_id, event_type, payload)
        {attempt_row!(attempt_id), event}
      end)

    publish_transaction(transaction)
  end

  defp campaign_row!(campaign_id) do
    {:ok, campaign} = campaign(campaign_id)
    campaign
  end

  defp current_sampling_row!(campaign_id, spec_id) do
    {:ok, sampling} = current_sampling(campaign_id, spec_id)
    sampling
  end

  defp slot_busy?(campaign_id, slot_index) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    Repo.query!(
      "SELECT 1 FROM attempts WHERE campaign_id = ? AND slot_index = ? AND status IN (#{placeholders}) LIMIT 1",
      [campaign_id, slot_index | @active_statuses]
    ).rows != []
  end

  defp attempt_row!(attempt_id) do
    case Repo.query!("SELECT #{attempt_columns()} FROM attempts WHERE id = ?", [attempt_id]).rows do
      [row] -> attempt_from_row(row)
      [] -> Repo.rollback(:attempt_not_found)
    end
  end

  defp attempt_columns do
    "id, campaign_id, ordinal, spec_revision_id, slot_index, sampling_revision_id, status, resume_state, base_sha, candidate_sha, branch_name, worktree_relative_path, description, summary, modification_scope_json, risk_json, profiler_summary, recommended_outcome, outcome_reason, patch_artifact_id, plan_artifact_id, correctness_artifact_id, metrics_artifact_id, accepted_sha, created_at, started_at, completed_at"
  end

  defp attempt_from_row([
         id,
         campaign_id,
         ordinal,
         spec_revision_id,
         slot_index,
         sampling_revision_id,
         status,
         resume_state,
         base_sha,
         candidate_sha,
         branch_name,
         worktree,
         description,
         summary,
         scope,
         risks,
         profiler,
         recommended,
         reason,
         patch,
         plan,
         correctness,
         metrics,
         accepted_sha,
         created_at,
         started_at,
         completed_at
       ]) do
    %{
      id: id,
      campaign_id: campaign_id,
      ordinal: ordinal,
      spec_revision_id: spec_revision_id,
      slot_index: slot_index,
      sampling_revision_id: sampling_revision_id,
      status: status,
      resume_state: resume_state,
      base_sha: base_sha,
      candidate_sha: candidate_sha,
      branch_name: branch_name,
      worktree_relative_path: worktree,
      description: description,
      summary: summary,
      modification_scope: Jason.decode!(scope),
      risks: Jason.decode!(risks),
      profiler_summary: profiler,
      recommended_outcome: recommended,
      outcome_reason: reason,
      patch_artifact_id: patch,
      plan_artifact_id: plan,
      correctness_artifact_id: correctness,
      metrics_artifact_id: metrics,
      accepted_sha: accepted_sha,
      created_at: created_at,
      started_at: started_at,
      completed_at: completed_at
    }
  end

  defp session_from_row([
         id,
         campaign_id,
         attempt_id,
         role,
         slot,
         backend,
         protocol,
         model,
         reasoning_effort,
         status,
         required,
         started,
         ended
       ]) do
    %{
      id: id,
      campaign_id: campaign_id,
      attempt_id: attempt_id,
      role: role,
      slot_index: slot,
      backend: backend,
      backend_protocol: protocol,
      model: model,
      reasoning_effort: reasoning_effort,
      status: status,
      required_operations: Jason.decode!(required),
      started_at: started,
      ended_at: ended
    }
  end

  defp metric_count(attempt_id, candidate_sha) do
    [[count]] =
      Repo.query!(
        "SELECT COUNT(*) FROM attempt_metrics WHERE attempt_id = ? AND measured_sha = ?",
        [attempt_id, candidate_sha]
      ).rows

    count
  end

  defp insert_event!(campaign_id, aggregate_type, aggregate_id, event_type, payload) do
    event = %{
      sequence: nil,
      event_id: Ecto.UUID.generate(),
      aggregate_type: aggregate_type,
      aggregate_id: aggregate_id,
      event_type: event_type,
      payload: payload,
      created_at: now_us(),
      campaign_id: campaign_id
    }

    result =
      Repo.query!(
        "INSERT INTO domain_events(event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING sequence",
        [
          event.event_id,
          aggregate_type,
          aggregate_id,
          event_type,
          Jason.encode!(payload),
          event.created_at
        ]
      )

    [[sequence]] = result.rows
    %{event | sequence: sequence}
  end

  defp publish_transaction({:ok, {value, event}}) do
    publish(event)
    {:ok, value}
  end

  defp publish_transaction({:error, reason}), do: {:error, reason}

  defp publish_event_result({:ok, event}) do
    publish(event)
    {:ok, event}
  end

  defp publish_event_result({:error, reason}), do: {:error, reason}

  defp publish_value_and_event({:ok, {value, event}}) do
    publish(event)
    {:ok, value}
  end

  defp publish_value_and_event({:error, reason}), do: {:error, reason}

  defp unwrap_value({:ok, value}), do: {:ok, value}
  defp unwrap_value({:error, reason}), do: {:error, reason}

  defp publish(event) do
    Phoenix.PubSub.broadcast(Pika.PubSub, Pika.Persistence.topic(event.campaign_id), {
      :domain_event,
      event
    })

    Phoenix.PubSub.broadcast(Pika.PubSub, "pika:optimization:#{event.campaign_id}", {
      :optimization_event,
      event
    })
  end

  defp maybe_condition(conditions, nil, _sql), do: conditions
  defp maybe_condition(conditions, value, sql), do: conditions ++ [{sql, value}]

  defp from_message(session_id, through_sequence) do
    import Ecto.Query

    from(message in "agent_messages",
      where:
        field(message, :to_session_id) == ^session_id and
          field(message, :sequence) <= ^through_sequence and
          field(message, :status) != "acknowledged"
    )
  end

  defp encode_response(response) do
    Jason.encode!(%{
      "encoding" => "erlang-term-v1",
      "data" => response |> :erlang.term_to_binary(compressed: 6) |> Base.encode64()
    })
  end

  defp decode_response(body) do
    %{"encoding" => "erlang-term-v1", "data" => data} = Jason.decode!(body)
    data |> Base.decode64!() |> :erlang.binary_to_term([:safe])
  end

  defp now_us, do: System.system_time(:microsecond)
end
