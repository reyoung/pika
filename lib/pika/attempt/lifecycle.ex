defmodule Pika.Attempt.Lifecycle do
  @moduledoc "The sole v2 Iteration Result write interface for an Attempt."

  alias Pika.Agent.RolePromptRegistry
  alias Pika.Attempt.History
  alias Pika.Optimization.{ArtifactStore, FileContract, Measurement, Persistence}
  alias Pika.{Git, Repo}

  @optimization_id "optimization"
  @open_statuses ~w(iterating refreshing_iteration)

  @spec finish(pos_integer(), Path.t(), Path.t()) :: {:ok, map()} | {:error, term()}
  def finish(attempt_id, attempt_root, result_path)
      when is_integer(attempt_id) and attempt_id > 0 and is_binary(attempt_root) and
             is_binary(result_path) do
    with {:ok, attempt} <- fetch_attempt(attempt_id),
         :ok <- require_open(attempt),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, result, result_receipt} <-
           FileContract.validate_json(attempt_root, result_path, schemas.iteration_result),
         :ok <- validate_identity(attempt, result),
         {:ok, updated} <-
           finish_outcome(attempt, attempt_root, result, result_receipt, schemas) do
      {:ok, updated}
    end
  end

  @spec fetch_attempt(pos_integer()) :: {:ok, map()} | {:error, term()}
  def fetch_attempt(attempt_id) do
    case Repo.query!(
           """
           SELECT id, status, work_relative_path, branch, slot_index, base_best_revision,
                  base_sha, sampling_revision_id, guidance_revision_id,
                  current_iteration_round, candidate_sha, summary, outcome, failure_reason,
                  inserted_at, updated_at
           FROM attempts WHERE optimization_id = ? AND id = ?
           """,
           [@optimization_id, attempt_id]
         ).rows do
      [row] -> {:ok, attempt(row)}
      [] -> {:error, {:attempt_not_found, attempt_id}}
    end
  end

  defp finish_outcome(
         attempt,
         attempt_root,
         %{"outcome" => "ready_for_integration"} = result,
         result_receipt,
         schemas
       ) do
    with {:ok, context} <- sampling_context(attempt.sampling_revision_id),
         {:ok, verify, verify_receipt} <-
           validate_result_file(attempt_root, result, "verify", schemas.verify_result, :json),
         {:ok, benchmark, benchmark_receipt} <-
           validate_result_file(
             attempt_root,
             result,
             "benchmark",
             schemas.benchmark_record,
             :jsonl
           ),
         {:ok, patch_contents, patch_receipt} <- validate_patch_file(attempt_root, result),
         :ok <- validate_sample_verify(verify, context.cases),
         {:ok, statistics} <-
           Measurement.evaluate(benchmark, context.cases, context.metrics, context.measurement),
         :ok <- verify_candidate_git(attempt_root, attempt, result, patch_contents),
         {:ok, artifact_ids} <-
           register_artifacts(
             attempt,
             result_receipt,
             verify_receipt,
             benchmark_receipt,
             patch_receipt
           ),
         {:ok, updated} <- persist_ready(attempt, result, statistics, artifact_ids),
         :ok <- record_history(attempt_root, updated, result) do
      {:ok, updated}
    end
  end

  defp finish_outcome(
         attempt,
         attempt_root,
         %{"outcome" => "rejected"} = result,
         result_receipt,
         _schemas
       ) do
    with {:ok, result_artifact_id} <-
           ArtifactStore.register(
             "attempt",
             to_string(attempt.id),
             "iteration_result",
             result_receipt
           ),
         {:ok, updated} <- persist_rejected(attempt, result, result_artifact_id),
         :ok <- record_history(attempt_root, updated, result) do
      {:ok, updated}
    end
  end

  defp validate_identity(attempt, result) do
    details = result["details"]

    cond do
      result["role"] != "iteration" ->
        {:error, :iteration_role_mismatch}

      result["work_id"] != to_string(attempt.id) ->
        {:error, :iteration_work_mismatch}

      details["attempt_id"] != attempt.id ->
        {:error, :iteration_attempt_mismatch}

      details["iteration_round"] != attempt.current_iteration_round ->
        {:error, :iteration_round_mismatch}

      details["base_sha"] != attempt.base_sha ->
        {:error, :iteration_base_mismatch}

      details["sampling_revision"] != sampling_sequence(attempt.sampling_revision_id) ->
        {:error, :iteration_sampling_mismatch}

      true ->
        :ok
    end
  end

  defp sampling_context(sampling_revision_id) do
    case Repo.query!(
           """
           SELECT sr.baseline_revision_id, br.work_relative_path
           FROM sampling_revisions sr
           JOIN baseline_revisions br ON br.id = sr.baseline_revision_id
           WHERE sr.id = ?
           """,
           [sampling_revision_id]
         ).rows do
      [[baseline_revision_id, work_relative_path]] ->
        cases =
          Repo.query!(
            """
            SELECT bc.case_id, bc.name, bc.description, bc.inputs_json, bc.weight, bc.critical
            FROM sampling_revision_cases src
            JOIN benchmark_cases bc
              ON bc.baseline_revision_id = ? AND bc.case_id = src.case_id
            WHERE src.sampling_revision_id = ?
            ORDER BY bc.case_id
            """,
            [baseline_revision_id, sampling_revision_id]
          ).rows
          |> Enum.map(fn [id, name, description, inputs, weight, critical] ->
            %{
              "id" => id,
              "name" => name,
              "description" => description,
              "inputs" => Jason.decode!(inputs),
              "weight" => weight,
              "critical" => critical in [1, true]
            }
          end)

        metrics =
          Repo.query!(
            """
            SELECT metric_id, unit, direction, role, aggregation_json
            FROM metric_definitions WHERE baseline_revision_id = ? ORDER BY rowid
            """,
            [baseline_revision_id]
          ).rows
          |> Enum.map(fn [id, unit, direction, role, aggregation_json] ->
            %{"id" => id, "unit" => unit, "direction" => direction, "role" => role}
            |> Map.merge(Jason.decode!(aggregation_json))
          end)

        workspace = Persistence.current().workspace_canonical_path

        manifest =
          workspace
          |> Path.join(work_relative_path)
          |> Path.join("baseline-definition.json")
          |> File.read!()
          |> Jason.decode!()

        {:ok, %{cases: cases, metrics: metrics, measurement: manifest["measurement"]}}

      [] ->
        {:error, :sampling_context_missing}
    end
  end

  defp validate_result_file(attempt_root, result, key, schema_path, :json) do
    with {:ok, file} <- result_file(result, key),
         {:ok, value, receipt} <-
           FileContract.validate_json(attempt_root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp validate_result_file(attempt_root, result, key, schema_path, :jsonl) do
    with {:ok, file} <- result_file(result, key),
         {:ok, value, receipt} <-
           FileContract.validate_jsonl(attempt_root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp validate_patch_file(attempt_root, result) do
    with {:ok, file} <- result_file(result, "patch"),
         {:ok, contents, receipt} <- FileContract.read(attempt_root, file["path"]),
         :ok <- expected_digest(file, receipt) do
      {:ok, contents, receipt}
    end
  end

  defp result_file(result, key) do
    case get_in(result, ["files", key]) do
      %{"path" => path, "sha256" => sha256} = file
      when is_binary(path) and is_binary(sha256) ->
        {:ok, file}

      _other ->
        {:error, {:iteration_file_missing, key}}
    end
  end

  defp expected_digest(%{"sha256" => sha256}, %{sha256: sha256}), do: :ok

  defp expected_digest(file, receipt),
    do: {:error, {:file_digest_mismatch, file["path"], file["sha256"], receipt.sha256}}

  defp validate_sample_verify(verify, cases) do
    ids = Enum.map(cases, & &1["id"])
    actual_ids = Enum.map(verify["cases"], & &1["case_id"])

    passed? =
      verify["requested_case_ids"] == ids and actual_ids == ids and
        Enum.all?(verify["cases"], fn result ->
          result["target"]["passed"] == true and result["candidate"]["passed"] == true and
            result["comparison"]["passed"] == true
        end)

    if passed?, do: :ok, else: {:error, :iteration_verify_failed_or_incomplete}
  end

  defp verify_candidate_git(attempt_root, attempt, result, patch_contents) do
    repo = Path.join(attempt_root, "repo")
    candidate_sha = get_in(result, ["details", "candidate_sha"])

    with {:ok, head} <- Git.run(repo, ["rev-parse", "HEAD"]),
         :ok <- equal(String.trim(head), candidate_sha, :iteration_candidate_not_head),
         {:ok, _output} <-
           Git.run(repo, ["merge-base", "--is-ancestor", attempt.base_sha, candidate_sha]),
         :ok <- clean_worktree(repo),
         {:ok, changed} <-
           Git.run(repo, ["diff", "--name-only", "#{attempt.base_sha}..#{candidate_sha}"]),
         :ok <- protected_paths_unchanged(changed),
         {:ok, actual_patch} <-
           Git.run(repo, ["diff", "--binary", "#{attempt.base_sha}..#{candidate_sha}"]),
         :ok <-
           equal(
             String.trim(patch_contents),
             String.trim(actual_patch),
             :iteration_patch_mismatch
           ) do
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

  defp register_artifacts(attempt, result, verify, benchmark, patch) do
    owner_id = to_string(attempt.id)

    with {:ok, result_id} <-
           ArtifactStore.register("attempt", owner_id, "iteration_result", result),
         {:ok, verify_id} <-
           ArtifactStore.register("attempt", owner_id, "iteration_verify", verify),
         {:ok, benchmark_id} <-
           ArtifactStore.register("attempt", owner_id, "iteration_benchmark", benchmark),
         {:ok, patch_id} <- ArtifactStore.register("attempt", owner_id, "iteration_patch", patch) do
      {:ok, %{result: result_id, verify: verify_id, benchmark: benchmark_id, patch: patch_id}}
    end
  end

  defp persist_ready(attempt, result, statistics, artifact_ids) do
    now = now_us()
    details = result["details"]

    transaction =
      Repo.transaction(fn ->
        Repo.query!("DELETE FROM attempt_metrics WHERE attempt_id = ?", [attempt.id])

        Enum.each(statistics, fn statistic ->
          Repo.query!(
            """
            INSERT INTO attempt_metrics(
              attempt_id, case_id, metric_id, target_value, candidate_value,
              best_relative_improvement, noise_tolerance, valid_pair_count, source_artifact_id
            ) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?)
            """,
            [
              attempt.id,
              statistic.case_id,
              statistic.metric_id,
              statistic.target_value,
              statistic.development_value,
              statistic.noise_tolerance,
              statistic.valid_pair_count,
              artifact_ids.benchmark
            ]
          )
        end)

        Repo.query!(
          """
          UPDATE iteration_rounds
          SET result_artifact_id = ?, status = 'completed', completed_at = ?
          WHERE attempt_id = ? AND round = ? AND status = 'running'
          """,
          [artifact_ids.result, now, attempt.id, attempt.current_iteration_round]
        )

        Repo.query!(
          """
          UPDATE attempts
          SET status = 'ready_for_integration', candidate_sha = ?, summary = ?,
              outcome = 'ready_for_integration', failure_reason = NULL, updated_at = ?
          WHERE id = ? AND status IN ('iterating', 'refreshing_iteration')
          """,
          [details["candidate_sha"], result["summary"], now, attempt.id]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "iteration_ready",
          %{round: attempt.current_iteration_round},
          now
        )

        fetch_attempt!(attempt.id)
      end)

    finish(transaction, :iteration_ready_persist_failed)
  end

  defp persist_rejected(attempt, result, result_artifact_id) do
    now = now_us()
    details = result["details"]

    transaction =
      Repo.transaction(fn ->
        Repo.query!(
          """
          UPDATE iteration_rounds
          SET result_artifact_id = ?, status = 'rejected', completed_at = ?
          WHERE attempt_id = ? AND round = ? AND status = 'running'
          """,
          [result_artifact_id, now, attempt.id, attempt.current_iteration_round]
        )

        Repo.query!(
          """
          UPDATE attempts
          SET status = 'rejected', candidate_sha = ?, summary = ?, outcome = 'rejected',
              failure_reason = ?, updated_at = ?
          WHERE id = ? AND status IN ('iterating', 'refreshing_iteration')
          """,
          [
            details["candidate_sha"],
            result["summary"],
            details["failure_reason"],
            now,
            attempt.id
          ]
        )

        append_event(
          "attempt",
          to_string(attempt.id),
          "iteration_rejected",
          %{round: attempt.current_iteration_round},
          now
        )

        fetch_attempt!(attempt.id)
      end)

    finish(transaction, :iteration_rejection_persist_failed)
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

  defp require_open(%{status: status}) when status in @open_statuses, do: :ok
  defp require_open(attempt), do: {:error, {:attempt_not_iterating, attempt.status}}

  defp sampling_sequence(sampling_revision_id) do
    [[sequence]] =
      Repo.query!("SELECT sequence FROM sampling_revisions WHERE id = ?", [sampling_revision_id]).rows

    sequence
  end

  defp fetch_attempt!(attempt_id) do
    {:ok, attempt} = fetch_attempt(attempt_id)
    attempt
  end

  defp attempt([
         id,
         status,
         work_relative_path,
         branch,
         slot_index,
         base_best_revision,
         base_sha,
         sampling_revision_id,
         guidance_revision_id,
         current_iteration_round,
         candidate_sha,
         summary,
         outcome,
         failure_reason,
         inserted_at,
         updated_at
       ]) do
    %{
      id: id,
      status: status,
      work_relative_path: work_relative_path,
      branch: branch,
      slot_index: slot_index,
      base_best_revision: base_best_revision,
      base_sha: base_sha,
      sampling_revision_id: sampling_revision_id,
      guidance_revision_id: guidance_revision_id,
      current_iteration_round: current_iteration_round,
      candidate_sha: candidate_sha,
      summary: summary,
      outcome: outcome,
      failure_reason: failure_reason,
      inserted_at: inserted_at,
      updated_at: updated_at
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

  defp equal(value, value, _reason), do: :ok
  defp equal(_actual, _expected, reason), do: {:error, reason}

  defp clean_worktree(repo),
    do: if(Git.clean?(repo), do: :ok, else: {:error, :iteration_worktree_not_clean})

  defp finish({:ok, value}, _tag), do: {:ok, value}
  defp finish({:error, reason}, tag), do: {:error, {tag, reason}}
  defp now_us, do: System.system_time(:microsecond)
end
