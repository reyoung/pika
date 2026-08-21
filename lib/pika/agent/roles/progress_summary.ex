defmodule Pika.Agent.Roles.ProgressSummary do
  @moduledoc "Read-only observer Role that durably submits one Progress Summary Request."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition, Tool}

  @impl true
  def definition do
    %Definition{
      id: "progress_summary",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :progress_summary,
      profile_key: "progress_summary",
      domain_adapter: __MODULE__.Domain,
      template: %{
        relative_path: "prompts/roles/progress_summary.md",
        builtin:
          "Focus on material progress, blockers, measurements, and the next expected milestone."
      },
      tools: [
        %Tool{
          name: "get_progress_context",
          description:
            "Return the immutable snapshot captured for this Progress Summary Request.",
          kind: :query,
          input_schema: object_schema(%{}, [])
        },
        %Tool{
          name: "submit_progress_summary",
          description: "Commit the concise factual summary and complete this request.",
          kind: :command,
          input_schema:
            object_schema(
              %{
                "content" => %{"type" => "string", "minLength" => 1},
                "idempotency_key" => %{"type" => "string", "minLength" => 1}
              },
              ["content", "idempotency_key"]
            )
        }
      ],
      completion: %{
        terminals: [
          {:completed, {:eq, :status, "completed"}},
          {:failed, {:eq, :status, "failed"}}
        ],
        suggestions: [
          {"submit_progress_summary", {:eq, :status, "requested"}},
          {"submit_progress_summary", {:eq, :status, "running"}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context) do
    {:ok,
     """
     You are Pika's Progress Summary Actor. Work only from the immutable snapshot returned by
     get_progress_context. Do not modify files, run shell commands, claim unobserved work, or treat
     inference as fact. Clearly distinguish observations from inferences. You must finish by calling
     submit_progress_summary exactly once with a concise factual summary.

     Workspace guidance:
     #{context.template}
     """
     |> String.trim()}
  end

  @impl true
  def initial_prompt(%Context{} = context) do
    {:ok,
     """
     Summarize this captured Pika progress snapshot. Inspect it again with get_progress_context if
     needed, then commit the result with submit_progress_summary.

     #{encoded_context(context)}
     """
     |> String.trim()}
  end

  @impl true
  def recovery_prompt(%Context{} = context) do
    {:ok,
     """
     Recover this interrupted Progress Summary Request from its committed snapshot. Do not assume
     any uncommitted prior response. Submit the summary with submit_progress_summary.

     #{encoded_context(context)}
     """
     |> String.trim()}
  end

  defp encoded_context(context),
    do: Jason.encode!(Pika.JSONSafe.json_safe(context.durable_context.context), pretty: true)

  defp object_schema(properties, required) do
    %{
      "type" => "object",
      "properties" => properties,
      "required" => required,
      "additionalProperties" => false
    }
  end
end

defmodule Pika.Agent.Roles.ProgressSummary.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.ProgressSummaryStore

  @impl true
  def prepare(%Work{id: id}, workspace) do
    with {:ok, request} <- ProgressSummaryStore.get(id) do
      {:ok,
       %DomainContext{
         facts: %{
           status: request.status,
           _revision: request.updated_at || request.created_at
         },
         durable_context: %{
           request_id: request.id,
           phase: request.phase,
           context: request.context
         },
         cwd: Map.get(workspace, :repo) || Map.fetch!(workspace, :root),
         skill_roots: []
       }}
    end
  end

  @impl true
  def invoke(%Work{id: id}, "get_progress_context", _arguments, _meta) do
    with {:ok, request} <- ProgressSummaryStore.get(id) do
      {:ok, %{request_id: request.id, context: request.context}}
    end
  end

  def invoke(%Work{id: id}, "submit_progress_summary", arguments, _meta) do
    content = Map.get(arguments, "content", Map.get(arguments, :content))
    ProgressSummaryStore.submit(id, content)
  end

  def invoke(_work, operation, _arguments, _meta),
    do: {:error, {:unsupported_progress_summary_operation, operation}}
end
