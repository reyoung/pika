defmodule Pika.Integration.Lifecycle do
  @moduledoc "Serial v2 Integration validation, Git intent, and Best advancement interface."

  alias Pika.Agent.RolePromptRegistry
  alias Pika.Attempt.{History, Lifecycle, Scheduler}
  alias Pika.Baseline.TargetSnapshot
  alias Pika.Integration.Decision
  alias Pika.Optimization.{ArtifactStore, Config, FileContract, Measurement, Persistence}
  alias Pika.{Git, Repo}

  @optimization_id "optimization"

  @spec project_work() :: [map()]
  def project_work do
    case Scheduler.next_queue_action() do
      {:integrate, attempt} -> [work_projection(attempt)]
      {:waiting, %{status: "integrating"} = attempt} -> [work_projection(attempt)]
      {:refresh, _attempt} -> []
      {:waiting, _attempt} -> []
      {:none, nil} -> []
    end
  end

  @spec prepare_best_update(pos_integer(), Path.t(), Path.t()) :: {:ok, map()} | {:error, term()}
  def prepare_best_update(attempt_id, attempt_root, validation_path) do
    with {:ok, attempt} <- Lifecycle.fetch_attempt(attempt_id),
         :ok <- require_queue_head(attempt),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, validation, validation_receipt} <-
           FileContract.validate_json(
             attempt_root,
             validation_path,
             schemas.integration_validation
           ),
         :ok <- validate_validation_identity(attempt, validation),
         {:ok, context} <- full_context(attempt),
         {:ok, verify, verify_receipt} <-
           validate_file(attempt_root, validation, "verify", schemas.verify_result, :json),
         {:ok, benchmark, benchmark_receipt} <-
           validate_file(
             attempt_root,
             validation,
             "benchmark",
             schemas.benchmark_record,
             :jsonl
           ),
         :ok <- validate_full_verify(verify, context.cases),
         {:ok, statistics} <-
           Measurement.evaluate(benchmark, context.cases, context.metrics, context.measurement),
         {:ok, decision} <-
           Decision.evaluate(
             statistics,
             context.best_metrics,
             context.cases,
             context.metrics,
             validation["judgements"]
           ),
         :ok <- require_accepted_decision(validation, decision),
         :ok <- validate_reported_aggregates(validation, decision),
         {:ok, artifact_ids} <-
           register_validation_artifacts(
             attempt,
             validation_receipt,
             verify_receipt,
             benchmark_receipt
           ),
         {:ok, prepared} <-
           persist_prepared(
             attempt,
             validation,
             validation_receipt,
             statistics,
             decision,
             artifact_ids
           ) do
      {:ok, prepared}
    end
  end

  @spec finish(pos_integer(), Path.t(), Path.t(), Config.t()) :: {:ok, map()} | {:error, term()}
  def finish(attempt_id, attempt_root, result_path, %Config{} = config) do
    with {:ok, attempt} <- Lifecycle.fetch_attempt(attempt_id),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, result, result_receipt} <-
           FileContract.validate_json(attempt_root, result_path, schemas.integration_result),
         :ok <- validate_result_identity(attempt, result),
         {:ok, updated} <- finish_outcome(attempt, attempt_root, result, result_receipt, config) do
      {:ok, updated}
    end
  end

  defp finish_outcome(attempt, attempt_root, %{"outcome" => "rejected"} = result, receipt, config) do
    with :ok <- require_rejectable(attempt),
         :ok <- require_best_unchanged_for_reject(attempt, config.repo),
         :ok <- validate_feedback(result, config.integration.regression_feedback_cases),
         {:ok, result_artifact_id} <-
           ArtifactStore.register("attempt", to_string(attempt.id), "integration_result", receipt),
         {:ok, updated} <- persist_rejected(attempt, result, result_artifact_id),
         :ok <- record_history(attempt_root, updated, result) do
      {:ok, updated}
    end
  end

  defp finish_outcome(attempt, attempt_root, %{"outcome" => "accepted"} = result, receipt, config) do
    with :ok <- require_integrating(attempt),
         {:ok, run} <- integration_run(attempt.id),
         {:ok, intent} <- operation_intent(run.intent_id),
         :ok <- validate_accept_feedback(run, config.integration.regression_feedback_cases),
         :ok <- validate_accept_identity(attempt, result, run, intent),
         :ok <- verify_best_git(config.repo, attempt, result, intent),
         {:ok, result_artifact_id} <-
           ArtifactStore.register("attempt", to_string(attempt.id), "integration_result", receipt),
         {:ok, updated} <- persist_accepted(attempt, result, run, intent, result_artifact_id),
         :ok <- record_history(attempt_root, updated, result) do
      {:ok, updated}
    end
  end

  defp persist_prepared(attempt, validation, receipt, statistics, decision, artifact_ids) do
    case integration_run(attempt.id) do
      {:ok, %{status: "best_update_prepared", validation_sha256: sha256} = run}
      when sha256 == receipt.sha256 ->
        {:ok, %{run: run, intent: operation_intent!(run.intent_id), decision: decision}}

      {:ok, run} ->
        {:error, {:integration_already_active, run.status}}

      {:error, :integration_run_not_found} ->
        insert_prepared(attempt, validation, receipt, statistics, decision, artifact_ids)
    end
  end

  defp insert_prepared(attempt, validation, receipt, statistics, decision, artifact_ids) do
    now = now_us()
    intent_id = Ecto.UUID.generate()

    transaction =
      Repo.transaction(fn ->
        [[fifo_sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(fifo_sequence), 0) + 1 FROM integration_runs WHERE optimization_id = ?",
            [@optimization_id]
          ).rows

        Repo.query!(
          """
          INSERT INTO integration_runs(
            optimization_id, attempt_id, fifo_sequence, status, expected_best_sha,
            validation_artifact_id, verify_artifact_id, benchmark_artifact_id,
            validation_sha256, statistics_json, judgement_json, sampling_feedback_json,
            created_at, updated_at
          ) VALUES (?, ?, ?, 'best_update_prepared', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
          """,
          [
            @optimization_id,
            attempt.id,
            fifo_sequence,
            attempt.base_sha,
            artifact_ids.validation,
            artifact_ids.verify,
            artifact_ids.benchmark,
            receipt.sha256,
            Jason.encode!(statistics),
            Jason.encode!(decision),
            Jason.encode!(validation["sampling_feedback_case_ids"]),
            now,
            now
          ]
        )

        [[run_id]] = Repo.query!("SELECT last_insert_rowid()").rows

        Repo.query!(
          """
          INSERT INTO operation_intents(
            id, optimization_id, kind, owner_type, owner_id, state,
            expected_best_sha, candidate_sha, validation_receipt_sha256,
            idempotency_key, payload_json, created_at, updated_at
          ) VALUES (?, ?, 'best_update', 'attempt', ?, 'pending', ?, ?, ?, ?, ?, ?, ?)
          """,
          [
            intent_id,
            @optimization_id,
            to_string(attempt.id),
            attempt.base_sha,
            validation["candidate_sha"],
            receipt.sha256,
            "best-update:#{attempt.id}:#{receipt.sha256}",
            Jason.encode!(%{integration_run_id: run_id}),
            now,
            now
          ]
        )

        Repo.query!(
          "UPDATE attempts SET status = 'integrating', updated_at = ? WHERE id = ? AND status = 'ready_for_integration'",
          [now, attempt.id]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "integration_best_update_prepared",
          %{intent_id: intent_id},
          now
        )

        run = integration_run!(attempt.id)
        %{run: run, intent: operation_intent!(intent_id), decision: decision}
      end)

    finish_transaction(transaction, :integration_prepare_failed)
  end

  defp persist_rejected(attempt, result, result_artifact_id) do
    now = now_us()
    details = result["details"]

    transaction =
      Repo.transaction(fn ->
        run_id = ensure_rejected_run(attempt, result_artifact_id, now)

        sampling_revision_id =
          append_sampling_feedback(attempt, details["sampling_feedback"], now)

        Repo.query!(
          """
          UPDATE attempts
          SET status = 'rejected', outcome = 'rejected', summary = ?, failure_reason = ?, updated_at = ?
          WHERE id = ? AND status IN ('ready_for_integration', 'integrating')
          """,
          [result["summary"], details["reason"], now, attempt.id]
        )

        Repo.query!(
          "UPDATE integration_runs SET status = 'rejected', outcome = 'rejected', result_artifact_id = ?, updated_at = ? WHERE id = ?",
          [result_artifact_id, now, run_id]
        )

        Repo.query!(
          "UPDATE operation_intents SET state = 'aborted', updated_at = ? WHERE owner_type = 'attempt' AND owner_id = ? AND state = 'pending'",
          [now, to_string(attempt.id)]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "integration_rejected",
          %{sampling_revision_id: sampling_revision_id},
          now
        )

        fetch_attempt!(attempt.id)
      end)

    finish_transaction(transaction, :integration_rejection_failed)
  end

  defp persist_accepted(attempt, result, run, intent, result_artifact_id) do
    now = now_us()
    details = result["details"]
    statistics = Jason.decode!(run.statistics_json)

    transaction =
      Repo.transaction(fn ->
        [[sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(sequence), -1) + 1 FROM best_revisions WHERE optimization_id = ?",
            [@optimization_id]
          ).rows

        [[baseline_revision_id]] =
          Repo.query!(
            "SELECT baseline_revision_id FROM sampling_revisions WHERE id = ?",
            [attempt.sampling_revision_id]
          ).rows

        sampling_feedback_revision_id = append_accepted_sampling_feedback(attempt, run, now)

        Repo.query!(
          """
          INSERT INTO best_revisions(
            optimization_id, sequence, sha, source_kind, source_attempt_id,
            baseline_revision_id, summary, created_at
          ) VALUES (?, ?, ?, 'attempt', ?, ?, ?, ?)
          """,
          [
            @optimization_id,
            sequence,
            details["best_after_sha"],
            attempt.id,
            baseline_revision_id,
            result["summary"],
            now
          ]
        )

        [[best_revision_id]] = Repo.query!("SELECT last_insert_rowid()").rows

        Enum.each(statistics, fn statistic ->
          Repo.query!(
            """
            INSERT INTO best_metrics(
              best_revision_id, case_id, metric_id, target_value, development_value,
              normalized_ratio, mad, noise_tolerance, valid_pair_count, source_artifact_id
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            [
              best_revision_id,
              statistic["case_id"],
              statistic["metric_id"],
              statistic["target_value"],
              statistic["development_value"],
              statistic["normalized_ratio"],
              statistic["mad"],
              statistic["noise_tolerance"],
              statistic["valid_pair_count"],
              run.benchmark_artifact_id
            ]
          )
        end)

        Repo.query!(
          """
          UPDATE attempts
          SET status = 'accepted', outcome = 'accepted', summary = ?, candidate_sha = ?, updated_at = ?
          WHERE id = ? AND status = 'integrating'
          """,
          [result["summary"], details["best_after_sha"], now, attempt.id]
        )

        Repo.query!(
          """
          UPDATE integration_runs
          SET status = 'accepted', outcome = 'accepted', result_artifact_id = ?, updated_at = ?
          WHERE id = ? AND status = 'best_update_prepared'
          """,
          [result_artifact_id, now, run.id]
        )

        Repo.query!(
          "UPDATE operation_intents SET state = 'verified', updated_at = ? WHERE id = ? AND state = 'pending'",
          [now, intent.id]
        )

        Repo.query!(
          "UPDATE optimizations SET best_sha = ?, updated_at = ? WHERE id = ?",
          [details["best_after_sha"], now, @optimization_id]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "integration_accepted",
          %{
            best_sha: details["best_after_sha"],
            best_revision: sequence,
            sampling_revision_id: sampling_feedback_revision_id
          },
          now
        )

        fetch_attempt!(attempt.id)
      end)

    finish_transaction(transaction, :integration_accept_failed)
  end

  defp ensure_rejected_run(attempt, result_artifact_id, now) do
    case integration_run(attempt.id) do
      {:ok, run} ->
        run.id

      {:error, :integration_run_not_found} ->
        [[fifo_sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(fifo_sequence), 0) + 1 FROM integration_runs WHERE optimization_id = ?",
            [@optimization_id]
          ).rows

        Repo.query!(
          """
          INSERT INTO integration_runs(
            optimization_id, attempt_id, fifo_sequence, status, expected_best_sha,
            result_artifact_id, created_at, updated_at
          ) VALUES (?, ?, ?, 'rejected', ?, ?, ?, ?)
          """,
          [
            @optimization_id,
            attempt.id,
            fifo_sequence,
            attempt.base_sha,
            result_artifact_id,
            now,
            now
          ]
        )

        [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
        id
    end
  end

  defp append_sampling_feedback(_attempt, [], _now), do: nil

  defp append_sampling_feedback(attempt, feedback, now) do
    [[baseline_revision_id]] =
      Repo.query!("SELECT baseline_revision_id FROM sampling_revisions WHERE id = ?", [
        attempt.sampling_revision_id
      ]).rows

    [[current_sampling_id, current_sequence]] =
      Repo.query!(
        """
        SELECT id, sequence FROM sampling_revisions
        WHERE optimization_id = ? AND baseline_revision_id = ?
        ORDER BY sequence DESC LIMIT 1
        """,
        [@optimization_id, baseline_revision_id]
      ).rows

    current_ids = sampling_case_ids(current_sampling_id)
    feedback_ids = Enum.map(feedback, & &1["case_id"])
    additions = feedback_ids -- current_ids

    if additions == [] do
      nil
    else
      next_sequence = current_sequence + 1

      Repo.query!(
        """
        INSERT INTO sampling_revisions(
          optimization_id, baseline_revision_id, sequence, cause, created_at
        ) VALUES (?, ?, ?, 'integration_feedback', ?)
        """,
        [@optimization_id, baseline_revision_id, next_sequence, now]
      )

      [[new_id]] = Repo.query!("SELECT last_insert_rowid()").rows

      old_reasons =
        Repo.query!(
          "SELECT case_id, reason FROM sampling_revision_cases WHERE sampling_revision_id = ?",
          [current_sampling_id]
        ).rows

      Enum.each(old_reasons, fn [case_id, reason] ->
        Repo.query!(
          "INSERT INTO sampling_revision_cases(sampling_revision_id, case_id, reason) VALUES (?, ?, ?)",
          [new_id, case_id, reason]
        )
      end)

      reason_by_id = Map.new(feedback, &{&1["case_id"], &1["reason"]})

      Enum.each(additions, fn case_id ->
        Repo.query!(
          "INSERT INTO sampling_revision_cases(sampling_revision_id, case_id, reason) VALUES (?, ?, ?)",
          [new_id, case_id, Map.fetch!(reason_by_id, case_id)]
        )
      end)

      new_id
    end
  end

  defp validate_feedback(result, limit) do
    details = result["details"]
    regressed = MapSet.new(details["regressed_case_ids"])
    feedback = details["sampling_feedback"]
    feedback_ids = Enum.map(feedback, & &1["case_id"])

    cond do
      length(feedback) > limit ->
        {:error, {:sampling_feedback_limit_exceeded, limit}}

      Enum.uniq(feedback_ids) != feedback_ids ->
        {:error, :duplicate_sampling_feedback}

      not MapSet.subset?(MapSet.new(feedback_ids), regressed) ->
        {:error, :sampling_feedback_not_regressed}

      not full_case_subset?(feedback_ids) ->
        {:error, :sampling_feedback_unknown_cases}

      true ->
        :ok
    end
  end

  defp full_case_subset?(ids) do
    known =
      Repo.query!(
        """
        SELECT case_id FROM benchmark_cases
        WHERE baseline_revision_id = (
          SELECT id FROM baseline_revisions WHERE optimization_id = ? AND status = 'accepted'
          ORDER BY revision DESC LIMIT 1
        )
        """,
        [@optimization_id]
      ).rows
      |> List.flatten()
      |> MapSet.new()

    MapSet.subset?(MapSet.new(ids), known)
  end

  defp validate_validation_identity(attempt, validation) do
    cond do
      validation["role"] != "integration" ->
        {:error, :integration_role_mismatch}

      validation["work_id"] != to_string(attempt.id) ->
        {:error, :integration_work_mismatch}

      validation["attempt_id"] != attempt.id ->
        {:error, :integration_attempt_mismatch}

      validation["base_sha"] != attempt.base_sha ->
        {:error, :integration_base_mismatch}

      validation["candidate_sha"] != attempt.candidate_sha ->
        {:error, :integration_candidate_mismatch}

      validation["sampling_revision"] != sampling_sequence(attempt.sampling_revision_id) ->
        {:error, :integration_sampling_mismatch}

      true ->
        :ok
    end
  end

  defp validate_result_identity(attempt, result) do
    cond do
      result["role"] != "integration" -> {:error, :integration_role_mismatch}
      result["work_id"] != to_string(attempt.id) -> {:error, :integration_work_mismatch}
      result["attempt_id"] != attempt.id -> {:error, :integration_attempt_mismatch}
      true -> :ok
    end
  end

  defp require_queue_head(attempt) do
    case Scheduler.next_queue_action() do
      {:integrate, %{id: id}} when id == attempt.id -> :ok
      {:waiting, %{id: id, status: "integrating"}} when id == attempt.id -> :ok
      {:integrate, head} -> {:error, {:not_integration_queue_head, head.id}}
      {:refresh, _head} -> {:error, :attempt_became_stale}
      {:waiting, head} -> {:error, {:integration_queue_waiting, head.id}}
      {:none, nil} -> {:error, :integration_queue_empty}
    end
  end

  defp require_accepted_decision(validation, decision) do
    cond do
      validation["recommended_outcome"] != "accepted" ->
        {:error, :prepare_requires_accepted_recommendation}

      decision.outcome != :accepted ->
        {:error, {:integration_hard_gate_rejected, decision}}

      true ->
        :ok
    end
  end

  defp validate_reported_aggregates(validation, decision) do
    reported =
      Map.new(validation["weighted_aggregates"], &{&1["metric_id"], &1["regression_ratio"]})

    expected = Map.new(decision.weighted_aggregates, &{&1.metric_id, &1.regression_ratio})

    valid? =
      MapSet.new(Map.keys(reported)) == MapSet.new(Map.keys(expected)) and
        Enum.all?(expected, fn {metric_id, value} -> close?(reported[metric_id], value) end)

    if valid?, do: :ok, else: {:error, :integration_aggregate_mismatch}
  end

  defp full_context(attempt) do
    [[baseline_revision_id]] =
      Repo.query!("SELECT baseline_revision_id FROM sampling_revisions WHERE id = ?", [
        attempt.sampling_revision_id
      ]).rows

    [[target_snapshot_id]] =
      Repo.query!("SELECT target_snapshot_id FROM baseline_revisions WHERE id = ?", [
        baseline_revision_id
      ]).rows

    with :ok <- TargetSnapshot.verify(target_snapshot_id),
         do: full_context_verified(attempt, baseline_revision_id)
  end

  defp full_context_verified(_attempt, baseline_revision_id) do
    cases =
      Repo.query!(
        "SELECT case_id, name, description, inputs_json, weight, critical FROM benchmark_cases WHERE baseline_revision_id = ? ORDER BY case_id",
        [baseline_revision_id]
      ).rows
      |> Enum.map(fn [id, name, description, inputs, weight, critical] ->
        %{
          "id" => id,
          "name" => name,
          "description" => description,
          "inputs" => Jason.decode!(inputs),
          "weight" => weight,
          "critical" => critical in [1, true, "1", "true"]
        }
      end)

    metrics =
      Repo.query!(
        "SELECT metric_id, unit, direction, role, aggregation_json FROM metric_definitions WHERE baseline_revision_id = ? ORDER BY rowid",
        [baseline_revision_id]
      ).rows
      |> Enum.map(fn [id, unit, direction, role, aggregation_json] ->
        %{"id" => id, "unit" => unit, "direction" => direction, "role" => role}
        |> Map.merge(Jason.decode!(aggregation_json))
      end)

    [[work_relative_path]] =
      Repo.query!("SELECT work_relative_path FROM baseline_revisions WHERE id = ?", [
        baseline_revision_id
      ]).rows

    manifest =
      Persistence.current().workspace_canonical_path
      |> Path.join(work_relative_path)
      |> Path.join("baseline-definition.json")
      |> File.read!()
      |> Jason.decode!()

    [[best_revision_id]] =
      Repo.query!(
        "SELECT id FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1",
        [@optimization_id]
      ).rows

    best_metrics =
      Repo.query!(
        "SELECT case_id, metric_id, normalized_ratio, noise_tolerance FROM best_metrics WHERE best_revision_id = ?",
        [best_revision_id]
      ).rows
      |> Enum.map(fn [case_id, metric_id, ratio, noise] ->
        %{case_id: case_id, metric_id: metric_id, normalized_ratio: ratio, noise_tolerance: noise}
      end)

    {:ok,
     %{
       cases: cases,
       metrics: metrics,
       measurement: manifest["measurement"],
       best_metrics: best_metrics
     }}
  end

  defp validate_file(root, validation, key, schema_path, :json) do
    with {:ok, file} <- validation_file(validation, key),
         {:ok, value, receipt} <- FileContract.validate_json(root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp validate_file(root, validation, key, schema_path, :jsonl) do
    with {:ok, file} <- validation_file(validation, key),
         {:ok, value, receipt} <- FileContract.validate_jsonl(root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp validation_file(validation, key) do
    case get_in(validation, ["files", key]) do
      %{"path" => path, "sha256" => sha256} = file when is_binary(path) and is_binary(sha256) ->
        {:ok, file}

      _other ->
        {:error, {:integration_file_missing, key}}
    end
  end

  defp expected_digest(%{"sha256" => sha256}, %{sha256: sha256}), do: :ok

  defp expected_digest(file, receipt),
    do: {:error, {:file_digest_mismatch, file["path"], file["sha256"], receipt.sha256}}

  defp validate_full_verify(verify, cases) do
    ids = Enum.map(cases, & &1["id"])
    actual = Enum.map(verify["cases"], & &1["case_id"])

    passed? =
      verify["requested_case_ids"] == ids and actual == ids and
        Enum.all?(verify["cases"], fn result ->
          result["target"]["passed"] == true and result["candidate"]["passed"] == true and
            result["comparison"]["passed"] == true
        end)

    if passed?, do: :ok, else: {:error, :integration_verify_failed_or_incomplete}
  end

  defp register_validation_artifacts(attempt, validation, verify, benchmark) do
    owner_id = to_string(attempt.id)

    with {:ok, validation_id} <-
           ArtifactStore.register("attempt", owner_id, "integration_validation", validation),
         {:ok, verify_id} <-
           ArtifactStore.register("attempt", owner_id, "integration_verify", verify),
         {:ok, benchmark_id} <-
           ArtifactStore.register("attempt", owner_id, "integration_benchmark", benchmark) do
      {:ok, %{validation: validation_id, verify: verify_id, benchmark: benchmark_id}}
    end
  end

  defp validate_accept_identity(attempt, result, run, intent) do
    details = result["details"]
    trailers = details["trailers"]

    cond do
      details["intent_id"] != intent.id ->
        {:error, :integration_intent_mismatch}

      intent.state != "pending" ->
        {:error, :integration_intent_not_pending}

      intent.expected_best_sha != run.expected_best_sha or
          details["best_before_sha"] != run.expected_best_sha ->
        {:error, :integration_best_before_mismatch}

      intent.candidate_sha != attempt.candidate_sha ->
        {:error, :integration_candidate_mismatch}

      details["best_after_sha"] != details["squash_sha"] ->
        {:error, :integration_squash_mismatch}

      trailers != expected_trailers(attempt) ->
        {:error, :integration_trailer_identity_mismatch}

      true ->
        :ok
    end
  end

  defp validate_accept_feedback(run, limit) do
    feedback_ids = Jason.decode!(run.sampling_feedback_json)
    decision = Jason.decode!(run.judgement_json)
    regressed = MapSet.new(decision["regressed_case_ids"] || [])

    cond do
      length(feedback_ids) > limit ->
        {:error, {:sampling_feedback_limit_exceeded, limit}}

      Enum.uniq(feedback_ids) != feedback_ids ->
        {:error, :duplicate_sampling_feedback}

      not MapSet.subset?(MapSet.new(feedback_ids), regressed) ->
        {:error, :sampling_feedback_not_regressed}

      not full_case_subset?(feedback_ids) ->
        {:error, :sampling_feedback_unknown_cases}

      true ->
        :ok
    end
  end

  defp append_accepted_sampling_feedback(attempt, run, now) do
    run.sampling_feedback_json
    |> Jason.decode!()
    |> Enum.map(fn case_id ->
      %{
        "case_id" => case_id,
        "reason" => "Integration feedback from an accepted full-set validation"
      }
    end)
    |> then(&append_sampling_feedback(attempt, &1, now))
  end

  defp expected_trailers(attempt) do
    [[baseline_revision]] =
      Repo.query!(
        """
        SELECT br.revision
        FROM baseline_revisions br
        JOIN sampling_revisions sr ON sr.baseline_revision_id = br.id
        WHERE sr.id = ?
        """,
        [attempt.sampling_revision_id]
      ).rows

    %{
      "Pika-Attempt" => to_string(attempt.id),
      "Pika-Baseline-Revision" => to_string(baseline_revision),
      "Pika-Sampling-Revision" => to_string(sampling_sequence(attempt.sampling_revision_id))
    }
  end

  defp verify_best_git(best_repo, attempt, result, intent) do
    details = result["details"]
    best_after = details["best_after_sha"]

    with {:ok, actual_best} <- Git.run(best_repo, ["rev-parse", "pika/best"]),
         :ok <- equal(actual_best, best_after, :best_branch_not_advanced),
         {:ok, parent} <- Git.run(best_repo, ["rev-parse", "#{best_after}^"]),
         :ok <- equal(parent, intent.expected_best_sha, :best_parent_mismatch),
         {:ok, _output} <-
           Git.run(best_repo, ["diff", "--quiet", attempt.candidate_sha, best_after]),
         {:ok, changed} <-
           Git.run(best_repo, [
             "diff",
             "--name-only",
             "#{intent.expected_best_sha}..#{best_after}"
           ]),
         :ok <- protected_paths_unchanged(changed),
         {:ok, message} <- Git.run(best_repo, ["show", "-s", "--format=%B", best_after]),
         :ok <- validate_trailers(message, details["trailers"]) do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp protected_paths_unchanged(changed) do
    protected? =
      changed
      |> String.split("\n", trim: true)
      |> Enum.any?(
        &(&1 in ["verify_cases.sh", "benchmark_cases.sh"] or String.starts_with?(&1, "target/"))
      )

    if protected?, do: {:error, :protected_path_changed}, else: :ok
  end

  defp validate_trailers(message, trailers) do
    valid? =
      Enum.all?(trailers, fn {key, value} -> String.contains?(message, "#{key}: #{value}") end)

    if valid?, do: :ok, else: {:error, :integration_trailers_missing}
  end

  defp require_rejectable(%{status: status})
       when status in ["ready_for_integration", "integrating"],
       do: :ok

  defp require_rejectable(attempt), do: {:error, {:attempt_not_rejectable, attempt.status}}

  defp require_best_unchanged_for_reject(%{status: "ready_for_integration"}, _repo), do: :ok

  defp require_best_unchanged_for_reject(%{status: "integrating", id: attempt_id}, repo) do
    with {:ok, run} <- integration_run(attempt_id),
         {:ok, actual} <- Git.run(repo, ["rev-parse", "pika/best"]) do
      if actual == run.expected_best_sha,
        do: :ok,
        else: {:error, {:best_mutated_pending_intent, run.expected_best_sha, actual}}
    end
  end

  defp require_integrating(%{status: "integrating"}), do: :ok
  defp require_integrating(attempt), do: {:error, {:attempt_not_integrating, attempt.status}}

  defp integration_run(attempt_id) do
    case Repo.query!(
           """
           SELECT id, status, expected_best_sha, validation_sha256, statistics_json,
                  benchmark_artifact_id, judgement_json, sampling_feedback_json, outcome
           FROM integration_runs WHERE attempt_id = ?
           """,
           [attempt_id]
         ).rows do
      [
        [
          id,
          status,
          expected,
          validation_sha,
          statistics,
          benchmark_artifact_id,
          judgement_json,
          sampling_feedback_json,
          outcome
        ]
      ] ->
        intent_id =
          case Repo.query!(
                 "SELECT id FROM operation_intents WHERE owner_type = 'attempt' AND owner_id = ? ORDER BY created_at DESC LIMIT 1",
                 [to_string(attempt_id)]
               ).rows do
            [[id]] -> id
            [] -> nil
          end

        {:ok,
         %{
           id: id,
           status: status,
           expected_best_sha: expected,
           validation_sha256: validation_sha,
           statistics_json: statistics,
           benchmark_artifact_id: benchmark_artifact_id,
           judgement_json: judgement_json,
           sampling_feedback_json: sampling_feedback_json,
           outcome: outcome,
           intent_id: intent_id
         }}

      [] ->
        {:error, :integration_run_not_found}
    end
  end

  defp integration_run!(attempt_id) do
    {:ok, run} = integration_run(attempt_id)
    run
  end

  defp operation_intent(id) do
    case Repo.query!(
           "SELECT id, state, expected_best_sha, candidate_sha, validation_receipt_sha256 FROM operation_intents WHERE id = ?",
           [id]
         ).rows do
      [[id, state, expected, candidate, validation_sha]] ->
        {:ok,
         %{
           id: id,
           state: state,
           expected_best_sha: expected,
           candidate_sha: candidate,
           validation_receipt_sha256: validation_sha
         }}

      [] ->
        {:error, :operation_intent_not_found}
    end
  end

  defp operation_intent!(id) do
    {:ok, intent} = operation_intent(id)
    intent
  end

  defp fetch_attempt!(attempt_id) do
    {:ok, attempt} = Lifecycle.fetch_attempt(attempt_id)
    attempt
  end

  defp sampling_case_ids(id) do
    Repo.query!("SELECT case_id FROM sampling_revision_cases WHERE sampling_revision_id = ?", [id]).rows
    |> List.flatten()
  end

  defp sampling_sequence(id) do
    [[sequence]] = Repo.query!("SELECT sequence FROM sampling_revisions WHERE id = ?", [id]).rows
    sequence
  end

  defp record_history(attempt_root, attempt, result) do
    History.record(attempt_root, attempt.id, %{
      attempt_id: attempt.id,
      iteration_round: attempt.current_iteration_round,
      summary: result["summary"],
      status: attempt.status,
      outcome: result["outcome"],
      failure_reason: attempt.failure_reason
    })
  end

  defp work_projection(attempt) do
    %{
      role_id: "integration",
      work_kind: :attempt,
      work_id: to_string(attempt.id),
      attempt: attempt
    }
  end

  defp append_event(aggregate_type, aggregate_id, event_type, payload, now) do
    Repo.query!(
      "INSERT INTO domain_events(event_id, optimization_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
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

  defp close?(left, right) when is_number(left) and is_number(right),
    do: abs(left - right) <= max(1.0e-9, abs(right) * 1.0e-9)

  defp close?(_left, _right), do: false
  defp equal(value, value, _reason), do: :ok
  defp equal(_actual, _expected, reason), do: {:error, reason}
  defp finish_transaction({:ok, value}, _tag), do: {:ok, value}
  defp finish_transaction({:error, reason}, tag), do: {:error, {tag, reason}}
  defp now_us, do: System.system_time(:microsecond)
end
