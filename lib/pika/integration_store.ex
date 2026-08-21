defmodule Pika.IntegrationStore do
  @moduledoc false

  alias Pika.{AttemptStore, Persistence, Repo}

  def queue_head(campaign_id) do
    case Repo.query!(
           "SELECT id FROM attempts WHERE campaign_id = ? AND status IN ('ready_for_integration','refreshing','integrating') ORDER BY ordinal LIMIT 1",
           [campaign_id]
         ).rows do
      [[attempt_id]] -> AttemptStore.attempt(attempt_id)
      [] -> {:error, :integration_queue_empty}
    end
  end

  def lease(campaign_id) do
    case Repo.query!(
           "SELECT id, campaign_id, backend_session_id, attempt_id, intent_id, expected_best_sha, state, acquired_at FROM integration_leases WHERE campaign_id = ?",
           [campaign_id]
         ).rows do
      [row] -> {:ok, lease_from_row(row)}
      [] -> {:error, :integration_lease_missing}
    end
  end

  def acquire_lease(campaign_id, attempt_id, session_id, expected_best_sha) do
    transaction =
      Repo.transaction(fn ->
        campaign = campaign!(campaign_id)
        attempt = queue_head!(campaign_id)

        cond do
          attempt.id != attempt_id ->
            Repo.rollback({:not_queue_head, attempt.id})

          campaign.best_sha != expected_best_sha ->
            Repo.rollback({:stale_expected_best, campaign.best_sha})

          true ->
            acquire_or_recover_lease!(campaign, attempt, session_id)
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:integration_lease_failed, Exception.message(error)}}
  end

  def complete_refresh(lease_id, session_id, attempt_id, new_base_sha, candidate_sha, metrics) do
    with {:ok, lease} <- owned_lease(lease_id, session_id, attempt_id),
         {:ok, _event} <-
           AttemptStore.record_metrics(attempt_id, metrics, candidate_sha, "iteration") do
      transaction =
        Repo.transaction(fn ->
          campaign = campaign!(lease.campaign_id)

          if campaign.best_sha != new_base_sha,
            do: Repo.rollback({:stale_expected_best, campaign.best_sha})

          Repo.query!(
            "UPDATE attempts SET base_sha = ?, candidate_sha = ?, status = 'integrating', resume_state = NULL WHERE id = ?",
            [new_base_sha, candidate_sha, attempt_id]
          )

          event =
            insert_event!("attempt", attempt_id, "attempt_refreshed", %{
              base_sha: new_base_sha,
              candidate_sha: candidate_sha,
              lease_id: lease_id
            })

          {AttemptStore.attempt(attempt_id) |> elem(1), event}
        end)

      publish_transaction(transaction)
    end
  rescue
    error -> {:error, {:attempt_refresh_failed, Exception.message(error)}}
  end

  def issue_receipt(lease_id, session_id, attempt_id, attrs) do
    transaction =
      Repo.transaction(fn ->
        lease = owned_lease!(lease_id, session_id, attempt_id)
        attempt = AttemptStore.attempt(attempt_id) |> unwrap!()
        campaign = campaign!(lease.campaign_id)

        cond do
          attempt.base_sha != campaign.best_sha ->
            Repo.rollback({:stale_attempt_base, campaign.best_sha})

          attempt.candidate_sha != attrs.candidate_sha ->
            Repo.rollback(:candidate_sha_mismatch)

          lease.expected_best_sha != campaign.best_sha ->
            Repo.rollback({:stale_lease, campaign.best_sha})

          true ->
            existing_receipt_or_insert!(lease, attempt, attrs)
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:receipt_issue_failed, Exception.message(error)}}
  end

  def create_merge_intent(lease_id, session_id, attempt_id, receipt_id, key) do
    transaction =
      Repo.transaction(fn ->
        lease = owned_lease!(lease_id, session_id, attempt_id)
        receipt = receipt!(receipt_id)
        campaign = campaign!(lease.campaign_id)
        attempt = AttemptStore.attempt(attempt_id) |> unwrap!()

        cond do
          receipt.status != "passed" ->
            Repo.rollback(:receipt_not_passed)

          receipt.lease_id != lease.id ->
            Repo.rollback(:receipt_lease_mismatch)

          receipt.attempt_id != attempt_id ->
            Repo.rollback(:receipt_attempt_mismatch)

          receipt.base_sha != campaign.best_sha ->
            Repo.rollback({:stale_receipt, campaign.best_sha})

          receipt.candidate_sha != attempt.candidate_sha ->
            Repo.rollback(:receipt_candidate_mismatch)

          true ->
            insert_or_get_merge_intent!(lease, receipt, key)
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:merge_intent_failed, Exception.message(error)}}
  end

  def reject_attempt(
        lease_id,
        session_id,
        attempt_id,
        receipt_id,
        representative_case_ids,
        representative_case_reasons,
        rejection_reason \\ nil
      ) do
    transaction =
      Repo.transaction(fn ->
        lease = owned_lease!(lease_id, session_id, attempt_id)
        receipt = receipt!(receipt_id)
        attempt = AttemptStore.attempt(attempt_id) |> unwrap!()

        if receipt.status != "rejected" or receipt.lease_id != lease.id,
          do: Repo.rollback(:rejected_receipt_required)

        sampling_event =
          maybe_advance_sampling!(
            attempt,
            receipt.regressed_case_ids,
            representative_case_ids,
            representative_case_reasons
          )

        now = now_us()

        Repo.query!(
          "UPDATE attempts SET status = 'rejected', outcome_reason = ?, completed_at = ? WHERE id = ?",
          [
            rejection_outcome(receipt.regressed_case_ids, rejection_reason),
            now,
            attempt_id
          ]
        )

        Repo.query!("DELETE FROM integration_leases WHERE id = ?", [lease.id])

        rejected_event =
          insert_event!("attempt", attempt_id, "attempt_rejected", %{
            receipt_id: receipt.id,
            regressed_case_ids: receipt.regressed_case_ids
          })

        events = Enum.reject([sampling_event, rejected_event], &is_nil/1)
        {AttemptStore.attempt(attempt_id) |> unwrap!(), events}
      end)

    publish_events_transaction(transaction)
  rescue
    error -> {:error, {:attempt_reject_failed, Exception.message(error)}}
  end

  defp rejection_outcome([_ | _] = case_ids, _reason),
    do: "full regression confirmed: #{Enum.join(case_ids, ", ")}"

  defp rejection_outcome([], reason) when is_binary(reason) and reason != "", do: reason
  defp rejection_outcome([], _reason), do: "no meaningful target improvement"

  def complete_merge(lease_id, session_id, attempt_id, receipt_id, intent_id, new_sha) do
    transaction =
      Repo.transaction(fn ->
        lease = owned_lease!(lease_id, session_id, attempt_id)
        receipt = receipt!(receipt_id)
        intent = intent!(intent_id)
        attempt = AttemptStore.attempt(attempt_id) |> unwrap!()
        campaign = campaign!(lease.campaign_id)

        cond do
          receipt.status != "passed" ->
            Repo.rollback(:receipt_not_passed)

          receipt.lease_id != lease.id ->
            Repo.rollback(:receipt_lease_mismatch)

          intent.id != lease.intent_id ->
            Repo.rollback(:intent_lease_mismatch)

          intent.state not in ["pending", "applied"] ->
            Repo.rollback({:invalid_intent_state, intent.state})

          intent.expected_best_sha != campaign.best_sha ->
            Repo.rollback({:stale_merge_intent, campaign.best_sha})

          true ->
            accept_merge!(campaign, attempt, receipt, intent, new_sha)
        end
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:merge_complete_failed, Exception.message(error)}}
  end

  def block(campaign_id, attempt_id, reason) do
    transaction =
      Repo.transaction(fn ->
        [[status, resume_state]] =
          Repo.query!("SELECT status, resume_state FROM campaigns WHERE id = ?", [campaign_id]).rows

        resume_state = if(status == "blocked", do: resume_state, else: status)
        now = now_us()

        Repo.query!(
          "UPDATE campaigns SET status = 'blocked', resume_state = ?, dispatch_gate = 'blocked', updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
          [resume_state, now, campaign_id]
        )

        event =
          insert_event!("campaign", campaign_id, "integration_blocked", %{
            attempt_id: attempt_id,
            reason: inspect(reason)
          })

        {%{campaign_id: campaign_id, status: "blocked", attempt_id: attempt_id}, event}
      end)

    publish_transaction(transaction)
  rescue
    error -> {:error, {:integration_block_failed, Exception.message(error)}}
  end

  def receipt_for_attempt(attempt_id) do
    case Repo.query!(
           "SELECT id, campaign_id, attempt_id, lease_id, base_sha, candidate_sha, harness_digest, status, regressed_case_ids_json, metrics_json, correctness_artifact_id, screening_artifact_id, full_artifact_id, issued_at FROM full_regression_receipts WHERE attempt_id = ? ORDER BY issued_at DESC LIMIT 1",
           [attempt_id]
         ).rows do
      [row] -> {:ok, receipt_from_row(row)}
      [] -> {:error, :receipt_missing}
    end
  end

  def intent_for_attempt(attempt_id) do
    case Repo.query!(
           "SELECT id, campaign_id, kind, owner_type, owner_id, state, expected_best_sha, target_sha, idempotency_key, payload_json, created_at, updated_at FROM operation_intents WHERE kind = 'merge' AND owner_id = ? ORDER BY created_at DESC LIMIT 1",
           [attempt_id]
         ).rows do
      [row] -> {:ok, intent_from_row(row)}
      [] -> {:error, :merge_intent_missing}
    end
  end

  def integration_context(campaign_id, attempt_id) do
    with {:ok, context} <- AttemptStore.campaign_context(campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(attempt_id) do
      {:ok,
       Map.merge(context, %{
         attempt: attempt,
         lease: optional(&lease/1, campaign_id),
         receipt: optional(&receipt_for_attempt/1, attempt_id),
         intent: optional(&intent_for_attempt/1, attempt_id),
         artifacts: AttemptStore.artifacts_for_owner(campaign_id, "attempt", attempt_id)
       })}
    end
  end

  def repair_legacy_target_gate_rejections(campaign_id) do
    transaction =
      Repo.transaction(fn ->
        receipt_ids =
          Repo.query!(
            """
            SELECT id FROM full_regression_receipts
            WHERE campaign_id = ? AND status = 'rejected'
              AND regressed_case_ids_json = '["__target__"]'
            """,
            [campaign_id]
          ).rows
          |> List.flatten()

        if receipt_ids != [] do
          placeholders = Enum.map_join(receipt_ids, ",", fn _ -> "?" end)

          Repo.query!(
            "DELETE FROM full_regression_receipts WHERE id IN (#{placeholders})",
            receipt_ids
          )

          Repo.query!(
            "UPDATE integration_leases SET state = 'lease_acquired' WHERE campaign_id = ? AND state = 'receipt_issued'",
            [campaign_id]
          )

          insert_event!("campaign", campaign_id, "legacy_target_gate_rejections_discarded", %{
            receipt_ids: receipt_ids
          })
        else
          nil
        end
      end)

    case transaction do
      {:ok, nil} ->
        :ok

      {:ok, event} ->
        publish(event)
        :ok

      {:error, reason} ->
        {:error, {:legacy_target_gate_repair_failed, reason}}
    end
  end

  defp acquire_or_recover_lease!(campaign, attempt, session_id) do
    case lease(campaign.campaign_id) do
      {:error, :integration_lease_missing} ->
        lease_id = Ecto.UUID.generate()
        stale? = attempt.base_sha != campaign.best_sha
        status = if(stale?, do: "refreshing", else: "integrating")
        now = now_us()

        Repo.query!(
          "INSERT INTO integration_leases(singleton_key, id, campaign_id, backend_session_id, attempt_id, expected_best_sha, state, acquired_at) VALUES (1, ?, ?, ?, ?, ?, 'lease_acquired', ?)",
          [lease_id, campaign.campaign_id, session_id, attempt.id, campaign.best_sha, now]
        )

        Repo.query!("UPDATE attempts SET status = ? WHERE id = ?", [status, attempt.id])

        event =
          insert_event!("attempt", attempt.id, "integration_lease_acquired", %{
            lease_id: lease_id,
            expected_best_sha: campaign.best_sha,
            stale_base: stale?
          })

        {%{
           id: lease_id,
           attempt_id: attempt.id,
           expected_best_sha: campaign.best_sha,
           stale_base: stale?,
           state: "lease_acquired"
         }, event}

      {:ok, %{attempt_id: id} = existing} when id != attempt.id ->
        Repo.rollback({:integration_lease_held, existing.attempt_id})

      {:ok, existing} ->
        Repo.query!(
          "UPDATE integration_leases SET backend_session_id = ? WHERE id = ?",
          [session_id, existing.id]
        )

        event =
          insert_event!("attempt", attempt.id, "integration_lease_recovered", %{
            lease_id: existing.id,
            backend_session_id: session_id
          })

        {%{
           id: existing.id,
           attempt_id: attempt.id,
           expected_best_sha: existing.expected_best_sha,
           stale_base: attempt.base_sha != campaign.best_sha,
           state: existing.state
         }, event}
    end
  end

  defp existing_receipt_or_insert!(lease, attempt, attrs) do
    case receipt_for_attempt(attempt.id) do
      {:ok, existing} when existing.lease_id == lease.id ->
        {existing, nil}

      {:ok, _other} ->
        Repo.rollback(:receipt_identity_conflict)

      {:error, :receipt_missing} ->
        id = Ecto.UUID.generate()

        status =
          if(attrs[:force_reject] || attrs.regressions != [], do: "rejected", else: "passed")

        regressed_case_ids = attrs.regressions |> Enum.map(&elem(&1, 0)) |> Enum.uniq()
        now = now_us()

        upsert_attempt_metrics!(attempt, attrs.metrics, attrs.candidate_sha, now)

        Repo.query!(
          "INSERT INTO full_regression_receipts(id, campaign_id, attempt_id, lease_id, base_sha, candidate_sha, harness_digest, status, regressed_case_ids_json, metrics_json, correctness_artifact_id, screening_artifact_id, full_artifact_id, issued_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
          [
            id,
            lease.campaign_id,
            attempt.id,
            lease.id,
            attempt.base_sha,
            attrs.candidate_sha,
            attrs.harness_digest,
            status,
            Jason.encode!(regressed_case_ids),
            Jason.encode!(Pika.JSONSafe.json_safe(attrs.metrics)),
            attrs.correctness_artifact_id,
            attrs.screening_artifact_id,
            attrs.full_artifact_id,
            now
          ]
        )

        Repo.query!("UPDATE integration_leases SET state = 'receipt_issued' WHERE id = ?", [
          lease.id
        ])

        receipt = receipt!(id)

        event =
          insert_event!("attempt", attempt.id, "full_regression_#{status}", %{
            receipt_id: id,
            lease_id: lease.id,
            regressed_case_ids: regressed_case_ids
          })

        {receipt, event}
    end
  end

  defp upsert_attempt_metrics!(attempt, metrics, candidate_sha, measured_at) do
    target_snapshot_id = target_snapshot_id!(attempt.spec_revision_id)

    Enum.each(metrics, fn metric ->
      case_name = metric[:case_id] || metric["case_id"]
      metric_name = metric[:metric_id] || metric["metric_id"]

      [[case_id]] =
        Repo.query!(
          "SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?",
          [attempt.spec_revision_id, case_name]
        ).rows

      [[metric_id]] =
        Repo.query!(
          "SELECT id FROM metric_definitions WHERE spec_revision_id = ? AND name = ?",
          [attempt.spec_revision_id, metric_name]
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
          attempt.id,
          case_id,
          metric_id,
          candidate_sha,
          metric[:value] || metric["value"],
          metric[:baseline_value] || metric["baseline_value"],
          metric[:improvement_ratio] || metric["improvement_ratio"],
          metric[:mad] || metric["mad"],
          metric[:noise_tolerance] || metric["noise_tolerance"],
          metric[:pair_count] || metric["pair_count"],
          metric[:valid_pair_count] || metric["valid_pair_count"],
          metric[:source] || metric["source"],
          measured_at,
          target_snapshot_id,
          metric[:target_value] || metric["target_value"],
          metric[:target_relative_improvement] || metric["target_relative_improvement"],
          metric[:best_relative_improvement] || metric["best_relative_improvement"]
        ]
      )
    end)
  end

  defp insert_or_get_merge_intent!(lease, receipt, key) do
    case intent_for_attempt(receipt.attempt_id) do
      {:ok, intent} ->
        {intent, nil}

      {:error, :merge_intent_missing} ->
        id = Ecto.UUID.generate()
        now = now_us()

        Repo.query!(
          "INSERT INTO operation_intents(id, campaign_id, kind, owner_type, owner_id, state, expected_best_sha, target_sha, idempotency_key, payload_json, created_at, updated_at) VALUES (?, ?, 'merge', 'attempt', ?, 'pending', ?, ?, ?, ?, ?, ?)",
          [
            id,
            lease.campaign_id,
            receipt.attempt_id,
            receipt.base_sha,
            receipt.candidate_sha,
            key,
            Jason.encode!(%{receipt_id: receipt.id, lease_id: lease.id}),
            now,
            now
          ]
        )

        Repo.query!(
          "UPDATE integration_leases SET intent_id = ?, state = 'merge_intent' WHERE id = ?",
          [id, lease.id]
        )

        intent = intent!(id)

        event =
          insert_event!("attempt", receipt.attempt_id, "merge_intent_created", %{intent_id: id})

        {intent, event}
    end
  end

  defp maybe_advance_sampling!(
         attempt,
         regressed_case_ids,
         representative_case_ids,
         representative_case_reasons
       ) do
    sampled = sampled_case_ids(attempt.sampling_revision_id)
    unsampled = regressed_case_ids -- sampled
    representatives = Enum.uniq(representative_case_ids)

    cond do
      unsampled == [] ->
        nil

      representatives == [] ->
        Repo.rollback({:representative_case_required, unsampled})

      not MapSet.subset?(MapSet.new(representatives), MapSet.new(unsampled)) ->
        Repo.rollback({:invalid_representative_cases, representatives, unsampled})

      not valid_representative_reasons?(representatives, representative_case_reasons) ->
        Repo.rollback({:representative_case_reasons_required, representatives})

      true ->
        campaign = campaign!(attempt.campaign_id)

        [[sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(sequence), 0) + 1 FROM sampling_revisions WHERE campaign_id = ? AND spec_revision_id = ?",
            [attempt.campaign_id, attempt.spec_revision_id]
          ).rows

        id = Ecto.UUID.generate()
        now = now_us()

        Repo.query!(
          "INSERT INTO sampling_revisions(id, campaign_id, spec_revision_id, sequence, cause, source_attempt_id, summary, estimated_cost_json, created_at) VALUES (?, ?, ?, ?, 'regression_feedback', ?, ?, '{}', ?)",
          [
            id,
            attempt.campaign_id,
            attempt.spec_revision_id,
            sequence,
            attempt.id,
            "Regression feedback from Attempt ##{attempt.ordinal}",
            now
          ]
        )

        all_case_ids = Enum.uniq(sampled ++ representatives)

        Enum.each(all_case_ids, fn case_name ->
          [[case_id]] =
            Repo.query!(
              "SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?",
              [attempt.spec_revision_id, case_name]
            ).rows

          Repo.query!(
            "INSERT INTO sampling_revision_cases(sampling_revision_id, benchmark_case_id, reason, evidence_json) VALUES (?, ?, ?, ?)",
            [
              id,
              case_id,
              if(case_name in sampled,
                do: "carried forward",
                else: "confirmed regression representative"
              ),
              Jason.encode!(%{
                source_attempt_id: attempt.id,
                representative_reason: representative_case_reasons[case_name]
              })
            ]
          )
        end)

        event =
          insert_event!("campaign", campaign.campaign_id, "sampling_advanced", %{
            old_sampling_revision_id: attempt.sampling_revision_id,
            new_sampling_revision_id: id,
            sequence: sequence,
            source_attempt_id: attempt.id,
            added_case_ids: representatives
          })

        notify_active_sessions!(attempt.campaign_id, "sampling_advanced", event.payload)
        event
    end
  end

  defp valid_representative_reasons?(representatives, reasons) when is_map(reasons) do
    Enum.all?(representatives, fn case_name ->
      case reasons[case_name] do
        reason when is_binary(reason) -> String.trim(reason) != ""
        _ -> false
      end
    end)
  end

  defp valid_representative_reasons?(_representatives, _reasons), do: false

  defp accept_merge!(campaign, attempt, receipt, intent, new_sha) do
    now = now_us()

    [[sequence]] =
      Repo.query!(
        "SELECT COALESCE(MAX(sequence), 0) + 1 FROM best_revisions WHERE campaign_id = ?",
        [
          campaign.campaign_id
        ]
      ).rows

    best_revision_id = Ecto.UUID.generate()

    Repo.query!(
      "INSERT INTO best_revisions(id, campaign_id, sequence, sha, cause, attempt_id, spec_revision_id, summary, inserted_at) VALUES (?, ?, ?, ?, 'attempt', ?, ?, ?, ?)",
      [
        best_revision_id,
        campaign.campaign_id,
        sequence,
        new_sha,
        attempt.id,
        attempt.spec_revision_id,
        attempt.summary || "Accepted Attempt ##{attempt.ordinal}",
        now
      ]
    )

    Enum.each(receipt.metrics, fn metric ->
      [[case_id]] =
        Repo.query!(
          "SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?",
          [attempt.spec_revision_id, metric["case_id"]]
        ).rows

      [[metric_id]] =
        Repo.query!(
          "SELECT id FROM metric_definitions WHERE spec_revision_id = ? AND name = ?",
          [attempt.spec_revision_id, metric["metric_id"]]
        ).rows

      Repo.query!(
        "INSERT INTO best_metrics(best_revision_id, benchmark_case_id, metric_definition_id, measured_sha, value, baseline_value, improvement_ratio, mad, noise_tolerance, pair_count, valid_pair_count, source, measured_at, target_snapshot_id, target_value, target_relative_improvement, best_relative_improvement) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        [
          best_revision_id,
          case_id,
          metric_id,
          new_sha,
          metric["value"],
          metric["baseline_value"],
          metric["improvement_ratio"],
          metric["mad"],
          metric["noise_tolerance"],
          metric["pair_count"],
          metric["valid_pair_count"],
          metric["source"],
          now,
          metric["target_snapshot_id"] || target_snapshot_id!(attempt.spec_revision_id),
          metric["target_value"],
          metric["target_relative_improvement"],
          metric["best_relative_improvement"]
        ]
      )
    end)

    Repo.query!(
      "UPDATE campaigns SET best_sha = ?, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
      [new_sha, now, campaign.campaign_id]
    )

    Repo.query!(
      "UPDATE attempts SET status = 'accepted', accepted_sha = ?, completed_at = ? WHERE id = ?",
      [new_sha, now, attempt.id]
    )

    Repo.query!(
      "UPDATE operation_intents SET state = 'verified', target_sha = ?, updated_at = ? WHERE id = ?",
      [new_sha, now, intent.id]
    )

    Repo.query!("DELETE FROM integration_leases WHERE id = ?", [receipt.lease_id])

    payload = %{
      old_sha: campaign.best_sha,
      new_sha: new_sha,
      attempt_id: attempt.id,
      spec_revision_id: attempt.spec_revision_id,
      sampling_revision_id: attempt.sampling_revision_id,
      metric_deltas:
        Enum.map(receipt.metrics, fn metric ->
          %{
            case_id: metric["case_id"],
            metric_id: metric["metric_id"],
            improvement_ratio: metric["improvement_ratio"]
          }
        end)
    }

    event = insert_event!("campaign", campaign.campaign_id, "best_advanced", payload)
    notify_active_sessions!(campaign.campaign_id, "best_advanced", payload)
    {AttemptStore.attempt(attempt.id) |> unwrap!(), event}
  end

  defp target_snapshot_id!(spec_revision_id) do
    case Repo.query!("SELECT target_snapshot_id FROM spec_revisions WHERE id = ?", [
           spec_revision_id
         ]).rows do
      [[id]] when is_binary(id) -> id
      _ -> Repo.rollback(:target_snapshot_missing)
    end
  end

  defp notify_active_sessions!(campaign_id, scope, payload) do
    now = now_us()
    body = Jason.encode!(Pika.JSONSafe.json_safe(payload))

    Repo.query!(
      "INSERT INTO agent_messages(id, campaign_id, from_session_id, to_session_id, scope, body, priority, status, created_at) SELECT lower(hex(randomblob(16))), ?, NULL, id, ?, ?, 'high', 'queued', ? FROM agent_sessions WHERE campaign_id = ? AND status IN ('running','awaiting_report')",
      [campaign_id, scope, body, now, campaign_id]
    )
  end

  defp owned_lease(lease_id, session_id, attempt_id) do
    case Repo.query!(
           "SELECT id, campaign_id, backend_session_id, attempt_id, intent_id, expected_best_sha, state, acquired_at FROM integration_leases WHERE id = ? AND backend_session_id = ? AND attempt_id = ?",
           [lease_id, session_id, attempt_id]
         ).rows do
      [row] -> {:ok, lease_from_row(row)}
      [] -> {:error, :integration_lease_identity_mismatch}
    end
  end

  defp owned_lease!(lease_id, session_id, attempt_id),
    do: owned_lease(lease_id, session_id, attempt_id) |> unwrap!()

  defp campaign!(campaign_id) do
    case Repo.query!(
           "SELECT id, best_sha, current_spec_revision_id FROM campaigns WHERE id = ?",
           [campaign_id]
         ).rows do
      [[id, best_sha, spec_id]] ->
        %{campaign_id: id, best_sha: best_sha, spec_revision_id: spec_id}

      [] ->
        Repo.rollback(:campaign_not_found)
    end
  end

  defp queue_head!(campaign_id), do: queue_head(campaign_id) |> unwrap!()

  defp receipt!(receipt_id) do
    case Repo.query!(
           "SELECT id, campaign_id, attempt_id, lease_id, base_sha, candidate_sha, harness_digest, status, regressed_case_ids_json, metrics_json, correctness_artifact_id, screening_artifact_id, full_artifact_id, issued_at FROM full_regression_receipts WHERE id = ?",
           [receipt_id]
         ).rows do
      [row] -> receipt_from_row(row)
      [] -> Repo.rollback(:receipt_missing)
    end
  end

  defp intent!(intent_id) do
    case Repo.query!(
           "SELECT id, campaign_id, kind, owner_type, owner_id, state, expected_best_sha, target_sha, idempotency_key, payload_json, created_at, updated_at FROM operation_intents WHERE id = ?",
           [intent_id]
         ).rows do
      [row] -> intent_from_row(row)
      [] -> Repo.rollback(:merge_intent_missing)
    end
  end

  defp sampled_case_ids(sampling_revision_id) do
    Repo.query!(
      "SELECT bc.name FROM sampling_revision_cases src JOIN benchmark_cases bc ON bc.id = src.benchmark_case_id WHERE src.sampling_revision_id = ? ORDER BY bc.ordinal",
      [sampling_revision_id]
    ).rows
    |> List.flatten()
  end

  defp insert_event!(aggregate_type, aggregate_id, event_type, payload) do
    event = %{
      sequence: nil,
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

    %{event | sequence: sequence}
  end

  defp publish_transaction({:ok, {value, nil}}), do: {:ok, value}

  defp publish_transaction({:ok, {value, event}}) do
    publish(event)
    {:ok, value}
  end

  defp publish_transaction({:error, reason}), do: {:error, reason}

  defp publish_events_transaction({:ok, {value, events}}) do
    Enum.each(events, &publish/1)
    {:ok, value}
  end

  defp publish_events_transaction({:error, reason}), do: {:error, reason}

  defp publish(event) do
    if Process.whereis(Pika.PubSub) do
      campaign_id = campaign_id_for_event(event)

      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        Persistence.topic(campaign_id),
        {:domain_event, event}
      )

      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        "pika:optimization:events",
        {:integration_event, event}
      )
    end
  end

  defp campaign_id_for_event(%{aggregate_type: "campaign", aggregate_id: campaign_id}),
    do: campaign_id

  defp campaign_id_for_event(%{aggregate_type: "attempt", aggregate_id: attempt_id}) do
    [[campaign_id]] =
      Repo.query!("SELECT campaign_id FROM attempts WHERE id = ?", [attempt_id]).rows

    campaign_id
  end

  defp lease_from_row([id, campaign, session, attempt, intent, best, state, acquired]) do
    %{
      id: id,
      campaign_id: campaign,
      backend_session_id: session,
      attempt_id: attempt,
      intent_id: intent,
      expected_best_sha: best,
      state: state,
      acquired_at: acquired
    }
  end

  defp receipt_from_row([
         id,
         campaign,
         attempt,
         lease,
         base,
         candidate,
         digest,
         status,
         regressed,
         metrics,
         correctness,
         screening,
         full,
         issued
       ]) do
    %{
      id: id,
      campaign_id: campaign,
      attempt_id: attempt,
      lease_id: lease,
      base_sha: base,
      candidate_sha: candidate,
      harness_digest: digest,
      status: status,
      regressed_case_ids: Jason.decode!(regressed),
      metrics: Jason.decode!(metrics),
      correctness_artifact_id: correctness,
      screening_artifact_id: screening,
      full_artifact_id: full,
      issued_at: issued
    }
  end

  defp intent_from_row([
         id,
         campaign,
         kind,
         owner_type,
         owner_id,
         state,
         expected,
         target,
         key,
         payload,
         created,
         updated
       ]) do
    %{
      id: id,
      campaign_id: campaign,
      kind: kind,
      owner_type: owner_type,
      owner_id: owner_id,
      state: state,
      expected_best_sha: expected,
      target_sha: target,
      idempotency_key: key,
      payload: Jason.decode!(payload),
      created_at: created,
      updated_at: updated
    }
  end

  defp optional(fun, arg) do
    case fun.(arg) do
      {:ok, value} -> value
      _ -> nil
    end
  end

  defp unwrap!({:ok, value}), do: value
  defp unwrap!({:error, reason}), do: Repo.rollback(reason)
  defp now_us, do: System.system_time(:microsecond)
end
