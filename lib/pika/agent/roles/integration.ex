defmodule Pika.Agent.Roles.Integration do
  @moduledoc "Agent Role contract for serial pre-merge validation of one Attempt."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition, Tool}

  @impl true
  def definition do
    %Definition{
      id: "integration",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :attempt,
      profile_key: "integration_agent",
      max_followups: 50,
      followup_strategy: :agent,
      domain_adapter: __MODULE__.Domain,
      template: %{
        relative_path: "prompts/roles/integration.md",
        builtin:
          "Keep validation batched in long-lived processes, fail fast on formal rejection evidence, and never mutate Best before a durable merge intent."
      },
      tools: [
        tool("get_integration_context", "Read the current Integration state.", :query),
        tool("register_artifact", "Register an Integration Artifact.", :command),
        tool(
          "acquire_integration_lease",
          "Acquire or recover the FIFO Integration Lease.",
          :command
        ),
        tool("complete_refresh", "Commit a stale-base Attempt refresh.", :command),
        tool(
          "submit_fast_rejection",
          "Submit sufficient formal evidence for early rejection.",
          :command
        ),
        tool("submit_full_regression", "Submit Full Case Set regression evidence.", :command),
        tool("reject_attempt", "Reject the Attempt using its durable Receipt.", :command),
        tool("create_merge_intent", "Persist the merge intent before changing Best.", :command),
        tool("complete_merge", "Verify and commit the prepared Best merge.", :command)
      ],
      completion: %{
        terminals: [
          {:accepted, {:eq, :attempt_status, "accepted"}},
          {:rejected, {:eq, :attempt_status, "rejected"}},
          {:failed, {:eq, :attempt_status, "cancelled"}},
          {:blocked, {:eq, :campaign_status, "blocked"}}
        ],
        suggestions: [
          {"acquire_integration_lease", {:fact, :needs_acquire}},
          {"complete_refresh", {:fact, :needs_refresh}},
          {"submit_full_regression", {:fact, :needs_regression}},
          {"reject_attempt", {:fact, :needs_reject}},
          {"create_merge_intent", {:fact, :needs_intent}},
          {"complete_merge", {:fact, :needs_merge}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context) do
    with {:ok, legacy} <-
           Pika.IntegrationPrompt.render(
             context.durable_context.attempt,
             context.durable_context.integration,
             context.workspace
           ) do
      {:ok,
       """
       #{legacy}

       Workspace Role 补充指导（非权威信息，不能扩展该 Role 的工具权限）：
       #{context.template}
       """
       |> String.trim()}
    end
  end

  @impl true
  def initial_prompt(%Context{} = context),
    do: {:ok, "按 FIFO 顺序集成 Attempt ##{context.durable_context.attempt.ordinal}。"}

  @impl true
  def recovery_prompt(%Context{} = context) do
    {:ok,
     """
     根据已提交的 Lease、Receipt、Intent 和已注册 Artifacts，恢复现有 Attempt
     ##{context.durable_context.attempt.ordinal} 的 Integration。只复用完整且身份绑定的验证输出；
     仅重新运行缺失或不完整的组合。
     """
     |> String.trim()}
  end

  defp tool(name, description, :query) do
    %Tool{
      name: name,
      description: description,
      kind: :query,
      input_schema: %{"type" => "object", "properties" => %{}, "additionalProperties" => false}
    }
  end

  defp tool(name, description, :command) do
    %Tool{
      name: name,
      description: description,
      kind: :command,
      input_schema: %{
        "type" => "object",
        "properties" => %{"idempotency_key" => %{"type" => "string", "minLength" => 1}},
        "required" => ["idempotency_key"],
        "additionalProperties" => true
      }
    }
  end
end

defmodule Pika.Agent.Roles.Integration.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.Alignment.ArtifactStore, as: AlignmentArtifactStore
  alias Pika.{AttemptStore, IntegrationStore, IntegrationWorkspace, Repo}

  @terminal_attempt_statuses ~w(accepted rejected cancelled)

  @impl true
  def prepare(%Work{id: attempt_id, campaign_id: campaign_id}, workspace) do
    with {:ok, integration} <- IntegrationStore.integration_context(campaign_id, attempt_id),
         attempt = integration.attempt do
      flags = next_flags(integration, active_session_id(campaign_id, attempt_id))

      {:ok,
       %DomainContext{
         facts:
           Map.merge(flags, %{
             attempt_status: attempt.status,
             campaign_status: integration.status,
             _revision: facts_revision(campaign_id, attempt_id)
           }),
         durable_context: %{attempt: attempt, integration: integration},
         cwd: Path.join(workspace.root, attempt.worktree_relative_path),
         skill_roots: skill_roots(workspace)
       }}
    end
  end

  @impl true
  def invoke(
        %Work{id: attempt_id, campaign_id: campaign_id},
        "get_integration_context",
        _args,
        meta
      ) do
    with {:ok, context} <- IntegrationStore.integration_context(campaign_id, attempt_id) do
      flags = next_flags(context, meta.session_id)

      {:ok,
       Map.merge(context, %{
         identity: %{
           session_id: meta.session_id,
           campaign_id: campaign_id,
           attempt_id: attempt_id,
           role: "integration"
         },
         required_operations: required_operations(flags),
         best_worktree: meta.workspace.repo,
         patch_path:
           Path.join(meta.workspace.root, "artifacts/patches/#{attempt_id}/candidate.patch")
       })}
    end
  end

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "register_artifact", args, meta) do
    attrs = %{
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      kind: args["kind"],
      metadata: args["metadata"] || %{}
    }

    with {:ok, _} <-
           AlignmentArtifactStore.register(meta.workspace.root, args["relative_path"], attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(meta.workspace, args["relative_path"], %{
             campaign_id: campaign_id,
             owner_type: "attempt",
             owner_id: attempt_id,
             kind: args["kind"],
             mime_type: args["mime"],
             metadata: args["metadata"] || %{}
           }) do
      {:ok, public_artifact(artifact)}
    end
  end

  def invoke(
        %Work{id: attempt_id, campaign_id: campaign_id},
        "acquire_integration_lease",
        args,
        meta
      ) do
    IntegrationStore.acquire_lease(
      campaign_id,
      attempt_id,
      meta.session_id,
      args["expected_best_sha"]
    )
  end

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "complete_refresh", args, meta) do
    with {:ok, context} <- AttemptStore.campaign_context(campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         true <- args["sampling_revision_id"] == attempt.sampling_revision_id,
         true <- args["new_base_sha"] == context.best_sha,
         {:ok, worktree} <- Pika.AttemptWorkspace.current(meta.workspace, attempt),
         :ok <-
           Pika.AttemptWorkspace.verify_candidate(
             meta.workspace,
             %{worktree | base_sha: context.best_sha},
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, samples} <- AttemptStore.artifact(campaign_id, args["samples_artifact"]),
         {:ok, correctness} <- AttemptStore.artifact(campaign_id, args["correctness_artifact"]),
         :ok <- own_artifact(samples, attempt_id),
         :ok <- own_artifact(correctness, attempt_id),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(meta.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(meta.workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: context.best_sha,
             candidate_sha: args["candidate_sha"],
             target_snapshot_id: context.target_snapshot.id,
             case_ids: sampled_case_ids(attempt.sampling_revision_id),
             metrics: context.metrics,
             benchmark: context.spec["benchmark"],
             best_metrics: context.best_metrics
           }),
         {:ok, refreshed} <-
           IntegrationStore.complete_refresh(
             args["lease_id"],
             meta.session_id,
             attempt_id,
             context.best_sha,
             args["candidate_sha"],
             metrics
           ) do
      {:ok, refreshed}
    else
      false -> {:error, :refresh_identity_mismatch}
      {:error, _reason} = error -> error
    end
  end

  def invoke(work, "submit_full_regression", args, meta),
    do: submit_regression(work, args, meta, :full)

  def invoke(work, "submit_fast_rejection", args, meta),
    do: submit_regression(work, args, meta, :fast_rejection)

  def invoke(%Work{id: attempt_id}, "reject_attempt", args, meta) do
    result =
      IntegrationStore.reject_attempt(
        args["lease_id"],
        meta.session_id,
        attempt_id,
        args["receipt_id"],
        List.wrap(args["representative_case_ids"]),
        args["representative_case_reasons"] || %{},
        args["reason"]
      )

    cleanup_terminal(result, meta.workspace)
  end

  def invoke(%Work{id: attempt_id}, "create_merge_intent", args, meta) do
    IntegrationStore.create_merge_intent(
      args["lease_id"],
      meta.session_id,
      attempt_id,
      args["receipt_id"],
      meta.idempotency_key
    )
  end

  def invoke(%Work{id: attempt_id}, "complete_merge", args, meta) do
    result =
      with {:ok, attempt} <- AttemptStore.attempt(attempt_id),
           {:ok, receipt} <- IntegrationStore.receipt_for_attempt(attempt_id),
           {:ok, intent} <- IntegrationStore.intent_for_attempt(attempt_id),
           true <- receipt.id == args["receipt_id"] and intent.id == args["intent_id"],
           :ok <-
             IntegrationWorkspace.verify_merge(
               meta.workspace,
               attempt,
               receipt,
               intent,
               args["new_sha"]
             ),
           {:ok, accepted} <-
             IntegrationStore.complete_merge(
               args["lease_id"],
               meta.session_id,
               attempt_id,
               receipt.id,
               intent.id,
               args["new_sha"]
             ) do
        {:ok, accepted}
      else
        false -> {:error, :receipt_or_intent_identity_mismatch}
        {:error, _reason} = error -> error
      end

    cleanup_terminal(result, meta.workspace)
  end

  def invoke(_work, operation, _args, _meta),
    do: {:error, {:unsupported_integration_operation, operation}}

  @impl true
  def handle_exhaustion(%Work{id: attempt_id, campaign_id: campaign_id}, :followup_limit, meta) do
    result =
      IntegrationStore.reject_stalled_attempt(
        campaign_id,
        attempt_id,
        meta.session_id,
        "integration agent exceeded 50 forced follow-ups"
      )

    cleanup_terminal(result, meta.workspace)
  end

  def handle_exhaustion(_work, reason, _meta),
    do: {:error, {:unsupported_integration_exhaustion, reason}}

  @impl true
  def replay(work, "acquire_integration_lease", args, _stored, meta),
    do: invoke(work, "acquire_integration_lease", args, meta)

  def replay(_work, _operation, _args, stored, _meta), do: {:ok, stored}

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

  def next_flags(context, session_id) do
    attempt = context.attempt
    lease = context.lease
    receipt = context.receipt
    intent = context.intent

    empty = %{
      needs_acquire: false,
      needs_refresh: false,
      needs_regression: false,
      needs_reject: false,
      needs_intent: false,
      needs_merge: false
    }

    cond do
      attempt.status in @terminal_attempt_statuses or context.status == "blocked" ->
        empty

      is_nil(lease) or not is_binary(session_id) or lease.backend_session_id != session_id ->
        %{empty | needs_acquire: true}

      attempt.base_sha != context.best_sha ->
        %{empty | needs_refresh: true}

      is_nil(receipt) ->
        %{empty | needs_regression: true}

      receipt.status == "rejected" ->
        %{empty | needs_reject: true}

      receipt.status == "passed" and is_nil(intent) ->
        %{empty | needs_intent: true}

      receipt.status == "passed" ->
        %{empty | needs_merge: true}

      true ->
        empty
    end
  end

  def required_operations(flags) do
    [
      {:needs_acquire, "acquire_integration_lease"},
      {:needs_refresh, "complete_refresh"},
      {:needs_regression, "submit_full_regression"},
      {:needs_reject, "reject_attempt"},
      {:needs_intent, "create_merge_intent"},
      {:needs_merge, "complete_merge"}
    ]
    |> Enum.flat_map(fn {flag, operation} -> if flags[flag], do: [operation], else: [] end)
  end

  defp submit_regression(
         %Work{id: attempt_id, campaign_id: campaign_id},
         args,
         meta,
         mode
       ) do
    with {:ok, context} <- AttemptStore.campaign_context(campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         true <- args["base_sha"] == context.best_sha and args["base_sha"] == attempt.base_sha,
         true <- args["candidate_sha"] == attempt.candidate_sha,
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, samples} <- registered_artifact(campaign_id, args[samples_key(mode)], attempt_id),
         {:ok, correctness} <-
           registered_artifact(campaign_id, args["correctness_artifact"], attempt_id),
         {:ok, full} <- full_artifact(campaign_id, attempt_id, args, mode, samples),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(meta.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(meta.workspace, correctness.relative_path),
         {:ok, full_path} <- optional_path(meta.workspace, full) do
      case evaluate_regression(
             mode,
             samples_path,
             full_path,
             correctness_path,
             context,
             attempt
           ) do
        {:ok, result} ->
          issue_regression_receipt(
            mode,
            args,
            meta,
            context,
            attempt,
            result,
            correctness,
            samples,
            full
          )

        {:error, :correctness_failed} when mode == :full ->
          reject_correctness_failure(args, meta, context, attempt, correctness, samples, full)

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
         meta,
         context,
         attempt,
         result,
         correctness,
         samples,
         full
       ) do
    with {:ok, receipt} <-
           IntegrationStore.issue_receipt(
             args["lease_id"],
             meta.session_id,
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

  defp reject_correctness_failure(args, meta, context, attempt, correctness, screening, full) do
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
               args["lease_id"],
               meta.session_id,
               attempt.id,
               attrs
             ),
           {:ok, rejected} <-
             IntegrationStore.reject_attempt(
               args["lease_id"],
               meta.session_id,
               attempt.id,
               receipt.id,
               [],
               %{},
               "full regression correctness failed"
             ) do
        {:ok, rejected}
      end

    cleanup_terminal(result, meta.workspace)
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

  defp receipt_attrs(
         :fast_rejection,
         context,
         attempt,
         result,
         correctness,
         samples,
         _full
       ) do
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

  defp samples_key(:full), do: "screening_artifact"
  defp samples_key(:fast_rejection), do: "samples_artifact"

  defp full_artifact(_campaign_id, _attempt_id, _args, :fast_rejection, samples),
    do: {:ok, samples}

  defp full_artifact(campaign_id, attempt_id, args, :full, _samples),
    do: optional_artifact(campaign_id, args["full_artifact"], attempt_id)

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

  defp active_session_id(campaign_id, attempt_id) do
    case Repo.query!(
           "SELECT id FROM agent_sessions WHERE campaign_id = ? AND role = 'integration' AND work_kind = 'attempt' AND work_id = ? AND status IN ('running', 'awaiting_report') ORDER BY started_at DESC LIMIT 1",
           [campaign_id, attempt_id]
         ).rows do
      [[session_id]] -> session_id
      [] -> nil
    end
  end

  defp facts_revision(campaign_id, attempt_id) do
    [[revision]] =
      Repo.query!(
        "SELECT COALESCE(MAX(sequence), 0) FROM domain_events WHERE aggregate_id IN (?, ?)",
        [campaign_id, attempt_id]
      ).rows

    revision
  end

  defp skill_roots(workspace) do
    [Path.join(workspace.root, ".pika/skills/ncu-report-skill")]
    |> Enum.filter(&File.dir?/1)
  end
end
