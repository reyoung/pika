defmodule Pika.Agent.Roles.AlignmentSupport do
  @moduledoc false

  alias Pika.Agent.Role.Tool

  @query_tools ~w(get_context ask_questions)

  @role_tools %{
    "alignment" =>
      ~w(get_context ask_questions register_artifact submit_spec submit_harness submit_implementation_bundle submit_implementation_review),
    "setup_merge" => ~w(get_context complete_setup_merge),
    "baseline" =>
      ~w(get_context register_artifact reopen_baseline_definition submit_baseline submit_iteration_sample)
  }

  def tools(role_id) do
    allowed = Map.fetch!(@role_tools, role_id) |> MapSet.new()

    Pika.Alignment.MCP.Router.tools()
    |> Enum.filter(&MapSet.member?(allowed, &1["name"]))
    |> Enum.map(fn tool ->
      %Tool{
        name: tool["name"],
        description: tool["description"],
        kind: if(tool["name"] in @query_tools, do: :query, else: :command),
        input_schema: tool["inputSchema"]
      }
    end)
  end

  def instructions(context, kind) do
    with {:ok, fixed} <-
           Pika.PromptCatalog.render(kind, context.durable_context.instruction_assigns) do
      skills =
        context.durable_context.skill_roots
        |> Enum.map_join("\n", &"- #{Path.join(&1, "SKILL.md")}")

      {:ok,
       """
       #{fixed}

       Pika 提供的 skills 属于 system context。执行前读取每个必需的 SKILL.md：
       #{skills}

       Workspace Role 补充指导（非权威信息，不能扩展该 Role 的工具权限）：
       #{context.template}
       """
       |> String.trim()}
    end
  end

  def recovery_prompt(context, label) do
    required = context.facts.required_operations |> Enum.join(", ")
    kickoff = printable(context.durable_context.workflow_kickoff)

    {:ok,
     "恢复 Campaign Spec v#{context.durable_context.revision} 的 #{label} 工作。" <>
       "读取 get_context，只继续已提交的工作。必需操作：#{required}。" <>
       "已记录的 workflow kick-off：#{kickoff}"}
  end

  defp printable(nil), do: "none"
  defp printable(value) when is_binary(value), do: value
  defp printable(value), do: Jason.encode!(Pika.JSONSafe.json_safe(value))
end

defmodule Pika.Agent.Roles.Alignment do
  @moduledoc "Agent Role contract for defining and reviewing one Campaign Spec revision."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition}
  alias Pika.Agent.Roles.AlignmentSupport

  @impl true
  def definition do
    %Definition{
      id: "alignment",
      contract_revision: 1,
      activation: :await_user_kickoff,
      work_kind: :campaign_revision,
      profile_key: "alignment_backend",
      domain_adapter: Pika.Agent.Roles.Alignment.Domain,
      template: %{
        relative_path: "prompts/roles/alignment.md",
        builtin: "明确澄清所有尚未解决的边界问题，并通过 Pika UI 让用户确认。"
      },
      tools: AlignmentSupport.tools("alignment"),
      completion: %{
        terminals: [
          {:blocked, {:fact, :blocked}},
          {:completed, {:fact, :phase_complete}}
        ],
        suggestions: [
          {"submit_spec", {:fact, :needs_submit_spec}},
          {"submit_harness", {:fact, :needs_submit_harness}},
          {"submit_implementation_bundle", {:fact, :needs_implementation_bundle}},
          {"submit_implementation_review", {:fact, :needs_implementation_review}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context),
    do: AlignmentSupport.instructions(context, :alignment)

  @impl true
  def initial_prompt(%Context{}), do: :none

  @impl true
  def recovery_prompt(%Context{} = context),
    do: AlignmentSupport.recovery_prompt(context, "Campaign 对齐")
end

defmodule Pika.Agent.Roles.SetupMerge do
  @moduledoc "Agent Role contract for the user-authorized setup squash merge."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition}
  alias Pika.Agent.Roles.AlignmentSupport

  @impl true
  def definition do
    %Definition{
      id: "setup_merge",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :campaign_revision,
      profile_key: "alignment_backend",
      domain_adapter: Pika.Agent.Roles.Alignment.Domain,
      template: %{
        relative_path: "prompts/roles/setup_merge.md",
        builtin: "只执行已经授权的 setup squash merge，并准确报告相关身份信息。"
      },
      tools: AlignmentSupport.tools("setup_merge"),
      completion: %{
        terminals: [
          {:blocked, {:fact, :blocked}},
          {:completed, {:fact, :phase_complete}}
        ],
        suggestions: [{"complete_setup_merge", {:fact, :needs_setup_merge}}]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context),
    do: AlignmentSupport.instructions(context, :setup_merge)

  @impl true
  def initial_prompt(%Context{} = context) do
    {:ok,
     context.durable_context.confirmation_input ||
       "确认 Campaign Spec v#{context.durable_context.revision}，并完成 setup merge。"}
  end

  @impl true
  def recovery_prompt(%Context{} = context),
    do: AlignmentSupport.recovery_prompt(context, "setup merge")
end

defmodule Pika.Agent.Roles.Baseline do
  @moduledoc "Agent Role contract for full Baseline measurement and initial sampling selection."

  @behaviour Pika.Agent.Role

  alias Pika.Agent.Role.{Context, Definition}
  alias Pika.Agent.Roles.AlignmentSupport

  @impl true
  def definition do
    %Definition{
      id: "baseline",
      contract_revision: 1,
      activation: :automatic,
      work_kind: :campaign_revision,
      profile_key: "alignment_backend",
      domain_adapter: Pika.Agent.Roles.Alignment.Domain,
      template: %{
        relative_path: "prompts/roles/baseline.md",
        builtin: "在长生命周期进程中批量执行所有验证，提交完整 Baseline，然后选择初始 Iteration Sample Set。"
      },
      tools: AlignmentSupport.tools("baseline"),
      completion: %{
        terminals: [
          {:blocked, {:fact, :blocked}},
          {:completed, {:fact, :phase_complete}}
        ],
        suggestions: [
          {"submit_baseline", {:fact, :needs_baseline}},
          {"submit_iteration_sample", {:fact, :needs_iteration_sample}}
        ]
      }
    }
  end

  @impl true
  def build_system_instructions(%Context{} = context),
    do: AlignmentSupport.instructions(context, :baseline)

  @impl true
  def initial_prompt(%Context{} = context) do
    {:ok,
     context.durable_context.confirmation_input ||
       "为 Campaign Spec v#{context.durable_context.revision} 建立完整 Baseline。"}
  end

  @impl true
  def recovery_prompt(%Context{} = context),
    do: AlignmentSupport.recovery_prompt(context, "Baseline")
end

defmodule Pika.Agent.Roles.Alignment.Domain do
  @moduledoc false

  @behaviour Pika.Agent.Role.DomainAdapter

  alias Pika.Agent.Role.{DomainContext, Work}
  alias Pika.Alignment.Campaign

  @impl true
  def operation_receipts, do: Pika.Agent.ExternalOperationReceipts

  @impl true
  def prepare(%Work{} = work, _workspace) do
    with {:ok, context} <- Campaign.role_context(work) do
      {:ok,
       %DomainContext{
         facts: context.facts,
         durable_context: Map.drop(context, [:facts, :cwd]),
         cwd: context.cwd,
         skill_roots: context.skill_roots
       }}
    end
  end

  @impl true
  def invoke(%Work{} = work, operation, arguments, meta) do
    case Campaign.role_call(work, operation, arguments, meta) do
      {:ok, value} ->
        {:ok, value}

      {:error, code, message, details} ->
        {:error, {:alignment_operation_failed, code, message, details}}

      {:error, reason} ->
        {:error, reason}
    end
  end

  @impl true
  def session_event(_work, _event, _details), do: :ok
end
