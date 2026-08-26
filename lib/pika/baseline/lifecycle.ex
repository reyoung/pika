defmodule Pika.Baseline.Lifecycle do
  @moduledoc "The sole write interface for the v2 Baseline Definition and Review lifecycle."

  alias Pika.Baseline.{Definition, TargetSnapshot}
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Optimization.{FileContract, Measurement, Persistence}
  alias Pika.{Git, Repo}

  @optimization_id "optimization"

  @spec ensure_draft(Path.t()) :: {:ok, map()} | {:error, term()}
  def ensure_draft(work_root) when is_binary(work_root) do
    with optimization when is_map(optimization) <- Persistence.current(),
         {:ok, work_relative_path} <- work_relative_path(optimization, work_root) do
      case latest_revision() do
        nil ->
          insert_draft(0, work_relative_path)

        %{status: "superseded", revision: revision} ->
          insert_draft(revision + 1, work_relative_path)

        %{status: "drafting"} = revision ->
          {:ok, revision}

        revision ->
          {:error, {:baseline_revision_already_active, revision.revision, revision.status}}
      end
    else
      nil -> {:error, :optimization_not_initialized}
      {:error, _reason} = error -> error
    end
  end

  @spec submit_definition(non_neg_integer(), Path.t(), Path.t()) ::
          {:ok, map()} | {:error, term()}
  def submit_definition(revision, work_root, manifest_path)
      when is_integer(revision) and revision >= 0 and is_binary(work_root) and
             is_binary(manifest_path) do
    repo_root = repo_root(work_root)

    with {:ok, persisted} <- fetch_revision(revision),
         :ok <- require_status(persisted, "drafting"),
         {:ok, definition} <- Definition.validate(work_root, manifest_path, repo_root: repo_root),
         :ok <- verify_git(repo_root, definition),
         {:ok, artifact_ids} <- register_definition_artifacts(persisted, definition),
         {:ok, updated} <- persist_submission(persisted, definition, artifact_ids) do
      {:ok, updated}
    end
  end

  @spec review(
          non_neg_integer(),
          :approve | :request_changes,
          Path.t(),
          Path.t(),
          String.t() | nil
        ) ::
          {:ok, map()} | {:error, term()}
  def review(revision, decision, work_root, manifest_path, feedback \\ nil)
      when decision in [:approve, :request_changes] do
    repo_root = repo_root(work_root)

    with {:ok, persisted} <- fetch_revision(revision),
         :ok <- require_status(persisted, "awaiting_review"),
         {:ok, definition} <- Definition.validate(work_root, manifest_path, repo_root: repo_root),
         :ok <- verify_git(repo_root, definition),
         :ok <- same_review_identity(persisted, definition),
         {:ok, updated} <- persist_review(persisted, definition, decision, feedback) do
      {:ok, updated}
    end
  end

  @spec finish_verification(non_neg_integer(), Path.t(), Path.t()) ::
          {:ok, map()} | {:error, term()}
  def finish_verification(revision, work_root, result_path)
      when is_integer(revision) and revision >= 0 and is_binary(work_root) and
             is_binary(result_path) do
    repo_root = repo_root(work_root)

    with {:ok, persisted} <- fetch_revision(revision),
         :ok <- require_status(persisted, "verifying"),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, result, result_receipt} <-
           FileContract.validate_json(
             work_root,
             result_path,
             schemas.baseline_verification_result
           ),
         :ok <- verify_result_identity(persisted, result),
         {:ok, updated} <-
           finish_verification_outcome(
             persisted,
             work_root,
             repo_root,
             result,
             result_receipt,
             schemas
           ) do
      {:ok, updated}
    end
  end

  @doc """
  Reconsiders a superseded Baseline whose completed evidence was rejected only by the
  retired exact-Best-SHA identity gate.

  This is an operator recovery path. It validates the frozen Definition, the recorded
  rejection, the existing Full Verify and Full Benchmark files, and the Development
  ancestry before writing a new audit result and accepting the revision. It never runs
  the benchmark or verification commands.
  """
  @spec reconsider_verified_revision(non_neg_integer(), Path.t()) ::
          {:ok, map()} | {:error, term()}
  def reconsider_verified_revision(revision, work_root)
      when is_integer(revision) and revision >= 0 and is_binary(work_root) do
    repo_root = repo_root(work_root)

    with {:ok, persisted} <- fetch_revision(revision) do
      case persisted.status do
        "accepted" ->
          {:ok, persisted}

        "superseded" ->
          with :ok <- require_revision_work_root(persisted, work_root),
               :ok <- require_only_draft_successors(persisted),
               {:ok, schemas} <- RolePromptRegistry.schemas(),
               {:ok, rejection, rejection_receipt} <-
                 rejected_verification_result(persisted, schemas),
               :ok <- verify_result_identity(persisted, rejection),
               :ok <- require_legacy_best_identity_rejection(persisted, rejection),
               {:ok, definition} <-
                 Definition.validate(work_root, "baseline-definition.json", repo_root: repo_root),
               :ok <- verify_git(repo_root, definition),
               :ok <- same_review_identity(persisted, definition),
               {:ok, verify, verify_receipt} <-
                 FileContract.validate_json(work_root, "full-verify.json", schemas.verify_result),
               {:ok, benchmark, benchmark_receipt} <-
                 FileContract.validate_jsonl(
                   work_root,
                   "full-benchmark.jsonl",
                   schemas.benchmark_record
                 ),
               :ok <- validate_full_verify(verify, definition.cases),
               {:ok, statistics} <-
                 Measurement.evaluate(
                   benchmark,
                   definition.cases,
                   definition.metrics,
                   definition.manifest["measurement"]
                 ),
               {:ok, sampling} <- reconsideration_sampling(persisted, definition.cases),
               result <-
                 reconsideration_result(
                   persisted,
                   statistics,
                   sampling,
                   verify_receipt,
                   benchmark_receipt
                 ),
               {:ok, result_receipt} <-
                 write_reconsideration_result(work_root, result, schemas),
               {:ok, artifact_ids} <-
                 register_verification_artifacts(
                   persisted,
                   result_receipt,
                   verify_receipt,
                   benchmark_receipt
                 ),
               {:ok, updated} <-
                 persist_verification_accept(
                   persisted,
                   result,
                   statistics,
                   sampling,
                   artifact_ids,
                   from_status: "superseded",
                   reconsideration: %{
                     rejection_artifact_sha256: rejection_receipt.sha256
                   }
                 ) do
            {:ok, updated}
          end

        status ->
          {:error, {:invalid_baseline_status, status, "superseded"}}
      end
    end
  end

  @spec latest_revision() :: map() | nil
  def latest_revision do
    revisions() |> List.last()
  end

  @spec revisions() :: [map()]
  def revisions do
    Repo.query!(
      """
      SELECT id, revision, status, work_relative_path, definition_artifact_id,
             definition_sha256, dependencies_sha256, development_sha,
             target_snapshot_id, review_feedback, terminal_reason,
             inserted_at, updated_at
      FROM baseline_revisions
      WHERE optimization_id = ?
      ORDER BY revision
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(&baseline_revision/1)
  end

  @spec project_work() :: [map()]
  def project_work do
    case latest_revision() do
      %{status: "drafting"} = revision ->
        [
          %{
            role_id: "baseline_alignment",
            work_kind: :baseline_revision,
            work_id: to_string(revision.id)
          }
        ]

      %{status: "verifying"} = revision ->
        [
          %{
            role_id: "baseline_verify",
            work_kind: :baseline_revision,
            work_id: to_string(revision.id)
          }
        ]

      _other ->
        []
    end
  end

  defp insert_draft(revision, work_relative_path) do
    now = now_us()

    result =
      Repo.transaction(fn ->
        Repo.query!(
          """
          INSERT INTO baseline_revisions(
            optimization_id, revision, status, work_relative_path, inserted_at, updated_at
          ) VALUES (?, ?, 'drafting', ?, ?, ?)
          """,
          [@optimization_id, revision, work_relative_path, now, now]
        )

        Repo.query!(
          "UPDATE optimizations SET status = 'aligning_baseline', updated_at = ? WHERE id = ?",
          [now, @optimization_id]
        )

        append_event(
          "baseline_revision",
          to_string(revision),
          "baseline_revision_drafting",
          %{},
          now
        )

        fetch_revision!(revision)
      end)

    finish(result, :baseline_draft_creation_failed)
  end

  defp persist_submission(persisted, definition, artifact_ids) do
    now = now_us()
    manifest_artifact_id = Map.fetch!(artifact_ids, definition.manifest_receipt.relative_path)

    result =
      Repo.transaction(fn ->
        Repo.query!(
          """
          UPDATE baseline_revisions
          SET status = 'awaiting_review', definition_artifact_id = ?, definition_sha256 = ?,
              dependencies_sha256 = ?, development_sha = ?, updated_at = ?
          WHERE id = ? AND status = 'drafting'
          """,
          [
            manifest_artifact_id,
            definition.manifest_receipt.sha256,
            definition.dependencies_sha256,
            get_in(definition.manifest, ["development_baseline", "commit_sha"]),
            now,
            persisted.id
          ]
        )

        replace_cases_and_metrics(persisted.id, definition)

        Repo.query!(
          "UPDATE optimizations SET status = 'awaiting_baseline_review', updated_at = ? WHERE id = ?",
          [now, @optimization_id]
        )

        append_event(
          "baseline_revision",
          to_string(persisted.revision),
          "baseline_definition_submitted",
          %{definition_sha256: definition.manifest_receipt.sha256},
          now
        )

        fetch_revision!(persisted.revision)
      end)

    finish(result, :baseline_definition_submission_failed)
  end

  defp persist_review(persisted, definition, :request_changes, feedback) do
    feedback = if is_binary(feedback), do: String.trim(feedback), else: ""

    if feedback == "" do
      {:error, :review_feedback_required}
    else
      now = now_us()

      result =
        Repo.transaction(fn ->
          insert_review(persisted, definition, "changes_requested", feedback, now)

          Repo.query!(
            """
            UPDATE baseline_revisions
            SET status = 'superseded', review_feedback = ?, terminal_reason = 'changes_requested', updated_at = ?
            WHERE id = ? AND status = 'awaiting_review'
            """,
            [feedback, now, persisted.id]
          )

          Repo.query!(
            "UPDATE optimizations SET status = 'aligning_baseline', updated_at = ? WHERE id = ?",
            [now, @optimization_id]
          )

          append_event(
            "baseline_revision",
            to_string(persisted.revision),
            "baseline_review_changes_requested",
            %{feedback: feedback},
            now
          )

          fetch_revision!(persisted.revision)
        end)

      finish(result, :baseline_review_failed)
    end
  end

  defp persist_review(persisted, definition, :approve, _feedback) do
    now = now_us()
    target_manifest_path = get_in(definition.manifest, ["optimization_target", "manifest_path"])
    target_snapshot_id = Ecto.UUID.generate()

    with {:ok, target_artifact_id} <- artifact_id_for_kind(persisted, "target_manifest"),
         {:ok, target_relative_path} <-
           TargetSnapshot.create(
             Path.join(
               Persistence.current().workspace_canonical_path,
               persisted.work_relative_path
             ),
             target_snapshot_id,
             definition.target_manifest,
             definition.dependency_receipts[target_manifest_path].sha256
           ) do
      result =
        Repo.transaction(fn ->
          insert_review(persisted, definition, "approved", nil, now)

          Repo.query!(
            """
            INSERT INTO target_snapshots(
              id, optimization_id, baseline_revision_id, provenance_json, relative_path, root_artifact_id,
              digest, entrypoint, created_at
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            [
              target_snapshot_id,
              @optimization_id,
              persisted.id,
              Jason.encode!(definition.target_manifest),
              target_relative_path,
              target_artifact_id,
              definition.dependency_receipts[target_manifest_path].sha256,
              get_in(definition.manifest, ["optimization_target", "entrypoint"]),
              now
            ]
          )

          Repo.query!(
            """
            UPDATE baseline_revisions
            SET status = 'verifying', target_snapshot_id = ?, updated_at = ?
            WHERE id = ? AND status = 'awaiting_review'
            """,
            [target_snapshot_id, now, persisted.id]
          )

          Repo.query!(
            "UPDATE optimizations SET status = 'verifying_baseline', updated_at = ? WHERE id = ?",
            [now, @optimization_id]
          )

          append_event(
            "baseline_revision",
            to_string(persisted.revision),
            "baseline_review_approved",
            %{target_snapshot_id: target_snapshot_id},
            now
          )

          fetch_revision!(persisted.revision)
        end)

      finish(result, :baseline_review_failed)
    end
  end

  defp finish_verification_outcome(
         persisted,
         work_root,
         repo_root,
         %{"outcome" => "accepted"} = result,
         result_receipt,
         schemas
       ) do
    with {:ok, definition} <-
           Definition.validate(work_root, "baseline-definition.json", repo_root: repo_root),
         :ok <- same_review_identity(persisted, definition),
         {:ok, verify, verify_receipt} <-
           validate_result_file(work_root, result, "verify", schemas.verify_result, :json),
         {:ok, benchmark, benchmark_receipt} <-
           validate_result_file(
             work_root,
             result,
             "benchmark",
             schemas.benchmark_record,
             :jsonl
           ),
         :ok <- validate_full_verify(verify, definition.cases),
         {:ok, statistics} <-
           Measurement.evaluate(
             benchmark,
             definition.cases,
             definition.metrics,
             definition.manifest["measurement"]
           ),
         :ok <- validate_reported_metrics(result, statistics),
         {:ok, sampling} <- validate_initial_sampling(result, definition.cases),
         :ok <- advance_initial_best(repo_root, persisted.development_sha),
         {:ok, artifact_ids} <-
           register_verification_artifacts(
             persisted,
             result_receipt,
             verify_receipt,
             benchmark_receipt
           ),
         {:ok, updated} <-
           persist_verification_accept(
             persisted,
             result,
             statistics,
             sampling,
             artifact_ids
           ) do
      {:ok, updated}
    end
  end

  defp finish_verification_outcome(
         persisted,
         _work_root,
         _repo_root,
         %{"outcome" => "definition_rejected"} = result,
         result_receipt,
         _schemas
       ) do
    with {:ok, result_artifact_id} <-
           register_verification_receipt(
             persisted,
             result_receipt,
             "baseline_verification_result"
           ),
         {:ok, updated} <-
           persist_verification_rejection(persisted, result, result_artifact_id) do
      {:ok, updated}
    end
  end

  defp require_revision_work_root(persisted, work_root) do
    optimization = Persistence.current()

    with {:ok, relative} <-
           workspace_relative(optimization.workspace_canonical_path, work_root) do
      if relative == persisted.work_relative_path,
        do: :ok,
        else:
          {:error,
           {:baseline_revision_work_root_mismatch,
            %{expected: persisted.work_relative_path, actual: relative}}}
    end
  end

  defp require_only_draft_successors(persisted) do
    successors =
      Repo.query!(
        "SELECT revision, status FROM baseline_revisions WHERE optimization_id = ? AND revision > ? ORDER BY revision",
        [@optimization_id, persisted.revision]
      ).rows

    case Enum.reject(successors, fn [_revision, status] -> status == "drafting" end) do
      [] -> :ok
      blocked -> {:error, {:baseline_reconsideration_has_nondraft_successors, blocked}}
    end
  end

  defp rejected_verification_result(persisted, schemas) do
    case Repo.query!(
           """
           SELECT a.relative_path
           FROM baseline_verifications bv
           JOIN artifacts a ON a.id = bv.result_artifact_id
           WHERE bv.optimization_id = ? AND bv.baseline_revision_id = ?
             AND bv.outcome = 'definition_rejected'
           ORDER BY bv.created_at DESC, bv.id DESC
           LIMIT 1
           """,
           [@optimization_id, persisted.id]
         ).rows do
      [[relative_path]] ->
        workspace = Persistence.current().workspace_canonical_path
        FileContract.validate_json(workspace, relative_path, schemas.baseline_verification_result)

      [] ->
        {:error, :baseline_reconsideration_rejection_missing}
    end
  end

  defp require_legacy_best_identity_rejection(persisted, rejection) do
    best_sha = Persistence.current().best_sha
    details = rejection["details"]
    reason = details["reason"] || ""
    requested_changes = details["requested_changes"] || []
    identity_text = Enum.join([reason | requested_changes], "\n")

    valid? =
      rejection["outcome"] == "definition_rejected" and
        details["failure_kind"] == "definition" and
        is_binary(best_sha) and
        String.contains?(identity_text, best_sha) and
        String.contains?(identity_text, persisted.development_sha) and
        (String.contains?(identity_text, "Best") or String.contains?(identity_text, "best")) and
        (String.contains?(identity_text, "Development") or
           String.contains?(identity_text, "development"))

    if valid?, do: :ok, else: {:error, :baseline_reconsideration_not_identity_gate_rejection}
  end

  defp reconsideration_sampling(persisted, cases) do
    known = MapSet.new(Enum.map(cases, & &1["id"]))

    previous =
      case Repo.query!(
             """
             SELECT sr.id
             FROM sampling_revisions sr
             JOIN baseline_revisions br ON br.id = sr.baseline_revision_id
             WHERE sr.optimization_id = ? AND br.revision < ?
             ORDER BY sr.created_at DESC, sr.id DESC
             LIMIT 1
             """,
             [@optimization_id, persisted.revision]
           ).rows do
        [[sampling_revision_id]] ->
          Repo.query!(
            "SELECT case_id, reason FROM sampling_revision_cases WHERE sampling_revision_id = ? ORDER BY case_id",
            [sampling_revision_id]
          ).rows
          |> Enum.flat_map(fn [case_id, reason] ->
            if MapSet.member?(known, case_id), do: [%{case_id: case_id, reason: reason}], else: []
          end)

        [] ->
          []
      end

    sampling = if previous == [], do: fallback_sampling(cases), else: Enum.take(previous, 10)

    if sampling == [],
      do: {:error, :baseline_reconsideration_sampling_unavailable},
      else: {:ok, sampling}
  end

  defp fallback_sampling(cases) do
    cases
    |> Enum.sort_by(fn case_ ->
      {not case_["critical"], -(case_["weight"] || 0), case_["id"]}
    end)
    |> Enum.take(10)
    |> Enum.map(fn case_ ->
      %{
        case_id: case_["id"],
        reason:
          if(case_["critical"],
            do: "critical case retained by system reconsideration",
            else: "highest-weight representative retained by system reconsideration"
          )
      }
    end)
  end

  defp reconsideration_result(
         persisted,
         statistics,
         sampling,
         verify_receipt,
         benchmark_receipt
       ) do
    %{
      "schema_version" => 1,
      "role" => "baseline_verify",
      "work_id" => to_string(persisted.id),
      "outcome" => "accepted",
      "summary" =>
        "Accepted from existing complete evidence after removing the obsolete exact-Best-SHA gate; no commands were rerun.",
      "files" => %{
        "verify" => receipt_identity(verify_receipt),
        "benchmark" => receipt_identity(benchmark_receipt)
      },
      "details" => %{
        "baseline_revision" => persisted.revision,
        "definition_sha256" => persisted.definition_sha256,
        "development_sha" => persisted.development_sha,
        "initial_iteration_case_ids" => Enum.map(sampling, & &1.case_id),
        "case_selection_reasons" =>
          Enum.map(sampling, &%{"case_id" => &1.case_id, "reason" => &1.reason}),
        "case_metrics" => Enum.map(statistics, &reported_metric/1),
        "judgement" => %{
          "reasonable" => true,
          "reason" =>
            "Frozen Definition, complete raw evidence, calculated metrics, and Development ancestry were revalidated by Pika."
        }
      }
    }
  end

  defp receipt_identity(receipt),
    do: %{"path" => receipt.relative_path, "sha256" => receipt.sha256}

  defp reported_metric(statistic) do
    %{
      "case_id" => statistic.case_id,
      "metric_id" => statistic.metric_id,
      "unit" => statistic.unit,
      "target_value" => statistic.target_value,
      "development_value" => statistic.development_value,
      "relative_difference" => statistic.relative_difference,
      "noise_tolerance" => statistic.noise_tolerance,
      "valid_pair_count" => statistic.valid_pair_count
    }
  end

  defp write_reconsideration_result(work_root, result, schemas) do
    relative_path = "baseline-verification-reconsideration.json"
    absolute_path = Path.join(Pika.Paths.canonical!(work_root), relative_path)
    contents = Jason.encode!(result)

    with :ok <- write_immutable_file(absolute_path, contents),
         {:ok, validated, receipt} <-
           FileContract.validate_json(
             work_root,
             relative_path,
             schemas.baseline_verification_result
           ),
         true <- validated == result || {:error, :baseline_reconsideration_result_changed} do
      {:ok, receipt}
    end
  end

  defp write_immutable_file(path, contents) do
    case File.write(path, contents, [:exclusive]) do
      :ok ->
        :ok

      {:error, :eexist} ->
        case File.read(path) do
          {:ok, ^contents} -> :ok
          {:ok, _other} -> {:error, {:baseline_reconsideration_result_conflict, path}}
          {:error, reason} -> {:error, {:baseline_reconsideration_result_read_failed, reason}}
        end

      {:error, reason} ->
        {:error, {:baseline_reconsideration_result_write_failed, reason}}
    end
  end

  defp persist_verification_accept(
         persisted,
         result,
         statistics,
         sampling,
         artifact_ids,
         opts \\ []
       ) do
    now = now_us()
    from_status = Keyword.get(opts, :from_status, "verifying")
    reconsideration = Keyword.get(opts, :reconsideration)

    with {:ok, best_transition} <- prepare_baseline_best_transition(persisted) do
      transaction =
        Repo.transaction(fn ->
          if reconsideration do
            supersede_reconsideration_successors(persisted, now)
          end

          Repo.query!(
            """
            INSERT INTO baseline_verifications(
              optimization_id, baseline_revision_id, result_artifact_id, outcome,
              verify_artifact_id, benchmark_artifact_id, development_sha,
              statistics_json, requested_changes_json, created_at
            ) VALUES (?, ?, ?, 'accepted', ?, ?, ?, ?, '[]', ?)
            """,
            [
              @optimization_id,
              persisted.id,
              artifact_ids.result,
              artifact_ids.verify,
              artifact_ids.benchmark,
              persisted.development_sha,
              Jason.encode!(statistics),
              now
            ]
          )

          best_revision_id =
            ensure_current_best_revision(
              persisted,
              result["summary"],
              best_transition,
              now
            )

          Repo.query!("DELETE FROM best_metrics WHERE best_revision_id = ?", [best_revision_id])

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
                statistic.case_id,
                statistic.metric_id,
                statistic.target_value,
                statistic.development_value,
                statistic.normalized_ratio,
                statistic.mad,
                statistic.noise_tolerance,
                statistic.valid_pair_count,
                artifact_ids.benchmark
              ]
            )
          end)

          Repo.query!(
            """
            INSERT INTO sampling_revisions(
              optimization_id, baseline_revision_id, sequence, cause, created_at
            ) VALUES (?, ?, 0, 'baseline_verification', ?)
            """,
            [@optimization_id, persisted.id, now]
          )

          [[sampling_revision_id]] =
            Repo.query!(
              "SELECT id FROM sampling_revisions WHERE baseline_revision_id = ? AND sequence = 0",
              [persisted.id]
            ).rows

          Enum.each(sampling, fn %{case_id: case_id, reason: reason} ->
            Repo.query!(
              """
              INSERT INTO sampling_revision_cases(sampling_revision_id, case_id, reason)
              VALUES (?, ?, ?)
              """,
              [sampling_revision_id, case_id, reason]
            )
          end)

          Repo.query!(
            """
            UPDATE attempts
            SET sampling_revision_id = ?,
                status = CASE
                  WHEN status = 'verification_retry_pending' THEN 'ready_for_integration'
                  ELSE status
                END,
                updated_at = ?
            WHERE optimization_id = ?
              AND status IN ('ready_for_integration', 'verification_retry_pending')
            """,
            [sampling_revision_id, now, @optimization_id]
          )

          case Repo.query!(
                 "UPDATE baseline_revisions SET status = 'accepted', terminal_reason = NULL, updated_at = ? WHERE id = ? AND status = ?",
                 [now, persisted.id, from_status]
               ).num_rows do
            1 -> :ok
            0 -> Repo.rollback({:baseline_status_changed, persisted.revision, from_status})
          end

          Repo.query!(
            """
            UPDATE optimizations
            SET status = 'optimizing', best_sha = ?, resume_status = NULL, stop_reason = NULL,
                updated_at = ?
            WHERE id = ?
            """,
            [persisted.development_sha, now, @optimization_id]
          )

          append_event(
            "baseline_revision",
            to_string(persisted.revision),
            if(reconsideration,
              do: "baseline_verification_reconsidered",
              else: "baseline_verification_accepted"
            ),
            %{
              best_sha: persisted.development_sha,
              sampling_revision_id: sampling_revision_id,
              reconsideration: reconsideration
            },
            now
          )

          fetch_revision!(persisted.revision)
        end)

      finish(transaction, :baseline_verification_accept_failed)
    end
  end

  defp supersede_reconsideration_successors(persisted, now) do
    successors =
      Repo.query!(
        "SELECT id, revision FROM baseline_revisions WHERE optimization_id = ? AND revision > ? AND status = 'drafting'",
        [@optimization_id, persisted.revision]
      ).rows

    Enum.each(successors, fn [successor_id, successor_revision] ->
      Repo.query!(
        "UPDATE baseline_question_batches SET status = 'cancelled' WHERE baseline_revision_id = ? AND status = 'pending'",
        [successor_id]
      )

      Repo.query!(
        """
        UPDATE conversation_turns
        SET ended_reason = 'interrupted', ended_at = ?
        WHERE optimization_id = ? AND role = 'baseline_alignment'
          AND work_kind = 'baseline_revision' AND work_id = ? AND partial = 1
        """,
        [now, @optimization_id, to_string(successor_id)]
      )

      Repo.query!(
        """
        UPDATE agent_sessions
        SET status = 'interrupted', ended_reason = 'baseline_revision_reconsidered', ended_at = ?
        WHERE optimization_id = ? AND role = 'baseline_alignment'
          AND work_kind = 'baseline_revision' AND work_id = ?
          AND status IN ('running', 'awaiting_report', 'awaiting_followup')
        """,
        [now, @optimization_id, to_string(successor_id)]
      )

      Repo.query!(
        """
        UPDATE baseline_revisions
        SET status = 'superseded', terminal_reason = ?, updated_at = ?
        WHERE id = ? AND status = 'drafting'
        """,
        ["replaced_by_reconsidered_baseline_v#{persisted.revision}", now, successor_id]
      )

      append_event(
        "baseline_revision",
        to_string(successor_revision),
        "baseline_revision_superseded_by_reconsideration",
        %{accepted_revision: persisted.revision},
        now
      )
    end)
  end

  defp prepare_baseline_best_transition(persisted) do
    optimization = Persistence.current()
    repo = optimization.repo_canonical_path
    expected = optimization.best_sha
    candidate = persisted.development_sha
    best_ref = "refs/heads/#{optimization.best_branch}"

    with {:ok, actual} <- Git.run(repo, ["rev-parse", best_ref]),
         :ok <- prepare_baseline_best_ref(repo, best_ref, actual, expected, candidate) do
      {:ok, %{previous_sha: expected, candidate_sha: candidate}}
    end
  end

  defp prepare_baseline_best_ref(_repo, _ref, candidate, nil, candidate), do: :ok

  defp prepare_baseline_best_ref(_repo, ref, actual, nil, candidate) do
    {:error,
     {:initial_baseline_best_ref_mismatch, %{ref: ref, actual: actual, expected: candidate}}}
  end

  defp prepare_baseline_best_ref(repo, ref, actual, expected, candidate) do
    with :ok <- require_descendant(repo, expected, candidate),
         do: advance_baseline_best_ref(repo, ref, actual, expected, candidate)
  end

  defp require_descendant(_repo, sha, sha), do: :ok

  defp require_descendant(repo, expected, candidate) do
    case Git.run(repo, ["merge-base", "--is-ancestor", expected, candidate]) do
      {:ok, _output} ->
        :ok

      {:error, reason} ->
        {:error,
         {:baseline_revision_development_not_descendant,
          %{expected: expected, actual: candidate, git: reason}}}
    end
  end

  defp advance_baseline_best_ref(_repo, _ref, candidate, _expected, candidate), do: :ok

  defp advance_baseline_best_ref(repo, ref, expected, expected, candidate) do
    case Git.run(repo, ["update-ref", ref, candidate, expected]) do
      {:ok, _output} -> :ok
      {:error, reason} -> {:error, {:baseline_best_ref_update_failed, reason}}
    end
  end

  defp advance_baseline_best_ref(repo, ref, actual, expected, candidate) do
    with :ok <- require_descendant(repo, expected, actual),
         :ok <- require_descendant(repo, actual, candidate),
         {:ok, _output} <- Git.run(repo, ["update-ref", ref, candidate, actual]) do
      :ok
    else
      {:error, reason} ->
        {:error,
         {:baseline_best_ref_mismatch,
          %{ref: ref, actual: actual, expected: expected, candidate: candidate, reason: reason}}}
    end
  end

  defp ensure_current_best_revision(persisted, summary, best_transition, now) do
    case Repo.query!(
           "SELECT id, sequence, sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1",
           [@optimization_id]
         ).rows do
      [] ->
        Repo.query!(
          """
          INSERT INTO best_revisions(
            optimization_id, sequence, sha, source_kind, baseline_revision_id, summary, created_at
          ) VALUES (?, 0, ?, 'baseline', ?, ?, ?)
          """,
          [@optimization_id, persisted.development_sha, persisted.id, summary, now]
        )

        [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
        id

      [[id, _sequence, sha]] when sha == persisted.development_sha ->
        Repo.query!(
          "UPDATE best_revisions SET baseline_revision_id = ? WHERE id = ?",
          [persisted.id, id]
        )

        id

      [[_id, sequence, sha]] when sha == best_transition.previous_sha ->
        Repo.query!(
          """
          INSERT INTO best_revisions(
            optimization_id, sequence, sha, source_kind, baseline_revision_id, summary, created_at
          ) VALUES (?, ?, ?, 'baseline', ?, ?, ?)
          """,
          [
            @optimization_id,
            sequence + 1,
            persisted.development_sha,
            persisted.id,
            summary,
            now
          ]
        )

        [[id]] = Repo.query!("SELECT last_insert_rowid()").rows
        id

      [[_id, _sequence, sha]] ->
        Repo.rollback(
          {:baseline_best_database_mismatch,
           %{actual: sha, expected: best_transition.previous_sha}}
        )
    end
  end

  defp persist_verification_rejection(persisted, result, result_artifact_id) do
    now = now_us()
    details = result["details"]

    transaction =
      Repo.transaction(fn ->
        Repo.query!(
          """
          INSERT INTO baseline_verifications(
            optimization_id, baseline_revision_id, result_artifact_id, outcome,
            development_sha, statistics_json, requested_changes_json, created_at
          ) VALUES (?, ?, ?, 'definition_rejected', ?, '[]', ?, ?)
          """,
          [
            @optimization_id,
            persisted.id,
            result_artifact_id,
            persisted.development_sha,
            Jason.encode!(details["requested_changes"]),
            now
          ]
        )

        Repo.query!(
          """
          UPDATE baseline_revisions
          SET status = 'superseded', terminal_reason = ?, updated_at = ?
          WHERE id = ? AND status = 'verifying'
          """,
          [details["reason"], now, persisted.id]
        )

        Repo.query!(
          "UPDATE optimizations SET status = 'aligning_baseline', updated_at = ? WHERE id = ?",
          [now, @optimization_id]
        )

        append_event(
          "baseline_revision",
          to_string(persisted.revision),
          "baseline_verification_rejected",
          %{failure_kind: details["failure_kind"], result_artifact_id: result_artifact_id},
          now
        )

        fetch_revision!(persisted.revision)
      end)

    finish(transaction, :baseline_verification_rejection_failed)
  end

  defp register_definition_artifacts(persisted, definition) do
    Enum.reduce_while(definition.dependency_receipts, {:ok, %{}}, fn {_path, receipt},
                                                                     {:ok, ids} ->
      case register_artifact(persisted, receipt) do
        {:ok, artifact_id} -> {:cont, {:ok, Map.put(ids, receipt.relative_path, artifact_id)}}
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
  end

  defp verify_result_identity(persisted, result) do
    details = result["details"]

    cond do
      result["role"] != "baseline_verify" ->
        {:error, :baseline_verification_role_mismatch}

      result["work_id"] != to_string(persisted.id) ->
        {:error, :baseline_verification_work_mismatch}

      details["baseline_revision"] != persisted.revision ->
        {:error, :baseline_verification_revision_mismatch}

      details["definition_sha256"] != persisted.definition_sha256 ->
        {:error, :baseline_verification_definition_mismatch}

      details["development_sha"] != persisted.development_sha ->
        {:error, :baseline_verification_development_mismatch}

      true ->
        :ok
    end
  end

  defp validate_result_file(work_root, result, key, schema_path, :json) do
    with {:ok, file} <- result_file(result, key),
         {:ok, value, receipt} <-
           FileContract.validate_json(work_root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp validate_result_file(work_root, result, key, schema_path, :jsonl) do
    with {:ok, file} <- result_file(result, key),
         {:ok, value, receipt} <-
           FileContract.validate_jsonl(work_root, file["path"], schema_path),
         :ok <- expected_digest(file, receipt) do
      {:ok, value, receipt}
    end
  end

  defp result_file(result, key) do
    case get_in(result, ["files", key]) do
      %{"path" => path, "sha256" => sha256} = file
      when is_binary(path) and is_binary(sha256) ->
        {:ok, file}

      _other ->
        {:error, {:baseline_verification_file_missing, key}}
    end
  end

  defp expected_digest(%{"sha256" => sha256}, %{sha256: sha256}), do: :ok

  defp expected_digest(file, receipt),
    do: {:error, {:file_digest_mismatch, file["path"], file["sha256"], receipt.sha256}}

  defp validate_full_verify(verify, cases) do
    expected_ids = Enum.map(cases, & &1["id"])
    actual_ids = Enum.map(verify["cases"], & &1["case_id"])

    passed? =
      verify["requested_case_ids"] == expected_ids and actual_ids == expected_ids and
        Enum.all?(verify["cases"], fn result ->
          result["target"]["passed"] == true and
            result["candidate"]["passed"] == true and
            result["comparison"]["passed"] == true
        end)

    if passed?, do: :ok, else: {:error, :full_verify_failed_or_incomplete}
  end

  defp validate_reported_metrics(result, statistics) do
    reported = get_in(result, ["details", "case_metrics"])

    reported_by_key =
      Map.new(reported, fn metric -> {{metric["case_id"], metric["metric_id"]}, metric} end)

    expected_keys = MapSet.new(Enum.map(statistics, &{&1.case_id, &1.metric_id}))
    reported_keys = MapSet.new(Map.keys(reported_by_key))

    cond do
      length(reported) != map_size(reported_by_key) ->
        {:error, :duplicate_reported_case_metrics}

      expected_keys != reported_keys ->
        {:error, :reported_case_metric_coverage_mismatch}

      true ->
        Enum.reduce_while(statistics, :ok, fn statistic, :ok ->
          reported = Map.fetch!(reported_by_key, {statistic.case_id, statistic.metric_id})

          if reported_metric_matches?(reported, statistic) do
            {:cont, :ok}
          else
            {:halt,
             {:error, {:reported_case_metric_mismatch, statistic.case_id, statistic.metric_id}}}
          end
        end)
    end
  end

  defp reported_metric_matches?(reported, statistic) do
    reported["unit"] == statistic.unit and
      reported["valid_pair_count"] == statistic.valid_pair_count and
      close?(reported["target_value"], statistic.target_value) and
      close?(reported["development_value"], statistic.development_value) and
      close?(reported["relative_difference"], statistic.relative_difference) and
      close?(reported["noise_tolerance"], statistic.noise_tolerance)
  end

  defp close?(reported, expected) when is_number(reported) and is_number(expected) do
    abs(reported - expected) <= max(1.0e-9, abs(expected) * 1.0e-9)
  end

  defp close?(_reported, _expected), do: false

  defp validate_initial_sampling(result, cases) do
    ids = get_in(result, ["details", "initial_iteration_case_ids"])
    reasons = get_in(result, ["details", "case_selection_reasons"])
    reason_by_id = Map.new(reasons, &{&1["case_id"], &1["reason"]})
    known = MapSet.new(Enum.map(cases, & &1["id"]))

    cond do
      ids == [] or length(ids) > 10 ->
        {:error, :invalid_initial_sampling_size}

      Enum.uniq(ids) != ids or not MapSet.subset?(MapSet.new(ids), known) ->
        {:error, :invalid_initial_sampling_cases}

      MapSet.new(ids) != MapSet.new(Map.keys(reason_by_id)) or
          length(reasons) != map_size(reason_by_id) ->
        {:error, :initial_sampling_reasons_mismatch}

      true ->
        {:ok, Enum.map(ids, &%{case_id: &1, reason: Map.fetch!(reason_by_id, &1)})}
    end
  end

  defp advance_initial_best(repo_root, development_sha) do
    case Git.run(repo_root, ["branch", "-f", "pika/best", development_sha]) do
      {:ok, _output} -> :ok
      {:error, reason} -> {:error, {:initial_best_update_failed, reason}}
    end
  end

  defp register_verification_artifacts(persisted, result, verify, benchmark) do
    with {:ok, result_id} <-
           register_verification_receipt(persisted, result, "baseline_verification_result"),
         {:ok, verify_id} <- register_verification_receipt(persisted, verify, "full_verify"),
         {:ok, benchmark_id} <-
           register_verification_receipt(persisted, benchmark, "full_benchmark") do
      {:ok, %{result: result_id, verify: verify_id, benchmark: benchmark_id}}
    end
  end

  defp register_verification_receipt(persisted, receipt, kind) do
    optimization = Persistence.current()

    with {:ok, workspace_relative_path} <-
           workspace_relative(optimization.workspace_canonical_path, receipt.absolute_path) do
      upsert_artifact(
        workspace_relative_path,
        "baseline_verification",
        to_string(persisted.id),
        kind,
        receipt
      )
    end
  end

  defp register_artifact(persisted, receipt) do
    optimization = Persistence.current()

    with {:ok, workspace_relative_path} <-
           workspace_relative(optimization.workspace_canonical_path, receipt.absolute_path),
         {:ok, artifact_id} <-
           upsert_artifact(
             workspace_relative_path,
             "baseline_revision",
             to_string(persisted.id),
             artifact_kind(receipt.relative_path),
             receipt
           ) do
      {:ok, artifact_id}
    end
  end

  defp upsert_artifact(relative_path, owner_type, owner_id, kind, receipt) do
    now = now_us()

    case Repo.query!(
           """
           SELECT id, owner_type, owner_id, kind, sha256, byte_size FROM artifacts
           WHERE optimization_id = ? AND relative_path = ?
           """,
           [@optimization_id, relative_path]
         ).rows do
      [] ->
        id = Ecto.UUID.generate()

        Repo.query!(
          """
          INSERT INTO artifacts(
            id, optimization_id, owner_type, owner_id, kind, relative_path,
            sha256, byte_size, mime_type, metadata_json, created_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?)
          """,
          [
            id,
            @optimization_id,
            owner_type,
            owner_id,
            kind,
            relative_path,
            receipt.sha256,
            receipt.byte_size,
            MIME.from_path(receipt.absolute_path),
            now
          ]
        )

        {:ok, id}

      [[id, ^owner_type, ^owner_id, ^kind, sha256, byte_size]]
      when sha256 == receipt.sha256 and byte_size == receipt.byte_size ->
        {:ok, id}

      [[_id, ^owner_type, ^owner_id, existing_kind, existing_sha, existing_size]] ->
        {:error,
         {:artifact_immutable_conflict, relative_path,
          %{
            expected: %{kind: existing_kind, sha256: existing_sha, byte_size: existing_size},
            actual: %{kind: kind, sha256: receipt.sha256, byte_size: receipt.byte_size}
          }}}

      [[_id, existing_type, existing_id, _kind, _sha, _size]] ->
        {:error, {:artifact_owner_conflict, relative_path, existing_type, existing_id}}
    end
  end

  defp replace_cases_and_metrics(baseline_revision_id, definition) do
    Repo.query!("DELETE FROM benchmark_cases WHERE baseline_revision_id = ?", [
      baseline_revision_id
    ])

    Repo.query!("DELETE FROM metric_definitions WHERE baseline_revision_id = ?", [
      baseline_revision_id
    ])

    Enum.each(definition.cases, fn case_ ->
      Repo.query!(
        """
        INSERT INTO benchmark_cases(
          baseline_revision_id, case_id, name, description, inputs_json, weight, critical
        ) VALUES (?, ?, ?, ?, ?, ?, ?)
        """,
        [
          baseline_revision_id,
          case_["id"],
          case_["name"],
          case_["description"],
          Jason.encode!(case_["inputs"]),
          case_["weight"],
          if(case_["critical"], do: 1, else: 0)
        ]
      )
    end)

    Enum.each(definition.metrics, fn metric ->
      Repo.query!(
        """
        INSERT INTO metric_definitions(
          baseline_revision_id, metric_id, unit, direction, role, aggregation_json
        ) VALUES (?, ?, ?, ?, ?, ?)
        """,
        [
          baseline_revision_id,
          metric["id"],
          metric["unit"],
          metric["direction"],
          metric["role"],
          metric |> Map.take(["max_regression_ratio"]) |> Jason.encode!()
        ]
      )
    end)
  end

  defp insert_review(persisted, definition, decision, feedback, now) do
    Repo.query!(
      """
      INSERT INTO baseline_reviews(
        optimization_id, baseline_revision_id, decision, definition_sha256,
        development_sha, dependencies_sha256, feedback, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
      """,
      [
        @optimization_id,
        persisted.id,
        decision,
        definition.manifest_receipt.sha256,
        get_in(definition.manifest, ["development_baseline", "commit_sha"]),
        definition.dependencies_sha256,
        feedback,
        now
      ]
    )
  end

  defp artifact_id_for_kind(persisted, kind) do
    case Repo.query!(
           """
           SELECT id FROM artifacts
           WHERE optimization_id = ? AND owner_type = 'baseline_revision' AND owner_id = ? AND kind = ?
           """,
           [@optimization_id, to_string(persisted.id), kind]
         ).rows do
      [[id]] -> {:ok, id}
      [] -> {:error, {:artifact_not_registered, kind}}
    end
  end

  defp verify_git(work_root, definition) do
    development_sha = get_in(definition.manifest, ["development_baseline", "commit_sha"])

    tracked = ~w(verify_cases.sh benchmark_cases.sh)

    with {:ok, head} <- Git.run(work_root, ["rev-parse", "HEAD"]),
         :ok <- equal(String.trim(head), development_sha, :development_sha_not_head),
         {:ok, status} <- Git.run(work_root, ["status", "--porcelain"]),
         :ok <- equal(String.trim(status), "", :baseline_worktree_not_clean),
         :ok <- verify_tracked(work_root, tracked) do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp verify_tracked(work_root, paths) do
    Enum.reduce_while(paths, :ok, fn path, :ok ->
      case Git.run(work_root, ["ls-files", "--error-unmatch", "--", path]) do
        {:ok, _output} -> {:cont, :ok}
        {:error, _reason} -> {:halt, {:error, {:baseline_file_not_tracked, path}}}
      end
    end)
  end

  defp same_review_identity(persisted, definition) do
    actual = %{
      definition_sha256: definition.manifest_receipt.sha256,
      dependencies_sha256: definition.dependencies_sha256,
      development_sha: get_in(definition.manifest, ["development_baseline", "commit_sha"])
    }

    expected = %{
      definition_sha256: persisted.definition_sha256,
      dependencies_sha256: persisted.dependencies_sha256,
      development_sha: persisted.development_sha
    }

    if actual == expected,
      do: :ok,
      else: {:error, {:baseline_review_identity_changed, %{expected: expected, actual: actual}}}
  end

  defp require_status(%{status: status}, status), do: :ok

  defp require_status(revision, expected),
    do: {:error, {:invalid_baseline_status, revision.status, expected}}

  defp fetch_revision(revision) do
    case Repo.query!(
           """
           SELECT id, revision, status, work_relative_path, definition_artifact_id,
                  definition_sha256, dependencies_sha256, development_sha,
                  target_snapshot_id, review_feedback, terminal_reason,
                  inserted_at, updated_at
           FROM baseline_revisions
           WHERE optimization_id = ? AND revision = ?
           """,
           [@optimization_id, revision]
         ).rows do
      [row] -> {:ok, baseline_revision(row)}
      [] -> {:error, {:baseline_revision_not_found, revision}}
    end
  end

  defp fetch_revision!(revision) do
    {:ok, persisted} = fetch_revision(revision)
    persisted
  end

  defp baseline_revision([
         id,
         revision,
         status,
         work_relative_path,
         definition_artifact_id,
         definition_sha256,
         dependencies_sha256,
         development_sha,
         target_snapshot_id,
         review_feedback,
         terminal_reason,
         inserted_at,
         updated_at
       ]) do
    %{
      id: id,
      revision: revision,
      status: status,
      work_relative_path: work_relative_path,
      definition_artifact_id: definition_artifact_id,
      definition_sha256: definition_sha256,
      dependencies_sha256: dependencies_sha256,
      development_sha: development_sha,
      target_snapshot_id: target_snapshot_id,
      review_feedback: review_feedback,
      terminal_reason: terminal_reason,
      inserted_at: inserted_at,
      updated_at: updated_at
    }
  end

  defp work_relative_path(optimization, work_root) do
    with {:ok, relative} <- workspace_relative(optimization.workspace_canonical_path, work_root),
         :ok <- directory_exists(work_root) do
      {:ok, relative}
    else
      {:error, _reason} = error -> error
    end
  end

  defp workspace_relative(workspace, absolute) do
    workspace = Pika.Paths.canonical!(workspace)
    absolute = Pika.Paths.canonical!(absolute)
    relative = Path.relative_to(absolute, workspace)

    if relative == "." or not String.starts_with?(relative, "..") do
      {:ok, relative}
    else
      {:error, {:path_outside_workspace, absolute}}
    end
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

  defp artifact_kind("baseline-definition.json"), do: "baseline_definition"
  defp artifact_kind("target/manifest.json"), do: "target_manifest"
  defp artifact_kind("cases.json"), do: "cases"
  defp artifact_kind("metrics.json"), do: "metrics"

  defp artifact_kind(path) do
    cond do
      String.contains?(path, "verify") -> "verify"
      String.contains?(path, "benchmark") -> "benchmark"
      true -> "baseline_dependency"
    end
  end

  defp equal(value, value, _reason), do: :ok
  defp equal(_actual, _expected, reason), do: {:error, reason}

  defp directory_exists(path) do
    if File.dir?(path), do: :ok, else: {:error, {:baseline_work_root_missing, path}}
  end

  defp repo_root(work_root) do
    nested = Path.join(work_root, "repo")
    if File.dir?(nested), do: nested, else: work_root
  end

  defp finish({:ok, value}, _tag), do: {:ok, value}
  defp finish({:error, reason}, tag), do: {:error, {tag, reason}}
  defp now_us, do: System.system_time(:microsecond)
end
