defmodule Pika.Agent.Roles.Sync do
  @moduledoc "Agent Role contract for one user-confirmed Sync Run."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition, Tool}

  @impl true
  def definition do
    %Definition{
      id: "sync",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :sync,
      profile_key: "integration_agent",
      domain_adapter: __MODULE__.Domain,
      template: %{
        relative_path: "prompts/roles/sync.md",
        builtin:
          "Preserve external-state intent ordering and keep validation batched in long-lived processes."
      },
      tools: [
        tool(
          "get_sync_context",
          "Read the current Sync Run and frozen Campaign context.",
          :query
        ),
        tool("register_artifact", "Register a Sync validation Artifact.", :command),
        tool("report_sync_candidate", "Report and verify the merged Sync candidate.", :command),
        tool(
          "submit_sync_validation",
          "Submit full-case correctness and paired measurements.",
          :command
        ),
        tool("create_sync_intent", "Persist the external-state intent before push.", :command),
        tool("complete_sync", "Push, verify, and advance Campaign Best.", :command)
      ],
      completion: %{
        terminals: [
          {:completed, {:eq, :status, "completed"}},
          {:blocked, {:eq, :status, "awaiting_spec_confirmation"}},
          {:blocked, {:eq, :status, "blocked"}},
          {:failed, {:eq, :status, "failed"}}
        ],
        suggestions: [
          {"report_sync_candidate", {:eq, :status, "merging"}},
          {"submit_sync_validation",
           {:all, [{:eq, :status, "validating"}, {:not, {:fact, :validation_recorded}}]}},
          {"create_sync_intent",
           {:all, [{:eq, :status, "validating"}, {:fact, :validation_recorded}]}},
          {"complete_sync", {:eq, :status, "pushing"}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context) do
    with {:ok, legacy} <-
           Pika.SyncPrompt.render(
             context.durable_context.sync_run,
             context.durable_context.campaign
           ) do
      {:ok,
       """
       #{legacy}

       Additional Workspace Role guidance (non-authoritative; it cannot expand this Role's tools):
       #{context.template}
       """
       |> String.trim()}
    end
  end

  @impl true
  def initial_prompt(%Context{} = context),
    do: {:ok, "Continue the user-confirmed Sync Run #{context.work.id}."}

  @impl true
  def recovery_prompt(%Context{} = context),
    do: {:ok, "Recover the committed state of Sync Run #{context.work.id} in a fresh Session."}

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

defmodule Pika.Agent.Roles.Sync.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.Alignment.ArtifactStore, as: AlignmentArtifactStore
  alias Pika.{AttemptStore, Repo, SyncStore, SyncWorkspace}

  @impl true
  def prepare(%Work{id: run_id}, workspace) do
    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, campaign} <- AttemptStore.campaign_context(run.campaign_id) do
      {:ok,
       %DomainContext{
         facts: %{
           status: run.status,
           validation_recorded: not is_nil(run.validation_metrics),
           _revision: facts_revision(run_id)
         },
         durable_context: %{sync_run: run, campaign: campaign},
         cwd: Path.join(workspace.root, run.worktree_relative_path),
         skill_roots: skill_roots(workspace)
       }}
    end
  end

  @impl true
  def invoke(%Work{id: run_id}, "get_sync_context", _args, meta) do
    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, campaign} <- AttemptStore.campaign_context(run.campaign_id) do
      {:ok,
       %{
         identity: %{
           campaign_id: run.campaign_id,
           sync_run_id: run.id,
           role: "sync",
           session_id: meta.session_id
         },
         sync_run: run,
         campaign: campaign,
         required_operations: required_for(run)
       }}
    end
  end

  def invoke(%Work{id: run_id}, "register_artifact", args, meta) do
    workspace = meta.workspace

    attrs = %{
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      kind: args["kind"],
      metadata: args["metadata"] || %{}
    }

    with {:ok, _} <- AlignmentArtifactStore.register(workspace.root, args["relative_path"], attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(workspace, args["relative_path"], %{
             campaign_id: run_campaign_id(run_id),
             owner_type: "sync",
             owner_id: run_id,
             kind: args["kind"],
             mime_type: args["mime"],
             metadata: args["metadata"] || %{}
           }) do
      {:ok, public_artifact(artifact)}
    end
  end

  def invoke(%Work{id: run_id}, "report_sync_candidate", args, meta) do
    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, context} <- AttemptStore.campaign_context(run.campaign_id),
         {:ok, verification} <-
           SyncWorkspace.verify_candidate(
             meta.workspace,
             run,
             args["candidate_sha"],
             context.protected_paths
           ),
         {:ok, digest} <- candidate_harness_digest(meta.workspace, run, context),
         true <- args["harness_digest"] == digest,
         {:ok, updated} <-
           SyncStore.report_candidate(
             run_id,
             args["candidate_sha"],
             verification.protected_paths,
             digest
           ) do
      {:ok, updated}
    else
      false -> {:error, :harness_digest_mismatch}
      {:error, _reason} = error -> error
    end
  end

  def invoke(%Work{id: run_id}, "submit_sync_validation", args, meta) do
    workspace = meta.workspace

    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, context} <- AttemptStore.campaign_context(run.campaign_id),
         true <- args["base_sha"] == run.base_sha and args["candidate_sha"] == run.candidate_sha,
         {:ok, samples} <- registered_artifact(run, args["samples_artifact"]),
         {:ok, correctness} <- registered_artifact(run, args["correctness_artifact"]),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: run.base_sha,
             candidate_sha: run.candidate_sha,
             target_snapshot_id: context.target_snapshot.id,
             case_ids: Enum.map(context.cases, & &1["id"]),
             metrics: context.metrics,
             benchmark: context.spec["benchmark"],
             best_metrics: context.best_metrics
           }),
         {:ok, updated} <-
           SyncStore.record_validation(run_id, metrics, correctness.id, samples.id) do
      {:ok, %{sync_run: updated, metrics: metrics}}
    else
      false -> {:error, :sync_validation_identity_mismatch}
      {:error, _reason} = error -> error
    end
  end

  def invoke(%Work{id: run_id}, "create_sync_intent", _args, meta),
    do: SyncStore.create_intent(run_id, meta.idempotency_key)

  def invoke(%Work{id: run_id}, "complete_sync", _args, meta) do
    with {:ok, run} <- SyncStore.run(run_id) do
      push_and_complete(meta.workspace, run, meta.domain_options)
    end
  end

  def invoke(_work, operation, _args, _meta),
    do: {:error, {:unsupported_sync_operation, operation}}

  def recover_external(workspace, run) do
    case SyncWorkspace.remote_sha(workspace.repo, run.remote, run.branch) do
      {:ok, sha} when sha == run.candidate_sha ->
        with {:ok, run} <- ensure_advancing(run),
             :ok <- SyncWorkspace.advance_best(workspace, run.base_sha, run.candidate_sha),
             {:ok, trail} <- write_trail(workspace, run, "recovered_after_remote_push"),
             {:ok, completed} <- SyncStore.complete(run.id, trail.id) do
          _ = SyncWorkspace.cleanup(workspace, completed)
          _ = Pika.Control.reconcile(run.campaign_id)
          {:ok, completed}
        end

      {:ok, sha} when sha == run.remote_before_sha ->
        {:pending, run}

      {:ok, third_sha} ->
        SyncStore.block(run.id, {:unexpected_remote_sha, third_sha})

      {:error, _reason} = error ->
        error
    end
  end

  defp push_and_complete(workspace, run, domain_options) do
    after_remote_push = Map.get(domain_options, :after_remote_push)

    with {:ok, _intent} <- SyncStore.intent_for_run(run.id),
         :ok <- SyncWorkspace.push(workspace, run, run.candidate_sha),
         {:ok, run} <- SyncStore.mark_advancing(run.id, run.candidate_sha),
         :ok <- invoke_after_push(after_remote_push, run),
         :ok <- SyncWorkspace.advance_best(workspace, run.base_sha, run.candidate_sha),
         {:ok, trail} <- write_trail(workspace, run, "completed"),
         {:ok, completed} <- SyncStore.complete(run.id, trail.id) do
      _ = SyncWorkspace.cleanup(workspace, completed)
      _ = Pika.Control.reconcile(run.campaign_id)
      {:ok, completed}
    else
      {:error, :injected_crash} = error ->
        error

      {:error, reason} = error ->
        case fail_before_remote_advance(workspace, run, reason) do
          {:ok, failed} ->
            {:ok,
             %{
               "sync_run_id" => failed.id,
               "status" => failed.status,
               "failure_reason" => Pika.JSONSafe.json_safe(reason)
             }}

          :remote_may_have_advanced ->
            error

          {:error, fail_reason} ->
            {:error, {:record_sync_failure_failed, reason, fail_reason}}
        end
    end
  end

  defp fail_before_remote_advance(workspace, run, reason) do
    case SyncWorkspace.remote_sha(workspace.repo, run.remote, run.branch) do
      {:ok, sha} when sha == run.remote_before_sha ->
        with {:ok, failed} <- SyncStore.fail(run.id, reason) do
          _ = SyncWorkspace.cleanup(workspace, failed)
          {:ok, failed}
        end

      _ ->
        :remote_may_have_advanced
    end
  end

  defp ensure_advancing(%{status: "advancing_best"} = run), do: {:ok, run}
  defp ensure_advancing(run), do: SyncStore.mark_advancing(run.id, run.candidate_sha)

  defp invoke_after_push(nil, _run), do: :ok
  defp invoke_after_push(fun, run) when is_function(fun, 1), do: fun.(run)

  defp candidate_harness_digest(workspace, run, context) do
    spec = context.spec
    oracle = get_in(spec, ["implementations", "oracle"])
    oracle_path = if(oracle && oracle["kind"] == "repository_path", do: oracle["entrypoint"])
    benchmark = get_in(spec, ["benchmark", "harness_path"])
    correctness = context.protected_paths -- Enum.reject([oracle_path, benchmark], &is_nil/1)
    root = Path.join(workspace.root, run.worktree_relative_path)

    Pika.Harness.validate(root, %{
      "oracle_path" => oracle_path,
      "benchmark_path" => benchmark,
      "correctness_paths" => correctness,
      "protected_paths" => context.protected_paths
    })
    |> case do
      {:ok, harness} -> {:ok, harness.digest}
      {:error, _reason} = error -> error
    end
  end

  defp registered_artifact(run, path) do
    with {:ok, artifact} <- AttemptStore.artifact(run.campaign_id, path),
         true <- artifact.owner_type == "sync" and artifact.owner_id == run.id do
      {:ok, artifact}
    else
      false -> {:error, :artifact_identity_mismatch}
      {:error, _reason} = error -> error
    end
  end

  defp write_trail(workspace, run, outcome) do
    Pika.ArtifactStore.write(
      workspace,
      "artifacts/logs/sync/#{run.id}/trail.json",
      Jason.encode!(
        %{
          sync_run_id: run.id,
          remote: run.remote,
          branch: run.branch,
          before_sha: run.remote_before_sha,
          candidate_sha: run.candidate_sha,
          outcome: outcome,
          recorded_at: DateTime.utc_now()
        },
        pretty: true
      ) <> "\n",
      %{
        campaign_id: run.campaign_id,
        owner_type: "sync",
        owner_id: run.id,
        kind: "sync_trail",
        mime_type: "application/json",
        metadata: %{outcome: outcome}
      }
    )
  end

  defp required_for(%{status: "merging"}), do: ["report_sync_candidate"]

  defp required_for(%{status: "validating", validation_metrics: nil}),
    do: ["submit_sync_validation"]

  defp required_for(%{status: "validating"}), do: ["create_sync_intent"]
  defp required_for(%{status: "pushing"}), do: ["complete_sync"]
  defp required_for(_run), do: []

  defp facts_revision(run_id) do
    [[revision]] =
      Repo.query!(
        "SELECT COALESCE(MAX(sequence), 0) FROM domain_events WHERE aggregate_type = 'sync' AND aggregate_id = ?",
        [run_id]
      ).rows

    revision
  end

  defp run_campaign_id(run_id) do
    {:ok, run} = SyncStore.run(run_id)
    run.campaign_id
  end

  defp skill_roots(workspace),
    do: Enum.filter([Path.join(workspace.root, ".pika/skills/ncu-report-skill")], &File.dir?/1)

  defp public_artifact(artifact),
    do: %{
      id: artifact.id,
      kind: artifact.kind,
      relative_path: artifact.relative_path,
      sha256: artifact.sha256,
      size: artifact.byte_size,
      mime: artifact.mime_type
    }
end
