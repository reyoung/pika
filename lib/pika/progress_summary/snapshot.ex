defmodule Pika.ProgressSummary.Snapshot do
  @moduledoc "Builds the frozen, user-facing v2 Optimization status used by Progress Summary."

  alias Pika.Agent.BackendFailover
  alias Pika.Optimization.{FileContract, Persistence}
  alias Pika.Repo

  @optimization_id "optimization"
  @best_history_limit 20
  @max_iteration_evidence_bytes 1_048_576

  @spec build(non_neg_integer(), DateTime.t()) :: map()
  def build(cursor, %DateTime{} = generated_at) do
    %{
      schema_version: 1,
      generated_at: DateTime.to_iso8601(generated_at),
      snapshot_cursor: cursor,
      optimization: optimization(),
      baseline: latest_baseline(),
      best: latest_best(),
      best_history: best_history(),
      sampling: latest_sampling(),
      attempts: attempts(),
      integrations: integrations(),
      active_agent_sessions: active_sessions(),
      blocked_agent_works: BackendFailover.blocked_works(),
      active_followups: active_followups()
    }
  end

  defp optimization do
    case Persistence.current() do
      nil ->
        nil

      value ->
        Map.take(value, [
          :id,
          :status,
          :resume_status,
          :initial_sha,
          :best_branch,
          :best_sha,
          :stop_reason,
          :inserted_at,
          :updated_at
        ])
    end
  end

  defp latest_baseline do
    case Repo.query!(
           """
           SELECT revision, status, development_sha, terminal_reason, updated_at
           FROM baseline_revisions
           WHERE optimization_id = ? ORDER BY revision DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[revision, status, development_sha, terminal_reason, updated_at]] ->
        %{
          revision: revision,
          status: status,
          development_sha: development_sha,
          terminal_reason: terminal_reason,
          updated_at: updated_at
        }

      [] ->
        nil
    end
  end

  defp latest_best do
    case Repo.query!(
           """
           SELECT sequence, sha, source_kind, source_attempt_id, summary, created_at
           FROM best_revisions
           WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[sequence, sha, source_kind, source_attempt_id, summary, created_at]] ->
        %{
          revision: sequence,
          sha: sha,
          source_kind: source_kind,
          source_attempt_id: source_attempt_id,
          summary: summary,
          created_at: created_at,
          metrics: best_metrics(sequence)
        }

      [] ->
        nil
    end
  end

  defp best_metrics(sequence) do
    Repo.query!(
      """
      SELECT bm.case_id, bm.metric_id, bm.target_value, bm.development_value,
             bm.normalized_ratio, bm.noise_tolerance, bm.valid_pair_count
      FROM best_metrics bm
      JOIN best_revisions br ON br.id = bm.best_revision_id
      WHERE br.optimization_id = ? AND br.sequence = ?
      ORDER BY bm.case_id, bm.metric_id
      """,
      [@optimization_id, sequence]
    ).rows
    |> Enum.map(fn [case_id, metric_id, target, development, ratio, noise, pairs] ->
      %{
        case_id: case_id,
        metric_id: metric_id,
        target_value: target,
        best_value: development,
        normalized_ratio: ratio,
        noise_tolerance: noise,
        valid_pair_count: pairs
      }
    end)
  end

  defp best_history do
    Repo.query!(
      """
      SELECT br.sequence, br.sha, br.source_kind, br.source_attempt_id, br.summary,
             br.created_at, artifact.relative_path, artifact.sha256, artifact.byte_size
      FROM best_revisions br
      LEFT JOIN attempts attempt ON attempt.id = br.source_attempt_id
      LEFT JOIN iteration_rounds round
        ON round.attempt_id = attempt.id AND round.round = attempt.current_iteration_round
      LEFT JOIN artifacts artifact ON artifact.id = round.result_artifact_id
      WHERE br.optimization_id = ?
      ORDER BY br.sequence DESC
      LIMIT ?
      """,
      [@optimization_id, @best_history_limit]
    ).rows
    |> Enum.map(fn [
                     sequence,
                     sha,
                     source_kind,
                     source_attempt_id,
                     summary,
                     created_at,
                     artifact_path,
                     artifact_sha256,
                     artifact_bytes
                   ] ->
      %{
        revision: sequence,
        sha: sha,
        source_kind: source_kind,
        source_attempt_id: source_attempt_id,
        summary: summary,
        created_at: created_at,
        iteration_evidence: iteration_evidence(artifact_path, artifact_sha256, artifact_bytes)
      }
    end)
  end

  defp iteration_evidence(nil, _sha256, _byte_size), do: nil

  defp iteration_evidence(relative_path, expected_sha256, artifact_byte_size)
       when is_binary(relative_path) and is_binary(expected_sha256) and
              is_integer(artifact_byte_size) and
              artifact_byte_size <= @max_iteration_evidence_bytes do
    workspace = Persistence.current().workspace_canonical_path

    with {:ok, contents, receipt} <-
           FileContract.read(workspace, relative_path, max_bytes: @max_iteration_evidence_bytes),
         true <- receipt.byte_size == artifact_byte_size,
         true <- receipt.sha256 == expected_sha256,
         {:ok, result} when is_map(result) <- Jason.decode(contents) do
      details = result["details"] || %{}

      %{
        artifact_path: relative_path,
        status: "verified",
        summary: result["summary"],
        hypothesis: details["hypothesis"],
        changes: details["changes"] || [],
        risks: details["risks"] || []
      }
    else
      _reason -> %{artifact_path: relative_path, status: "unavailable_or_changed"}
    end
  end

  defp iteration_evidence(relative_path, _sha256, _byte_size),
    do: %{artifact_path: relative_path, status: "too_large"}

  defp latest_sampling do
    case Repo.query!(
           """
           SELECT id, sequence, cause, created_at
           FROM sampling_revisions
           WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1
           """,
           [@optimization_id]
         ).rows do
      [[id, sequence, cause, created_at]] ->
        case_ids =
          Repo.query!(
            "SELECT case_id FROM sampling_revision_cases WHERE sampling_revision_id = ? ORDER BY case_id",
            [id]
          ).rows
          |> List.flatten()

        %{revision: sequence, cause: cause, case_ids: case_ids, created_at: created_at}

      [] ->
        nil
    end
  end

  defp attempts do
    Repo.query!(
      """
      SELECT id, status, current_iteration_round, base_best_revision, base_sha,
             candidate_sha, summary, outcome, failure_reason, inserted_at, updated_at
      FROM attempts WHERE optimization_id = ? ORDER BY id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [
                     id,
                     status,
                     round,
                     base_revision,
                     base_sha,
                     candidate_sha,
                     summary,
                     outcome,
                     failure_reason,
                     inserted_at,
                     updated_at
                   ] ->
      %{
        id: id,
        status: status,
        iteration_round: round,
        base_best_revision: base_revision,
        base_sha: base_sha,
        candidate_sha: candidate_sha,
        summary: summary,
        outcome: outcome,
        failure_reason: failure_reason,
        inserted_at: inserted_at,
        updated_at: updated_at
      }
    end)
  end

  defp integrations do
    Repo.query!(
      """
      SELECT attempt_id, fifo_sequence, status, expected_best_sha, outcome, updated_at
      FROM integration_runs WHERE optimization_id = ? ORDER BY fifo_sequence
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [attempt_id, fifo, status, expected, outcome, updated_at] ->
      %{
        attempt_id: attempt_id,
        fifo_sequence: fifo,
        status: status,
        expected_best_sha: expected,
        outcome: outcome,
        updated_at: updated_at
      }
    end)
  end

  defp active_sessions do
    Repo.query!(
      """
      SELECT id, role, work_kind, work_id, session_sequence, backend_chain_sha256,
             backend_chain_index, status, started_at
      FROM agent_sessions
      WHERE optimization_id = ? AND status IN ('running', 'awaiting_report', 'awaiting_followup')
      ORDER BY started_at, id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [
                     id,
                     role,
                     work_kind,
                     work_id,
                     sequence,
                     chain_sha256,
                     chain_index,
                     status,
                     started_at
                   ] ->
      %{
        id: id,
        role: role,
        work_kind: work_kind,
        work_id: work_id,
        session_sequence: sequence,
        backend_chain_sha256: chain_sha256,
        backend_chain_index: chain_index,
        status: status,
        started_at: started_at
      }
    end)
  end

  defp active_followups do
    Repo.query!(
      """
      SELECT id, target_role, target_work_kind, target_work_id,
             target_followup_sequence, generator_attempt_sequence, status, updated_at
      FROM followup_requests
      WHERE optimization_id = ? AND status NOT IN ('target_terminal', 'exhausted', 'failed')
      ORDER BY id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [id, role, kind, work_id, followup, generator_attempt, status, updated_at] ->
      %{
        id: id,
        target_role: role,
        target_work_kind: kind,
        target_work_id: work_id,
        target_followup_sequence: followup,
        generator_attempt_sequence: generator_attempt,
        status: status,
        updated_at: updated_at
      }
    end)
  end
end
