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
     你是 Pika 的 Progress Summary Actor。只依据 get_progress_context 返回的不可变快照工作。
     不要修改文件、运行 shell 命令、声称未观察到的工作已经发生，也不要把推断当作事实。
     清楚区分观察与推断。最后必须且只能调用一次 submit_progress_summary，提交简洁、事实性的中文总结。

     Workspace 指导：
     #{context.template}
     """
     |> String.trim()}
  end

  @impl true
  def initial_prompt(%Context{} = context) do
    {:ok,
     """
     用中文总结这份已捕获的 Pika 进展快照。如有需要，再次调用 get_progress_context 检查，
     然后通过 submit_progress_summary 提交结果。

     #{encoded_context(context)}
     """
     |> String.trim()}
  end

  @impl true
  def recovery_prompt(%Context{} = context) do
    {:ok,
     """
     从已提交快照恢复这次中断的 Progress Summary Request。不要假定任何未提交的先前回复存在。
     使用 submit_progress_summary 提交中文总结。

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
