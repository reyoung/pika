defmodule Pika.SyncStore do
  @moduledoc false

  alias Pika.{Persistence, Repo}

  @active_statuses ~w(requested preparing fetching merging awaiting_spec_confirmation validating pushing advancing_best)

  def request(campaign_id, remote, branch, base_sha, remote_sha, idempotency_key) do
    request_hash = hash({remote, branch, base_sha, remote_sha})

    transaction =
      Repo.transaction(fn ->
        case control_action(idempotency_key, "sync", request_hash) do
          {:replay, response} ->
            {response, []}

          :conflict ->
            Repo.rollback(:idempotency_conflict)

          :missing ->
            campaign = campaign!(campaign_id)

            cond do
              campaign.status in ~w(stopped blocked completed) ->
                Repo.rollback({:invalid_campaign_state, campaign.status})

              campaign.best_sha != base_sha ->
                Repo.rollback({:stale_best, campaign.best_sha})

              active_run(campaign_id) != nil ->
                Repo.rollback(:sync_already_active)

              true ->
                id = Ecto.UUID.generate()
                now = now_us()
                sync_branch = "pika/sync/#{id}"
                worktree = "sync/#{id}"

                Repo.query!(
                  "INSERT INTO sync_runs(id, campaign_id, status, remote, branch, sync_branch, worktree_relative_path, base_sha, remote_before_sha, started_at) VALUES (?, ?, 'requested', ?, ?, ?, ?, ?, ?, ?)",
                  [
                    id,
                    campaign_id,
                    remote,
                    branch,
                    sync_branch,
                    worktree,
                    base_sha,
                    remote_sha,
                    now
                  ]
                )

                Repo.query!(
                  "UPDATE campaigns SET dispatch_gate = 'sync', updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
                  [now, campaign_id]
                )

                event =
                  insert_event!("sync", id, "sync_requested", %{
                    remote: remote,
                    branch: branch,
                    remote_sha: remote_sha,
                    best_sha: base_sha
                  })

                response = public_run(run!(id))

                insert_control_action!(
                  idempotency_key,
                  campaign_id,
                  "sync",
                  request_hash,
                  response
                )

                {response, [event]}
            end
        end
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_request_failed, Exception.message(error)}}
  end

  def ready_to_start?(campaign_id) do
    [[attempts]] =
      Repo.query!(
        "SELECT COUNT(*) FROM attempts WHERE campaign_id = ? AND status NOT IN ('accepted','rejected','cancelled')",
        [campaign_id]
      ).rows

    [[leases]] =
      Repo.query!("SELECT COUNT(*) FROM integration_leases WHERE campaign_id = ?", [campaign_id]).rows

    attempts == 0 and leases == 0
  end

  def begin_prepare(run_id) do
    transition(run_id, ~w(requested preparing fetching), "preparing", "sync_preparing", %{})
  end

  def mark_merging(run_id) do
    transition(run_id, ~w(preparing fetching merging), "merging", "sync_merging", %{})
  end

  def report_candidate(run_id, candidate_sha, protected_paths, protected_digest) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)

        unless run.status in ~w(merging validating awaiting_spec_confirmation),
          do: Repo.rollback({:invalid_sync_state, run.status})

        protected_paths = Enum.uniq(protected_paths)
        status = if(protected_paths == [], do: "validating", else: "awaiting_spec_confirmation")
        now = now_us()

        Repo.query!(
          "UPDATE sync_runs SET candidate_sha = ?, protected_paths_json = ?, protected_digest = ?, status = ? WHERE id = ?",
          [candidate_sha, Jason.encode!(protected_paths), protected_digest, status, run_id]
        )

        if protected_paths != [] do
          campaign = campaign!(run.campaign_id)

          Repo.query!(
            "UPDATE campaigns SET status = 'awaiting_spec_confirmation', resume_state = ?, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
            [campaign.status, now, run.campaign_id]
          )
        end

        event =
          insert_event!("sync", run_id, "sync_candidate_reported", %{
            candidate_sha: candidate_sha,
            protected_paths: protected_paths,
            awaiting_spec_confirmation: protected_paths != []
          })

        {public_run(run!(run_id)), [event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_candidate_failed, Exception.message(error)}}
  end

  def confirm_spec(run_id, approved, idempotency_key) when is_boolean(approved) do
    request_hash = hash({run_id, approved})

    transaction =
      Repo.transaction(fn ->
        case control_action(idempotency_key, "sync_spec_confirmation", request_hash) do
          {:replay, response} ->
            {response, []}

          :conflict ->
            Repo.rollback(:idempotency_conflict)

          :missing ->
            run = run!(run_id)

            if run.status != "awaiting_spec_confirmation",
              do: Repo.rollback({:invalid_sync_state, run.status})

            campaign = campaign!(run.campaign_id)
            restored = campaign.resume_state || "optimizing"
            now = now_us()
            next = if(approved, do: "validating", else: "failed")

            Repo.query!(
              "UPDATE sync_runs SET status = ?, failure_reason = ?, completed_at = ? WHERE id = ?",
              [
                next,
                if(approved, do: nil, else: "protected inputs rejected by user"),
                if(approved, do: nil, else: now),
                run_id
              ]
            )

            Repo.query!(
              "UPDATE campaigns SET status = ?, resume_state = NULL, dispatch_gate = ?, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
              [restored, if(approved, do: "sync", else: nil), now, run.campaign_id]
            )

            type =
              if(approved, do: "sync_spec_change_confirmed", else: "sync_spec_change_rejected")

            event = insert_event!("sync", run_id, type, %{protected_paths: run.protected_paths})
            response = public_run(run!(run_id))

            insert_control_action!(
              idempotency_key,
              run.campaign_id,
              "sync_spec_confirmation",
              request_hash,
              response
            )

            {response, [event]}
        end
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_spec_confirmation_failed, Exception.message(error)}}
  end

  def record_validation(run_id, metrics, correctness_artifact_id, metrics_artifact_id) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)

        if run.status != "validating", do: Repo.rollback({:invalid_sync_state, run.status})

        regressions =
          Enum.filter(metrics, fn metric ->
            improvement = metric[:improvement_ratio] || metric["improvement_ratio"]
            tolerance = metric[:noise_tolerance] || metric["noise_tolerance"] || 0.005
            is_nil(improvement) or improvement < -tolerance
          end)

        if regressions != [], do: Repo.rollback({:sync_regression, regressions})

        Repo.query!(
          "UPDATE sync_runs SET validation_metrics_json = ?, correctness_artifact_id = ?, metrics_artifact_id = ? WHERE id = ?",
          [
            Jason.encode!(Pika.JSONSafe.json_safe(metrics)),
            correctness_artifact_id,
            metrics_artifact_id,
            run_id
          ]
        )

        event =
          insert_event!("sync", run_id, "sync_validation_passed", %{
            candidate_sha: run.candidate_sha,
            metric_count: length(metrics)
          })

        {public_run(run!(run_id)), [event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_validation_failed, Exception.message(error)}}
  end

  def create_intent(run_id, idempotency_key) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)

        cond do
          run.status != "validating" -> Repo.rollback({:invalid_sync_state, run.status})
          is_nil(run.validation_metrics) -> Repo.rollback(:sync_validation_required)
          true -> :ok
        end

        case intent_for_run(run_id) do
          {:ok, intent} ->
            {intent, []}

          {:error, :sync_intent_missing} ->
            id = Ecto.UUID.generate()
            now = now_us()

            Repo.query!(
              "INSERT INTO operation_intents(id, campaign_id, kind, owner_type, owner_id, state, expected_best_sha, target_sha, idempotency_key, payload_json, created_at, updated_at) VALUES (?, ?, 'sync', 'sync', ?, 'pending', ?, ?, ?, ?, ?, ?)",
              [
                id,
                run.campaign_id,
                run.id,
                run.base_sha,
                run.candidate_sha,
                idempotency_key,
                Jason.encode!(%{
                  remote: run.remote,
                  branch: run.branch,
                  before_sha: run.remote_before_sha
                }),
                now,
                now
              ]
            )

            Repo.query!("UPDATE sync_runs SET intent_id = ?, status = 'pushing' WHERE id = ?", [
              id,
              run_id
            ])

            event = insert_event!("sync", run_id, "sync_intent_created", %{intent_id: id})
            {intent!(id), [event]}
        end
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_intent_failed, Exception.message(error)}}
  end

  def mark_advancing(run_id, remote_sha) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)

        unless run.status in ~w(pushing advancing_best),
          do: Repo.rollback({:invalid_sync_state, run.status})

        if remote_sha != run.candidate_sha, do: Repo.rollback(:remote_candidate_mismatch)

        Repo.query!(
          "UPDATE sync_runs SET status = 'advancing_best', remote_after_sha = ? WHERE id = ?",
          [remote_sha, run_id]
        )

        event = insert_event!("sync", run_id, "sync_remote_advanced", %{remote_sha: remote_sha})
        {public_run(run!(run_id)), [event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_remote_advance_failed, Exception.message(error)}}
  end

  def complete(run_id, trail_artifact_id) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)
        intent = intent!(run.intent_id)
        campaign = campaign!(run.campaign_id)

        cond do
          run.status != "advancing_best" ->
            Repo.rollback({:invalid_sync_state, run.status})

          campaign.best_sha != run.base_sha ->
            Repo.rollback({:stale_best, campaign.best_sha})

          intent.state not in ~w(pending applied) ->
            Repo.rollback({:invalid_intent_state, intent.state})

          true ->
            :ok
        end

        now = now_us()
        {spec_id, metric_mode} = maybe_rebuild_spec!(run, campaign, now)
        best_revision_id = Ecto.UUID.generate()

        [[sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(sequence), 0) + 1 FROM best_revisions WHERE campaign_id = ?",
            [run.campaign_id]
          ).rows

        Repo.query!(
          "INSERT INTO best_revisions(id, campaign_id, sequence, sha, cause, spec_revision_id, summary, inserted_at) VALUES (?, ?, ?, ?, 'sync', ?, ?, ?)",
          [
            best_revision_id,
            run.campaign_id,
            sequence,
            run.candidate_sha,
            spec_id,
            run.summary || "User-confirmed Sync",
            now
          ]
        )

        insert_best_metrics!(
          best_revision_id,
          spec_id,
          run.candidate_sha,
          run.validation_metrics,
          metric_mode,
          now
        )

        Repo.query!(
          "UPDATE campaigns SET best_sha = ?, current_spec_revision_id = ?, dispatch_gate = NULL, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
          [run.candidate_sha, spec_id, now, run.campaign_id]
        )

        Repo.query!(
          "UPDATE sync_runs SET status = 'completed', remote_after_sha = ?, best_after_sha = ?, trail_artifact_id = ?, completed_at = ? WHERE id = ?",
          [run.candidate_sha, run.candidate_sha, trail_artifact_id, now, run_id]
        )

        Repo.query!(
          "UPDATE operation_intents SET state = 'verified', updated_at = ? WHERE id = ?",
          [now, intent.id]
        )

        best_event =
          insert_event!("campaign", run.campaign_id, "best_advanced", %{
            cause: "sync",
            old_sha: run.base_sha,
            new_sha: run.candidate_sha,
            sync_run_id: run.id,
            spec_revision_id: spec_id
          })

        completed_event =
          insert_event!("sync", run.id, "sync_completed", %{
            remote: run.remote,
            branch: run.branch,
            before_sha: run.remote_before_sha,
            after_sha: run.candidate_sha
          })

        {public_run(run!(run_id)), [best_event, completed_event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_complete_failed, Exception.message(error)}}
  end

  def fail(run_id, reason) do
    terminal(run_id, "failed", "sync_failed", reason, false)
  end

  def block(run_id, reason) do
    terminal(run_id, "blocked", "sync_blocked", reason, true)
  end

  def active_run(campaign_id) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    case Repo.query!(
           "SELECT id FROM sync_runs WHERE campaign_id = ? AND status IN (#{placeholders}) ORDER BY started_at DESC LIMIT 1",
           [campaign_id | @active_statuses]
         ).rows do
      [[id]] -> public_run(run!(id))
      [] -> nil
    end
  end

  def latest_run(campaign_id) do
    case Repo.query!(
           "SELECT id FROM sync_runs WHERE campaign_id = ? ORDER BY started_at DESC LIMIT 1",
           [campaign_id]
         ).rows do
      [[id]] -> {:ok, public_run(run!(id))}
      [] -> {:error, :sync_run_missing}
    end
  end

  def run(run_id) do
    try do
      {:ok, public_run(run!(run_id))}
    rescue
      _ -> {:error, :sync_run_missing}
    end
  end

  def intent_for_run(run_id) do
    case Repo.query!(
           "SELECT id, campaign_id, state, expected_best_sha, target_sha, idempotency_key, payload_json FROM operation_intents WHERE kind = 'sync' AND owner_id = ? ORDER BY created_at DESC LIMIT 1",
           [run_id]
         ).rows do
      [row] -> {:ok, intent_from_row(row)}
      [] -> {:error, :sync_intent_missing}
    end
  end

  defp transition(run_id, allowed, status, event_type, payload) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)
        if run.status not in allowed, do: Repo.rollback({:invalid_sync_state, run.status})
        Repo.query!("UPDATE sync_runs SET status = ? WHERE id = ?", [status, run_id])
        event = insert_event!("sync", run_id, event_type, payload)
        {public_run(run!(run_id)), [event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_transition_failed, Exception.message(error)}}
  end

  defp terminal(run_id, status, event_type, reason, block_campaign?) do
    transaction =
      Repo.transaction(fn ->
        run = run!(run_id)
        now = now_us()

        Repo.query!(
          "UPDATE sync_runs SET status = ?, failure_reason = ?, completed_at = ? WHERE id = ?",
          [status, inspect(reason), now, run_id]
        )

        if run.intent_id do
          Repo.query!(
            "UPDATE operation_intents SET state = 'aborted', updated_at = ? WHERE id = ?",
            [now, run.intent_id]
          )
        end

        campaign = campaign!(run.campaign_id)

        if block_campaign? do
          resume =
            if(campaign.status == "blocked", do: campaign.resume_state, else: campaign.status)

          Repo.query!(
            "UPDATE campaigns SET status = 'blocked', resume_state = ?, dispatch_gate = 'blocked', updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
            [resume, now, run.campaign_id]
          )
        else
          restored =
            if(campaign.status == "awaiting_spec_confirmation",
              do: campaign.resume_state || "optimizing",
              else: campaign.status
            )

          Repo.query!(
            "UPDATE campaigns SET status = ?, resume_state = NULL, dispatch_gate = NULL, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
            [restored, now, run.campaign_id]
          )
        end

        event = insert_event!("sync", run_id, event_type, %{reason: inspect(reason)})
        {public_run(run!(run_id)), [event]}
      end)

    publish_events(transaction)
  rescue
    error -> {:error, {:sync_terminal_failed, Exception.message(error)}}
  end

  defp maybe_rebuild_spec!(%{protected_paths: []}, campaign, _now),
    do: {campaign.current_spec_revision_id, :comparison}

  defp maybe_rebuild_spec!(run, campaign, now) do
    [
      [
        revision,
        spec_json,
        paths_json,
        refs_json,
        skill_json,
        target_snapshot_id,
        implementation_manifest_json
      ]
    ] =
      Repo.query!(
        "SELECT revision, spec_json, protected_paths_json, reference_snapshot_json, skill_snapshot_json, target_snapshot_id, implementation_manifest_json FROM spec_revisions WHERE id = ?",
        [campaign.current_spec_revision_id]
      ).rows

    new_spec_id = Ecto.UUID.generate()

    Repo.query!(
      "INSERT INTO spec_revisions(id, campaign_id, revision, status, spec_json, protected_paths_json, protected_digest, baseline_sha, reference_snapshot_json, skill_snapshot_json, confirmed_at, inserted_at, updated_at, target_snapshot_id, development_baseline_sha, implementation_manifest_json) VALUES (?, ?, ?, 'confirmed', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
      [
        new_spec_id,
        run.campaign_id,
        revision + 1,
        spec_json,
        paths_json,
        run.protected_digest,
        run.candidate_sha,
        refs_json,
        skill_json,
        now,
        now,
        now,
        target_snapshot_id,
        run.candidate_sha,
        implementation_manifest_json
      ]
    )

    clone_cases_and_metrics!(campaign.current_spec_revision_id, new_spec_id)
    clone_sampling!(run.campaign_id, campaign.current_spec_revision_id, new_spec_id, run.id, now)

    insert_event!("campaign", run.campaign_id, "spec_revision_advanced", %{
      old_spec_revision_id: campaign.current_spec_revision_id,
      new_spec_revision_id: new_spec_id,
      source: "sync",
      protected_paths: run.protected_paths
    })

    {new_spec_id, :baseline}
  end

  defp clone_cases_and_metrics!(old_spec_id, new_spec_id) do
    Repo.query!(
      "INSERT INTO benchmark_cases(id, spec_revision_id, ordinal, name, kind, shape_json, dtype_json, layout_json, frequency_weight) SELECT lower(hex(randomblob(16))), ?, ordinal, name, kind, shape_json, dtype_json, layout_json, frequency_weight FROM benchmark_cases WHERE spec_revision_id = ?",
      [new_spec_id, old_spec_id]
    )

    Repo.query!(
      "INSERT INTO metric_definitions(id, spec_revision_id, name, unit, direction, role, min_improvement_ratio, parser_json) SELECT lower(hex(randomblob(16))), ?, name, unit, direction, role, min_improvement_ratio, parser_json FROM metric_definitions WHERE spec_revision_id = ?",
      [new_spec_id, old_spec_id]
    )
  end

  defp clone_sampling!(campaign_id, old_spec_id, new_spec_id, run_id, now) do
    sampling_id = Ecto.UUID.generate()

    Repo.query!(
      "INSERT INTO sampling_revisions(id, campaign_id, spec_revision_id, sequence, cause, summary, estimated_cost_json, created_at) VALUES (?, ?, ?, 1, 'sync', ?, '{}', ?)",
      [sampling_id, campaign_id, new_spec_id, "Rebuilt after Sync #{run_id}", now]
    )

    Repo.query!(
      "INSERT INTO sampling_revision_cases(sampling_revision_id, benchmark_case_id, reason, evidence_json) SELECT ?, new_case.id, old_src.reason, old_src.evidence_json FROM sampling_revision_cases old_src JOIN benchmark_cases old_case ON old_case.id = old_src.benchmark_case_id JOIN benchmark_cases new_case ON new_case.spec_revision_id = ? AND new_case.name = old_case.name WHERE old_src.sampling_revision_id = (SELECT id FROM sampling_revisions WHERE campaign_id = ? AND spec_revision_id = ? ORDER BY sequence DESC LIMIT 1)",
      [sampling_id, new_spec_id, campaign_id, old_spec_id]
    )
  end

  defp insert_best_metrics!(best_revision_id, spec_id, sha, metrics, mode, now) do
    Enum.each(metrics, fn metric ->
      case_name = metric["case_id"] || metric[:case_id]
      metric_name = metric["metric_id"] || metric[:metric_id]

      [[case_id]] =
        Repo.query!("SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?", [
          spec_id,
          case_name
        ]).rows

      [[metric_id]] =
        Repo.query!("SELECT id FROM metric_definitions WHERE spec_revision_id = ? AND name = ?", [
          spec_id,
          metric_name
        ]).rows

      value = metric["value"] || metric[:value]
      target_snapshot_id = metric["target_snapshot_id"] || metric[:target_snapshot_id]
      target_value = metric["target_value"] || metric[:target_value]

      target_improvement =
        metric["target_relative_improvement"] || metric[:target_relative_improvement]

      baseline_value =
        if(mode == :baseline,
          do: value,
          else: metric["baseline_value"] || metric[:baseline_value]
        )

      improvement =
        if(mode == :baseline,
          do: 0.0,
          else: metric["improvement_ratio"] || metric[:improvement_ratio]
        )

      best_improvement = if(mode == :baseline, do: 0.0, else: improvement)

      Repo.query!(
        "INSERT INTO best_metrics(best_revision_id, benchmark_case_id, metric_definition_id, measured_sha, value, baseline_value, improvement_ratio, mad, noise_tolerance, pair_count, valid_pair_count, source, measured_at, target_snapshot_id, target_value, target_relative_improvement, best_relative_improvement) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'sync', ?, ?, ?, ?, ?)",
        [
          best_revision_id,
          case_id,
          metric_id,
          sha,
          value,
          baseline_value,
          improvement,
          metric["mad"] || metric[:mad],
          metric["noise_tolerance"] || metric[:noise_tolerance],
          metric["pair_count"] || metric[:pair_count],
          metric["valid_pair_count"] || metric[:valid_pair_count],
          now,
          target_snapshot_id,
          target_value,
          target_improvement,
          best_improvement
        ]
      )
    end)
  end

  defp run!(run_id) do
    case Repo.query!(
           "SELECT id, campaign_id, status, remote, branch, sync_branch, worktree_relative_path, base_sha, remote_before_sha, candidate_sha, remote_after_sha, best_after_sha, protected_paths_json, protected_digest, validation_metrics_json, summary, failure_reason, correctness_artifact_id, metrics_artifact_id, trail_artifact_id, intent_id, started_at, completed_at FROM sync_runs WHERE id = ?",
           [run_id]
         ).rows do
      [row] -> run_from_row(row)
      [] -> raise "sync run not found: #{run_id}"
    end
  end

  defp campaign!(campaign_id) do
    case Repo.query!(
           "SELECT id, status, resume_state, dispatch_gate, best_sha, current_spec_revision_id FROM campaigns WHERE id = ?",
           [campaign_id]
         ).rows do
      [[id, status, resume, gate, best, spec]] ->
        %{
          id: id,
          status: status,
          resume_state: resume,
          dispatch_gate: gate,
          best_sha: best,
          current_spec_revision_id: spec
        }

      [] ->
        Repo.rollback(:campaign_not_found)
    end
  end

  defp intent!(id) do
    case Repo.query!(
           "SELECT id, campaign_id, state, expected_best_sha, target_sha, idempotency_key, payload_json FROM operation_intents WHERE id = ?",
           [id]
         ).rows do
      [row] -> intent_from_row(row)
      [] -> Repo.rollback(:sync_intent_missing)
    end
  end

  defp control_action(key, action, request_hash) do
    case Repo.query!(
           "SELECT action, request_sha256, response_json FROM control_actions WHERE idempotency_key = ?",
           [key]
         ).rows do
      [] -> :missing
      [[^action, ^request_hash, response]] -> {:replay, Jason.decode!(response)}
      [_] -> :conflict
    end
  end

  defp insert_control_action!(key, campaign_id, action, request_hash, response) do
    Repo.query!(
      "INSERT INTO control_actions(idempotency_key, campaign_id, action, request_sha256, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?)",
      [
        key,
        campaign_id,
        action,
        request_hash,
        Jason.encode!(Pika.JSONSafe.json_safe(response)),
        now_us()
      ]
    )
  end

  defp insert_event!(aggregate_type, aggregate_id, event_type, payload) do
    event = %{
      event_id: Ecto.UUID.generate(),
      aggregate_type: aggregate_type,
      aggregate_id: aggregate_id,
      event_type: event_type,
      payload: payload,
      created_at: now_us()
    }

    [[sequence]] =
      Repo.query!(
        "INSERT INTO domain_events(event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING sequence",
        [
          event.event_id,
          aggregate_type,
          aggregate_id,
          event_type,
          Jason.encode!(Pika.JSONSafe.json_safe(payload)),
          event.created_at
        ]
      ).rows

    Map.put(event, :sequence, sequence)
  end

  defp publish_events({:ok, {value, events}}) do
    Enum.each(events, &publish(&1, value))
    {:ok, value}
  end

  defp publish_events({:error, reason}), do: {:error, reason}

  defp publish(event, value) do
    if Process.whereis(Pika.PubSub) do
      campaign_id = value["campaign_id"] || value[:campaign_id] || event.aggregate_id

      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        Persistence.topic(campaign_id),
        {:domain_event, event}
      )

      Phoenix.PubSub.broadcast(Pika.PubSub, "pika:sync:events", {:sync_event, event})
    end
  end

  defp run_from_row([
         id,
         campaign,
         status,
         remote,
         branch,
         sync_branch,
         worktree,
         base,
         remote_before,
         candidate,
         remote_after,
         best_after,
         protected,
         digest,
         metrics,
         summary,
         failure,
         correctness,
         metrics_artifact,
         trail,
         intent,
         started,
         completed
       ]) do
    %{
      id: id,
      campaign_id: campaign,
      status: status,
      remote: remote,
      branch: branch,
      sync_branch: sync_branch,
      worktree_relative_path: worktree,
      base_sha: base,
      remote_before_sha: remote_before,
      candidate_sha: candidate,
      remote_after_sha: remote_after,
      best_after_sha: best_after,
      protected_paths: Jason.decode!(protected),
      protected_digest: digest,
      validation_metrics: decode_optional(metrics),
      summary: summary,
      failure_reason: failure,
      correctness_artifact_id: correctness,
      metrics_artifact_id: metrics_artifact,
      trail_artifact_id: trail,
      intent_id: intent,
      started_at: started,
      completed_at: completed
    }
  end

  defp intent_from_row([id, campaign, state, expected, target, key, payload]),
    do: %{
      id: id,
      campaign_id: campaign,
      state: state,
      expected_best_sha: expected,
      target_sha: target,
      idempotency_key: key,
      payload: Jason.decode!(payload)
    }

  defp public_run(run), do: run
  defp decode_optional(nil), do: nil
  defp decode_optional(value), do: Jason.decode!(value)

  defp hash(value),
    do: :crypto.hash(:sha256, :erlang.term_to_binary(value)) |> Base.encode16(case: :lower)

  defp now_us, do: System.system_time(:microsecond)
end
