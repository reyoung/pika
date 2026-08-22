defmodule Pika.Agent.Roles.AttemptSupport do
  @moduledoc false

  alias Pika.Agent.Role.Tool

  @shared_tools [
    {"get_context", "Read the immutable Campaign and own Attempt context.", :query},
    {"query_attempt_history", "Query terminal Attempt history.", :query},
    {"get_attempt", "Read this Attempt or another terminal Attempt.", :query},
    {"list_agents", "List Agent Sessions in this Campaign.", :query},
    {"read_agent_messages", "Read unacknowledged Mailbox messages.", :query},
    {"ack_agent_messages", "Acknowledge Mailbox messages.", :command},
    {"send_agent_message", "Persist a direct message to another Agent Session.", :command},
    {"register_artifact", "Register an existing Workspace Artifact.", :command}
  ]

  def shared_tools,
    do: Enum.map(@shared_tools, fn {name, description, kind} -> tool(name, description, kind) end)

  def tool(name, description, :query) do
    %Tool{
      name: name,
      description: description,
      kind: :query,
      input_schema: %{"type" => "object", "properties" => %{}, "additionalProperties" => true}
    }
  end

  def tool(name, description, :command) do
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

  def instructions(context, role) do
    attempt = context.durable_context.attempt
    campaign = context.durable_context.campaign

    with {:ok, legacy} <- Pika.AttemptPrompt.render(role, attempt, campaign, context.workspace) do
      recovery =
        if context.session_mode == :recovering,
          do: Pika.Agent.Roles.AttemptRecovery.instructions(context),
          else: ""

      {:ok,
       """
       #{legacy}

       Additional Workspace Role guidance (non-authoritative; it cannot expand this Role's tools):
       #{context.template}
       #{recovery}
       """
       |> String.trim()}
    end
  end
end

defmodule Pika.Agent.Roles.Plan do
  @moduledoc "Agent Role contract for the optional planning phase of one Attempt."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition}
  alias Pika.Agent.Roles.AttemptSupport

  @impl true
  def definition do
    %Definition{
      id: "plan",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :attempt,
      profile_key: "iteration_agents",
      domain_adapter: Pika.Agent.Roles.Attempt.Domain,
      template: %{
        relative_path: "prompts/roles/plan.md",
        builtin:
          "Produce one focused optimization plan. Do not implement or benchmark the candidate."
      },
      tools:
        AttemptSupport.shared_tools() ++
          [
            AttemptSupport.tool(
              "submit_plan",
              "Atomically publish the Attempt plan Markdown.",
              :command
            )
          ],
      completion: %{
        terminals: [
          {:completed, {:fact, :plan_submitted}},
          {:failed, {:eq, :attempt_status, "cancelled"}},
          {:blocked, {:eq, :campaign_status, "blocked"}}
        ],
        suggestions: [{"submit_plan", {:not, {:fact, :plan_submitted}}}]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context),
    do: AttemptSupport.instructions(context, :plan)

  @impl true
  def initial_prompt(%Context{} = context),
    do:
      {:ok,
       "Prepare the focused optimization plan for Attempt ##{context.durable_context.attempt.ordinal}."}

  @impl true
  def recovery_prompt(%Context{} = context),
    do: {:ok, recovery_prompt_text(context, "plan")}

  defp recovery_prompt_text(context, role) do
    required = context.facts |> Pika.Agent.Roles.Attempt.Domain.required_operations()

    "Recover #{role} work for the existing Attempt ##{context.durable_context.attempt.ordinal}; complete: " <>
      Enum.join(required, ", ") <> "."
  end
end

defmodule Pika.Agent.Roles.Iteration do
  @moduledoc "Agent Role contract for implementation and measurement of one Attempt."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition}
  alias Pika.Agent.Roles.AttemptSupport

  @impl true
  def definition do
    %Definition{
      id: "iteration",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :attempt,
      profile_key: "iteration_agents",
      domain_adapter: Pika.Agent.Roles.Attempt.Domain,
      template: %{
        relative_path: "prompts/roles/iteration.md",
        builtin:
          "Optimize only the Attempt worktree. Batch validation in long-lived processes and reject early when committed evidence proves the candidate is not worthwhile."
      },
      tools:
        AttemptSupport.shared_tools() ++
          [
            AttemptSupport.tool(
              "record_metrics",
              "Submit formal alternating-pair measurements.",
              :command
            ),
            AttemptSupport.tool(
              "submit_attempt_summary",
              "Submit the structured Attempt summary.",
              :command
            ),
            AttemptSupport.tool(
              "reject_attempt",
              "Reject this Attempt directly and skip Integration.",
              :command
            ),
            AttemptSupport.tool(
              "complete_attempt",
              "Run the completion gate for this Attempt.",
              :command
            )
          ],
      completion: %{
        terminals: [
          {:completed, {:eq, :attempt_status, "ready_for_integration"}},
          {:rejected, {:eq, :attempt_status, "rejected"}},
          {:failed, {:eq, :attempt_status, "cancelled"}},
          {:blocked, {:eq, :campaign_status, "blocked"}}
        ],
        suggestions: [
          {"record_metrics", {:fact, :needs_metrics}},
          {"submit_attempt_summary", {:fact, :needs_summary}},
          {"reject_attempt", {:fact, :needs_reject}},
          {"complete_attempt", {:fact, :needs_completion}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context),
    do: AttemptSupport.instructions(context, :iteration)

  @impl true
  def initial_prompt(%Context{} = context),
    do:
      {:ok,
       "Run Attempt ##{context.durable_context.attempt.ordinal} from its fixed Best and Sampling Revision."}

  @impl true
  def recovery_prompt(%Context{} = context) do
    required = context.facts |> Pika.Agent.Roles.Attempt.Domain.required_operations()

    {:ok,
     "Recover iteration work for the existing Attempt ##{context.durable_context.attempt.ordinal}; complete: " <>
       Enum.join(required, ", ") <> "."}
  end
end

defmodule Pika.Agent.Roles.Attempt.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.Alignment.ArtifactStore, as: StageArtifactStore
  alias Pika.{AttemptStore, AttemptWorkspace, Repo}

  @impl true
  def prepare(%Work{} = work, workspace) do
    with {:ok, campaign} <- AttemptStore.campaign_context(work.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(work.id) do
      facts = facts(work.role_id, campaign, attempt)

      {:ok,
       %DomainContext{
         facts: Map.put(facts, :_revision, facts_revision(work.campaign_id, work.id)),
         durable_context: %{
           attempt: attempt,
           campaign: campaign,
           required_operations: required_operations(facts)
         },
         cwd: Path.join(workspace.root, attempt.worktree_relative_path),
         skill_roots: skill_roots(workspace)
       }}
    end
  end

  @impl true
  def invoke(%Work{} = work, "get_context", _args, meta) do
    with {:ok, campaign} <- AttemptStore.campaign_context(work.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(work.id) do
      facts = facts(work.role_id, campaign, attempt)

      {:ok,
       %{
         identity: %{
           session_id: meta.session_id,
           campaign_id: work.campaign_id,
           attempt_id: work.id,
           role: role_atom(work.role_id),
           slot_index: attempt.slot_index
         },
         campaign: campaign,
         attempt: enrich_attempt(attempt),
         required_operations: required_operations(facts),
         guidance:
           AttemptStore.guidance_for_attempt(work.campaign_id, attempt.id, attempt.created_at)
       }}
    end
  end

  def invoke(%Work{campaign_id: campaign_id}, "query_attempt_history", args, _meta) do
    limit = args["limit"] || 100

    if is_integer(limit) and limit in 1..500 do
      history =
        AttemptStore.query_terminal_history(campaign_id,
          limit: limit,
          before_ordinal: args["before_ordinal"],
          outcome: args["outcome"]
        )
        |> Enum.map(&enrich_attempt/1)

      {:ok, history}
    else
      {:error, :invalid_attempt_history_limit}
    end
  end

  def invoke(%Work{id: own_id}, "get_attempt", args, _meta) do
    id = args["attempt_id"] || own_id

    case AttemptStore.attempt(id) do
      {:ok, attempt} when id == own_id or attempt.status in ~w(accepted rejected cancelled) ->
        {:ok, enrich_attempt(attempt)}

      {:ok, _attempt} ->
        {:error, :attempt_not_readable}

      {:error, _reason} = error ->
        error
    end
  end

  def invoke(%Work{campaign_id: campaign_id}, "list_agents", _args, _meta),
    do: {:ok, AttemptStore.sessions(campaign_id)}

  def invoke(_work, "read_agent_messages", args, meta),
    do:
      AttemptStore.read_messages(
        meta.session_id,
        args["after_sequence"] || 0,
        args["limit"] || 100
      )

  def invoke(_work, "ack_agent_messages", args, meta),
    do: AttemptStore.ack_messages(meta.session_id, args["through_sequence"] || 0)

  def invoke(%Work{campaign_id: campaign_id}, "send_agent_message", args, meta) do
    body = String.trim(args["body"] || "")
    priority = args["priority"] || "normal"

    cond do
      body == "" ->
        {:error, :message_body_required}

      priority not in ~w(normal high) ->
        {:error, :invalid_message_priority}

      true ->
        AttemptStore.send_message(
          campaign_id,
          meta.session_id,
          args["target_session_id"],
          body,
          priority
        )
    end
  end

  def invoke(%Work{} = work, "register_artifact", args, meta) do
    attrs = %{
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      kind: args["kind"],
      metadata: args["metadata"] || %{}
    }

    with {:ok, _verified} <-
           StageArtifactStore.register(meta.workspace.root, args["relative_path"], attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(meta.workspace, args["relative_path"], %{
             campaign_id: work.campaign_id,
             owner_type: "attempt",
             owner_id: work.id,
             kind: args["kind"],
             mime_type: args["mime"],
             metadata: args["metadata"] || %{}
           }) do
      {:ok, public_artifact(artifact)}
    end
  end

  def invoke(%Work{role_id: "plan"} = work, "submit_plan", args, meta) do
    markdown = String.trim(args["markdown"] || "")
    summary = String.trim(args["summary"] || "")

    if markdown == "" or summary == "" do
      {:error, :plan_content_required}
    else
      path = "artifacts/plans/#{work.id}/plan.md"

      with {:ok, artifact} <-
             Pika.ArtifactStore.write(meta.workspace, path, markdown <> "\n", %{
               campaign_id: work.campaign_id,
               owner_type: "attempt",
               owner_id: work.id,
               kind: "plan",
               mime_type: "text/markdown",
               metadata: %{summary: summary}
             }),
           :ok <- AttemptStore.attach_artifact(work.id, "plan_artifact_id", artifact.id) do
        {:ok, %{artifact: public_artifact(artifact), summary: summary}}
      end
    end
  end

  def invoke(%Work{role_id: "iteration"} = work, "record_metrics", args, meta) do
    with {:ok, context} <- AttemptStore.campaign_context(work.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(work.id),
         :ok <- validate_attempt_identity(args, attempt, context),
         {:ok, worktree} <- AttemptWorkspace.current(meta.workspace, attempt),
         :ok <-
           AttemptWorkspace.verify_candidate(
             meta.workspace,
             worktree,
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, samples} <- AttemptStore.artifact(work.campaign_id, args["samples_artifact"]),
         {:ok, correctness} <-
           AttemptStore.artifact(work.campaign_id, args["correctness_artifact"]),
         :ok <- verify_attempt_artifact(samples, work.id),
         :ok <- verify_attempt_artifact(correctness, work.id),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(meta.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(meta.workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: attempt.base_sha,
             candidate_sha: args["candidate_sha"],
             target_snapshot_id: context.target_snapshot.id,
             case_ids: context.sampled_case_ids,
             metrics: context.metrics,
             benchmark: context.spec["benchmark"],
             best_metrics: context.best_metrics
           }),
         {:ok, _event} <- AttemptStore.record_metrics(work.id, metrics, args["candidate_sha"]),
         :ok <- AttemptStore.attach_artifact(work.id, "metrics_artifact_id", samples.id),
         :ok <- AttemptStore.attach_artifact(work.id, "correctness_artifact_id", correctness.id) do
      {:ok, %{metrics: metrics, source: "iteration"}}
    else
      false -> {:error, :harness_digest_mismatch}
      {:error, _reason} = error -> error
    end
  end

  def invoke(%Work{role_id: "iteration"} = work, "submit_attempt_summary", args, _meta) do
    attrs = %{
      description: String.trim(args["description"] || ""),
      summary: String.trim(args["summary"] || ""),
      modification_scope: List.wrap(args["modification_scope"]),
      risks: List.wrap(args["risks"]),
      profiler_summary: args["profiler_summary"],
      recommended_outcome: args["recommended_outcome"]
    }

    if attrs.description == "" or attrs.summary == "" do
      {:error, :attempt_summary_required}
    else
      with {:ok, _event} <- AttemptStore.submit_summary(work.id, attrs), do: {:ok, attrs}
    end
  end

  def invoke(%Work{role_id: "iteration"} = work, "reject_attempt", args, _meta) do
    reason = String.trim(args["reason"] || "")

    if reason == "",
      do: {:error, :rejection_reason_required},
      else:
        with(
          {:ok, rejected} <- AttemptStore.reject_from_iteration(work.id, reason),
          do: {:ok, enrich_attempt(rejected)}
        )
  end

  def invoke(%Work{role_id: "iteration"} = work, "complete_attempt", args, meta) do
    with {:ok, context} <- AttemptStore.campaign_context(work.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(work.id),
         :ok <- validate_attempt_identity(args, attempt, context),
         {:ok, worktree} <- AttemptWorkspace.current(meta.workspace, attempt),
         :ok <-
           AttemptWorkspace.verify_candidate(
             meta.workspace,
             worktree,
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         {:ok, patch} <- AttemptWorkspace.patch(meta.workspace, worktree, args["candidate_sha"]),
         {:ok, artifact} <- write_patch(meta.workspace, work, attempt, patch),
         :ok <- AttemptStore.attach_artifact(work.id, "patch_artifact_id", artifact.id),
         expected <- AttemptStore.expected_metric_count(work.id),
         {:ok, completed} <-
           AttemptStore.complete_attempt(work.id, args["candidate_sha"], expected) do
      {:ok, enrich_attempt(completed)}
    end
  end

  def invoke(%Work{} = work, operation, _args, _meta),
    do: {:error, {:unsupported_attempt_operation, work.role_id, operation}}

  @impl true
  def session_event(%Work{id: attempt_id}, :running, _details) do
    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status}} when status in ~w(queued interrupted awaiting_report) ->
        AttemptStore.mark_running(attempt_id) |> normalize_event_result()

      {:ok, _attempt} ->
        :ok

      {:error, _reason} = error ->
        error
    end
  end

  def session_event(%Work{id: attempt_id}, :awaiting_report, %{required: required}) do
    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status}} when status in ~w(running awaiting_report) ->
        AttemptStore.mark_awaiting_report(attempt_id, required) |> normalize_event_result()

      {:ok, _attempt} ->
        :ok

      {:error, _reason} = error ->
        error
    end
  end

  def session_event(%Work{id: attempt_id}, :interrupted, details) do
    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status}} when status in ~w(running awaiting_report interrupted) ->
        AttemptStore.mark_interrupted(attempt_id, details) |> normalize_event_result()

      {:ok, _attempt} ->
        :ok

      {:error, _reason} = error ->
        error
    end
  end

  def session_event(_work, _event, _details), do: :ok

  def required_operations(facts) do
    [
      {:needs_plan, "submit_plan"},
      {:needs_metrics, "record_metrics"},
      {:needs_summary, "submit_attempt_summary"},
      {:needs_reject, "reject_attempt"},
      {:needs_completion, "complete_attempt"}
    ]
    |> Enum.flat_map(fn {flag, operation} -> if facts[flag], do: [operation], else: [] end)
  end

  defp facts("plan", campaign, attempt) do
    %{
      attempt_status: attempt.status,
      campaign_status: campaign.status,
      plan_submitted: not is_nil(attempt.plan_artifact_id),
      needs_plan: is_nil(attempt.plan_artifact_id)
    }
  end

  defp facts("iteration", campaign, attempt) do
    terminal? = attempt.status in ~w(ready_for_integration accepted rejected cancelled)

    reject? =
      not terminal? and attempt.recommended_outcome in ~w(skip reject) and
        not is_nil(attempt.summary)

    %{
      attempt_status: attempt.status,
      campaign_status: campaign.status,
      needs_metrics:
        not terminal? and not reject? and AttemptStore.metrics_for_attempt(attempt.id) == [],
      needs_summary: not terminal? and not reject? and is_nil(attempt.summary),
      needs_reject: reject?,
      needs_completion:
        not terminal? and not reject? and attempt.status != "ready_for_integration"
    }
  end

  defp facts(_role, campaign, attempt),
    do: %{attempt_status: attempt.status, campaign_status: campaign.status}

  defp validate_attempt_identity(args, attempt, context) do
    cond do
      args["sampling_revision_id"] != attempt.sampling_revision_id ->
        {:error, :sampling_revision_mismatch}

      args["base_sha"] != attempt.base_sha ->
        {:error, :base_sha_mismatch}

      attempt.base_sha != context.best_sha ->
        {:error, {:stale_best, context.best_sha}}

      not valid_sha?(args["candidate_sha"]) ->
        {:error, :invalid_candidate_sha}

      true ->
        :ok
    end
  end

  defp verify_attempt_artifact(%{owner_type: "attempt", owner_id: attempt_id}, attempt_id),
    do: :ok

  defp verify_attempt_artifact(_artifact, _attempt_id), do: {:error, :artifact_identity_mismatch}

  defp write_patch(workspace, work, attempt, patch) do
    Pika.ArtifactStore.write(
      workspace,
      "artifacts/patches/#{work.id}/candidate.patch",
      patch,
      %{
        campaign_id: work.campaign_id,
        owner_type: "attempt",
        owner_id: work.id,
        kind: "patch",
        mime_type: "text/x-diff",
        metadata: %{base_sha: attempt.base_sha, candidate_sha: attempt.candidate_sha}
      }
    )
  end

  defp enrich_attempt(attempt),
    do: Map.put(attempt, :metrics, AttemptStore.metrics_for_attempt(attempt.id))

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

  defp facts_revision(campaign_id, attempt_id) do
    [[revision]] =
      Repo.query!(
        "SELECT COALESCE(MAX(sequence), 0) FROM domain_events WHERE aggregate_id IN (?, ?)",
        [campaign_id, attempt_id]
      ).rows

    revision
  end

  defp role_atom("plan"), do: :plan
  defp role_atom("iteration"), do: :iteration
  defp role_atom(role), do: role

  defp skill_roots(workspace) do
    [Path.join(workspace.root, ".pika/skills/ncu-report-skill")]
    |> Enum.filter(&File.dir?/1)
  end

  defp valid_sha?(value) when is_binary(value), do: String.match?(value, ~r/^[0-9a-f]{40}$/)
  defp valid_sha?(_value), do: false

  defp normalize_event_result({:ok, _value}), do: :ok
  defp normalize_event_result({:error, _reason} = error), do: error
end

defmodule Pika.Agent.Roles.AttemptRecovery do
  @moduledoc false

  @tail_records 50
  @line_max_bytes 262_144
  @data_max_bytes 4_000

  def instructions(context) do
    attempt = context.durable_context.attempt
    required = context.durable_context.required_operations

    """

    This is a recovery Session for the same Attempt. Do not create a new Attempt. Continue in the
    existing worktree and complete only the missing operations: #{Enum.join(required, ", ")}.

    Recovery JSONL tail (oldest to newest):
    #{Jason.encode!(jsonl_tail(context.workspace, attempt.id), pretty: true)}
    """
  end

  defp jsonl_tail(workspace, attempt_id) do
    pattern = Path.join([workspace.root, "artifacts", "logs", attempt_id, "*.jsonl"])

    pattern
    |> Path.wildcard()
    |> Enum.sort()
    |> Enum.flat_map(fn path ->
      relative = Path.relative_to(path, workspace.root)

      path
      |> File.stream!(:line)
      |> Stream.filter(&event_line?/1)
      |> Stream.map(&Jason.decode/1)
      |> Stream.filter(fn
        {:ok, %{"type" => type}} when is_binary(type) -> true
        _other -> false
      end)
      |> Stream.map(fn {:ok, record} ->
        %{"artifact" => relative, "record" => compact_record(record)}
      end)
      |> Enum.to_list()
    end)
    |> Enum.take(-@tail_records)
  rescue
    _error -> []
  end

  defp event_line?(line) do
    byte_size(line) <= @line_max_bytes and
      not String.contains?(binary_part(line, 0, min(byte_size(line), 256)), "\"direction\":")
  end

  defp compact_record(record) do
    compact = Map.take(record, ~w(at session_id turn_id type backend role))
    data = Map.get(record, "data")

    if data in [nil, %{}, "", []], do: compact, else: Map.put(compact, "data", compact_data(data))
  end

  defp compact_data(data) do
    encoded = data |> Pika.JSONSafe.json_safe() |> Jason.encode!()

    if byte_size(encoded) <= @data_max_bytes do
      data
    else
      %{
        "truncated" => true,
        "preview" => String.slice(encoded, 0, @data_max_bytes),
        "original_bytes" => byte_size(encoded)
      }
    end
  end
end
