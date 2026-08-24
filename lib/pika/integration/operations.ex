defmodule Pika.Integration.Lifecycle.Operations do
  @moduledoc false

  alias Pika.Alignment.ArtifactStore, as: AlignmentArtifactStore
  alias Pika.Integration.Lifecycle.{Command, Identity}
  alias Pika.{AttemptStore, IntegrationStore, IntegrationWorkspace, Repo}

  def execute(
        %Identity{} = identity,
        %Command{operation: :register_artifact, params: args},
        workspace
      ) do
    attrs = %{
      sha256: param(args, :sha256),
      size: param(args, :size),
      mime: param(args, :mime),
      kind: param(args, :kind),
      metadata: param(args, :metadata) || %{}
    }

    with {:ok, _} <-
           AlignmentArtifactStore.register(workspace.root, param(args, :relative_path), attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(workspace, param(args, :relative_path), %{
             campaign_id: identity.campaign_id,
             owner_type: "attempt",
             owner_id: identity.attempt_id,
             kind: param(args, :kind),
             mime_type: param(args, :mime),
             metadata: param(args, :metadata) || %{}
           }) do
      {:ok, public_artifact(artifact)}
    end
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :complete_refresh, params: args},
        workspace
      ) do
    with {:ok, context} <- AttemptStore.campaign_context(identity.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(identity.attempt_id),
         true <- param(args, :sampling_revision_id) == attempt.sampling_revision_id,
         true <- param(args, :new_base_sha) == context.best_sha,
         {:ok, worktree} <- Pika.AttemptWorkspace.current(workspace, attempt),
         :ok <-
           Pika.AttemptWorkspace.verify_candidate(
             workspace,
             %{worktree | base_sha: context.best_sha},
             param(args, :candidate_sha),
             %{protected_paths: context.protected_paths}
           ),
         true <- param(args, :harness_digest) == context.spec_revision.protected_digest,
         {:ok, samples} <-
           AttemptStore.artifact(identity.campaign_id, param(args, :samples_artifact)),
         {:ok, correctness} <-
           AttemptStore.artifact(identity.campaign_id, param(args, :correctness_artifact)),
         :ok <- own_artifact(samples, identity.attempt_id),
         :ok <- own_artifact(correctness, identity.attempt_id),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: context.best_sha,
             candidate_sha: param(args, :candidate_sha),
             target_snapshot_id: context.target_snapshot.id,
             case_ids: sampled_case_ids(attempt.sampling_revision_id),
             metrics: context.metrics,
             benchmark: context.spec["benchmark"],
             best_metrics: context.best_metrics
           }),
         {:ok, refreshed} <-
           IntegrationStore.complete_refresh(
             param(args, :lease_id),
             identity.session_id,
             identity.attempt_id,
             context.best_sha,
             param(args, :candidate_sha),
             metrics
           ) do
      {:ok, refreshed}
    else
      false -> {:error, :refresh_identity_mismatch}
      {:error, _reason} = error -> error
    end
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :submit_full_regression, params: args},
        workspace
      ),
      do: submit_regression(identity, args, workspace, :full)

  def execute(
        %Identity{} = identity,
        %Command{operation: :submit_fast_rejection, params: args},
        workspace
      ),
      do: submit_regression(identity, args, workspace, :fast_rejection)

  def execute(
        %Identity{} = identity,
        %Command{operation: :reject_attempt, params: args},
        workspace
      ) do
    result =
      IntegrationStore.reject_attempt(
        param(args, :lease_id),
        identity.session_id,
        identity.attempt_id,
        param(args, :receipt_id),
        List.wrap(param(args, :representative_case_ids)),
        param(args, :representative_case_reasons) || %{},
        param(args, :reason)
      )

    cleanup_terminal(result, workspace)
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :create_merge_intent} = command,
        _workspace
      ) do
    IntegrationStore.create_merge_intent(
      param(command.params, :lease_id),
      identity.session_id,
      identity.attempt_id,
      param(command.params, :receipt_id),
      command.idempotency_key
    )
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :complete_merge, params: args},
        workspace
      ) do
    result =
      with {:ok, attempt} <- AttemptStore.attempt(identity.attempt_id),
           {:ok, receipt} <- IntegrationStore.receipt_for_attempt(identity.attempt_id),
           {:ok, intent} <- IntegrationStore.intent_for_attempt(identity.attempt_id),
           true <-
             receipt.id == param(args, :receipt_id) and intent.id == param(args, :intent_id),
           :ok <-
             IntegrationWorkspace.verify_merge(
               workspace,
               attempt,
               receipt,
               intent,
               param(args, :new_sha)
             ),
           {:ok, accepted} <-
             IntegrationStore.complete_merge(
               param(args, :lease_id),
               identity.session_id,
               identity.attempt_id,
               receipt.id,
               intent.id,
               param(args, :new_sha)
             ) do
        {:ok, accepted}
      else
        false -> {:error, :receipt_or_intent_identity_mismatch}
        {:error, _reason} = error -> error
      end

    cleanup_terminal(result, workspace)
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :reject_stalled_attempt, params: args},
        workspace
      ) do
    identity.campaign_id
    |> IntegrationStore.reject_stalled_attempt(
      identity.attempt_id,
      identity.session_id,
      param(args, :reason)
    )
    |> cleanup_terminal(workspace)
  end

  def execute(
        %Identity{} = identity,
        %Command{operation: :block_integration, params: args},
        _workspace
      ) do
    IntegrationStore.block(identity.campaign_id, identity.attempt_id, param(args, :reason))
  end

  def execute(_identity, %Command{operation: operation}, _workspace),
    do: {:error, {:unsupported_integration_operation, operation}}

  def ensure_recoverable_best(workspace, attempt) do
    with {:ok, actual_sha} <- Pika.Git.head(workspace.repo) do
      expected_sha = Pika.Persistence.current_campaign().best_sha

      if actual_sha == expected_sha do
        :ok
      else
        with {:ok, receipt} <- IntegrationStore.receipt_for_attempt(attempt.id),
             {:ok, intent} <- IntegrationStore.intent_for_attempt(attempt.id),
             :ok <-
               IntegrationWorkspace.verify_merge(
                 workspace,
                 attempt,
                 receipt,
                 intent,
                 actual_sha
               ) do
          :ok
        else
          error ->
            {:error,
             {:unexplained_best_state,
              %{expected_sha: expected_sha, actual_sha: actual_sha, verification: inspect(error)}}}
        end
      end
    else
      {:error, reason} ->
        {:error, {:unexplained_best_state, %{verification: inspect(reason)}}}
    end
  end

  defp submit_regression(identity, args, workspace, mode) do
    with {:ok, context} <- AttemptStore.campaign_context(identity.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(identity.attempt_id),
         true <-
           param(args, :base_sha) == context.best_sha and
             param(args, :base_sha) == attempt.base_sha,
         true <- param(args, :candidate_sha) == attempt.candidate_sha,
         true <- param(args, :harness_digest) == context.spec_revision.protected_digest,
         {:ok, samples} <-
           registered_artifact(
             identity.campaign_id,
             param(args, samples_key(mode)),
             identity.attempt_id
           ),
         {:ok, correctness} <-
           registered_artifact(
             identity.campaign_id,
             param(args, :correctness_artifact),
             identity.attempt_id
           ),
         {:ok, full} <-
           full_artifact(identity.campaign_id, identity.attempt_id, args, mode, samples),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(workspace, correctness.relative_path),
         {:ok, full_path} <- optional_path(workspace, full) do
      case evaluate_regression(mode, samples_path, full_path, correctness_path, context, attempt) do
        {:ok, result} ->
          issue_regression_receipt(
            mode,
            args,
            identity,
            context,
            attempt,
            result,
            correctness,
            samples,
            full
          )

        {:error, :correctness_failed} when mode == :full ->
          reject_correctness_failure(
            args,
            identity,
            workspace,
            context,
            attempt,
            correctness,
            samples,
            full
          )

        {:error, _reason} = error ->
          error
      end
    else
      false -> {:error, :full_regression_identity_mismatch}
      {:error, _reason} = error -> error
    end
  end

  defp issue_regression_receipt(
         mode,
         args,
         identity,
         context,
         attempt,
         result,
         correctness,
         samples,
         full
       ) do
    with {:ok, receipt} <-
           IntegrationStore.issue_receipt(
             param(args, :lease_id),
             identity.session_id,
             attempt.id,
             receipt_attrs(mode, context, attempt, result, correctness, samples, full)
           ) do
      response =
        case mode do
          :full -> Map.put(receipt, :escalated, result.escalated)
          :fast_rejection -> Map.put(receipt, :fast_rejection_reason, result.reason)
        end

      {:ok, response}
    end
  end

  defp reject_correctness_failure(
         args,
         identity,
         workspace,
         context,
         attempt,
         correctness,
         screening,
         full
       ) do
    attrs = %{
      candidate_sha: attempt.candidate_sha,
      harness_digest: context.spec_revision.protected_digest,
      metrics: [],
      regressions: [],
      force_reject: true,
      correctness_artifact_id: correctness.id,
      screening_artifact_id: screening.id,
      full_artifact_id: full && full.id
    }

    result =
      with {:ok, receipt} <-
             IntegrationStore.issue_receipt(
               param(args, :lease_id),
               identity.session_id,
               attempt.id,
               attrs
             ),
           {:ok, rejected} <-
             IntegrationStore.reject_attempt(
               param(args, :lease_id),
               identity.session_id,
               attempt.id,
               receipt.id,
               [],
               %{},
               "full regression correctness failed"
             ) do
        {:ok, rejected}
      end

    cleanup_terminal(result, workspace)
  end

  defp evaluate_regression(:full, screening_path, full_path, correctness_path, context, attempt) do
    Pika.Measurement.evaluate_integration(
      screening_path,
      full_path,
      correctness_path,
      regression_identity(context, attempt),
      context.best_metrics
    )
  end

  defp evaluate_regression(
         :fast_rejection,
         samples_path,
         _full_path,
         correctness_path,
         context,
         attempt
       ) do
    Pika.Measurement.evaluate_fast_rejection(
      samples_path,
      correctness_path,
      regression_identity(context, attempt),
      context.best_metrics
    )
  end

  defp regression_identity(context, attempt) do
    %{
      base_sha: attempt.base_sha,
      candidate_sha: attempt.candidate_sha,
      target_snapshot_id: context.target_snapshot.id,
      case_ids: Enum.map(context.cases, & &1["id"]),
      target_case_ids: for(case_ <- context.cases, case_["kind"] == "target", do: case_["id"]),
      metrics: context.metrics,
      benchmark: context.spec["benchmark"]
    }
  end

  defp receipt_attrs(:full, context, attempt, result, correctness, screening, full) do
    %{
      candidate_sha: attempt.candidate_sha,
      harness_digest: context.spec_revision.protected_digest,
      metrics: result.metrics,
      regressions: result.regressions,
      force_reject: not result.best_improvement?,
      correctness_artifact_id: correctness.id,
      screening_artifact_id: screening.id,
      full_artifact_id: full && full.id
    }
  end

  defp receipt_attrs(:fast_rejection, context, attempt, result, correctness, samples, _full) do
    %{
      candidate_sha: attempt.candidate_sha,
      harness_digest: context.spec_revision.protected_digest,
      metrics: result.metrics,
      regressions: result.regressions,
      force_reject: true,
      rejection_reason: result.reason,
      correctness_artifact_id: correctness.id,
      screening_artifact_id: samples.id,
      full_artifact_id: samples.id
    }
  end

  defp samples_key(:full), do: :screening_artifact
  defp samples_key(:fast_rejection), do: :samples_artifact

  defp full_artifact(_campaign_id, _attempt_id, _args, :fast_rejection, samples),
    do: {:ok, samples}

  defp full_artifact(campaign_id, attempt_id, args, :full, _samples),
    do: optional_artifact(campaign_id, param(args, :full_artifact), attempt_id)

  defp registered_artifact(campaign_id, path, attempt_id) do
    with {:ok, artifact} <- AttemptStore.artifact(campaign_id, path),
         :ok <- own_artifact(artifact, attempt_id) do
      {:ok, artifact}
    end
  end

  defp optional_artifact(_campaign_id, nil, _attempt_id), do: {:ok, nil}
  defp optional_artifact(_campaign_id, "", _attempt_id), do: {:ok, nil}

  defp optional_artifact(campaign_id, path, attempt_id),
    do: registered_artifact(campaign_id, path, attempt_id)

  defp optional_path(_workspace, nil), do: {:ok, nil}

  defp optional_path(workspace, artifact),
    do: Pika.ArtifactStore.resolve(workspace, artifact.relative_path)

  defp own_artifact(%{owner_type: "attempt", owner_id: id}, id), do: :ok
  defp own_artifact(_artifact, _attempt_id), do: {:error, :artifact_identity_mismatch}

  defp cleanup_terminal({:ok, attempt} = result, workspace) do
    _ = IntegrationWorkspace.cleanup(workspace, attempt)
    result
  end

  defp cleanup_terminal(error, _workspace), do: error

  defp public_artifact(artifact) do
    %{
      id: artifact.id,
      kind: artifact.kind,
      relative_path: artifact.relative_path,
      sha256: artifact.sha256,
      size: artifact.byte_size,
      mime: artifact.mime_type
    }
  end

  defp sampled_case_ids(sampling_revision_id) do
    Repo.query!(
      "SELECT bc.name FROM sampling_revision_cases src JOIN benchmark_cases bc ON bc.id = src.benchmark_case_id WHERE src.sampling_revision_id = ? ORDER BY bc.ordinal",
      [sampling_revision_id]
    ).rows
    |> List.flatten()
  end

  defp param(params, key) do
    case Map.fetch(params, key) do
      {:ok, value} -> value
      :error -> Map.get(params, Atom.to_string(key))
    end
  end
end
