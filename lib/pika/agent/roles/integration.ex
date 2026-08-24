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

defmodule Pika.Agent.Roles.Integration.LifecycleReceipts do
  @moduledoc false

  def run(_work, _role_id, _invocation, _request_sha256, operation), do: operation.()

  def run(_work, _role_id, _invocation, _request_sha256, operation, _replay),
    do: operation.()
end

defmodule Pika.Agent.Roles.Integration.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.Integration.Lifecycle
  alias Pika.Repo

  @command_operations %{
    "register_artifact" => :register_artifact,
    "acquire_integration_lease" => :acquire_integration_lease,
    "complete_refresh" => :complete_refresh,
    "submit_fast_rejection" => :submit_fast_rejection,
    "submit_full_regression" => :submit_full_regression,
    "reject_attempt" => :reject_attempt,
    "create_merge_intent" => :create_merge_intent,
    "complete_merge" => :complete_merge,
    "reject_stalled_attempt" => :reject_stalled_attempt
  }

  @impl true
  def operation_receipts, do: Pika.Agent.Roles.Integration.LifecycleReceipts

  @impl true
  def prepare(%Work{id: attempt_id, campaign_id: campaign_id}, workspace) do
    identity =
      lifecycle_identity(campaign_id, attempt_id, active_session_id(campaign_id, attempt_id))

    with {:ok, projection} <- Lifecycle.project(identity),
         integration = projection.context,
         attempt = integration.attempt do
      {:ok,
       %DomainContext{
         facts: Lifecycle.facts(projection),
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
    identity = lifecycle_identity(campaign_id, attempt_id, meta.session_id)

    with {:ok, projection} <- Lifecycle.project(identity) do
      context = projection.context

      {:ok,
       Map.merge(context, %{
         identity: %{
           session_id: meta.session_id,
           campaign_id: campaign_id,
           attempt_id: attempt_id,
           role: "integration"
         },
         required_operations: Lifecycle.required_operations(projection),
         best_worktree: meta.workspace.repo,
         patch_path:
           Path.join(meta.workspace.root, "artifacts/patches/#{attempt_id}/candidate.patch")
       })}
    end
  end

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "register_artifact", args, meta) do
    execute_command(campaign_id, attempt_id, "register_artifact", args, meta)
  end

  def invoke(
        %Work{id: attempt_id, campaign_id: campaign_id},
        "acquire_integration_lease",
        args,
        meta
      ) do
    execute_command(campaign_id, attempt_id, "acquire_integration_lease", args, meta)
  end

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "complete_refresh", args, meta) do
    execute_command(campaign_id, attempt_id, "complete_refresh", args, meta)
  end

  def invoke(
        %Work{id: attempt_id, campaign_id: campaign_id},
        "submit_full_regression",
        args,
        meta
      ),
      do: execute_command(campaign_id, attempt_id, "submit_full_regression", args, meta)

  def invoke(
        %Work{id: attempt_id, campaign_id: campaign_id},
        "submit_fast_rejection",
        args,
        meta
      ),
      do: execute_command(campaign_id, attempt_id, "submit_fast_rejection", args, meta)

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "reject_attempt", args, meta),
    do: execute_command(campaign_id, attempt_id, "reject_attempt", args, meta)

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "create_merge_intent", args, meta),
    do: execute_command(campaign_id, attempt_id, "create_merge_intent", args, meta)

  def invoke(%Work{id: attempt_id, campaign_id: campaign_id}, "complete_merge", args, meta),
    do: execute_command(campaign_id, attempt_id, "complete_merge", args, meta)

  def invoke(_work, operation, _args, _meta),
    do: {:error, {:unsupported_integration_operation, operation}}

  @impl true
  def handle_exhaustion(%Work{id: attempt_id, campaign_id: campaign_id}, :followup_limit, meta) do
    execute_command(
      campaign_id,
      attempt_id,
      "reject_stalled_attempt",
      %{"reason" => "integration agent exceeded 50 forced follow-ups"},
      Map.put(meta, :idempotency_key, "integration-followup-limit-#{attempt_id}")
    )
  end

  def handle_exhaustion(_work, reason, _meta),
    do: {:error, {:unsupported_integration_exhaustion, reason}}

  defp execute_command(campaign_id, attempt_id, operation, args, meta) do
    with {:ok, typed_operation} <- Map.fetch(@command_operations, operation) do
      Lifecycle.execute(
        lifecycle_identity(campaign_id, attempt_id, meta.session_id),
        %Lifecycle.Command{
          operation: typed_operation,
          facts_revision: meta.facts_revision,
          idempotency_key: meta.idempotency_key,
          params: args
        },
        meta.workspace
      )
    else
      :error -> {:error, {:unsupported_integration_operation, operation}}
    end
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

  defp lifecycle_identity(campaign_id, attempt_id, session_id) do
    %Lifecycle.Identity{
      campaign_id: campaign_id,
      attempt_id: attempt_id,
      session_id: session_id
    }
  end

  defp skill_roots(workspace) do
    [Path.join(workspace.root, ".pika/skills/ncu-report-skill")]
    |> Enum.filter(&File.dir?/1)
  end
end
