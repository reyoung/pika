defmodule Pika.Optimization.AgentRole.Tool do
  @moduledoc false

  @enforce_keys [:name, :kind]
  defstruct @enforce_keys

  @type t :: %__MODULE__{name: String.t(), kind: :query | :command}
end

defmodule Pika.Optimization.AgentRole.Definition do
  @moduledoc false

  alias Pika.Optimization.AgentRole.Tool

  @enforce_keys [
    :id,
    :activation,
    :work_kind,
    :prompt_module,
    :tools,
    :terminal_commands,
    :optional?,
    :concurrency
  ]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          id: String.t(),
          activation: :automatic | :await_user_kickoff,
          work_kind: atom(),
          prompt_module: module(),
          tools: [Tool.t()],
          terminal_commands: [String.t()],
          optional?: boolean(),
          concurrency: 1 | :iteration_agents
        }
end

defmodule Pika.Optimization.RoleRegistry do
  @moduledoc "Static v2 Agent Role contracts and their configuration projection."

  alias Pika.Optimization.AgentRole.{Definition, Tool}
  alias Pika.Optimization.Config

  @roles %{
    "baseline_alignment" => %Definition{
      id: "baseline_alignment",
      activation: :await_user_kickoff,
      work_kind: :baseline_revision,
      prompt_module: Pika.Agent.RolePrompts.BaselineAlignment,
      tools: [
        %Tool{name: "get_context", kind: :query},
        %Tool{name: "ask_questions", kind: :query},
        %Tool{name: "submit_baseline_definition", kind: :command}
      ],
      terminal_commands: ["submit_baseline_definition"],
      optional?: false,
      concurrency: 1
    },
    "baseline_verify" => %Definition{
      id: "baseline_verify",
      activation: :automatic,
      work_kind: :baseline_revision,
      prompt_module: Pika.Agent.RolePrompts.BaselineVerify,
      tools: [
        %Tool{name: "get_context", kind: :query},
        %Tool{name: "finish_baseline_verification", kind: :command}
      ],
      terminal_commands: ["finish_baseline_verification"],
      optional?: false,
      concurrency: 1
    },
    "baseline_verify_followup" => %Definition{
      id: "baseline_verify_followup",
      activation: :automatic,
      work_kind: :baseline_followup_request,
      prompt_module: Pika.Agent.RolePrompts.BaselineVerifyFollowup,
      tools: [%Tool{name: "submit_followup_message", kind: :command}],
      terminal_commands: ["submit_followup_message"],
      optional?: true,
      concurrency: 1
    },
    "iteration" => %Definition{
      id: "iteration",
      activation: :automatic,
      work_kind: :attempt,
      prompt_module: Pika.Agent.RolePrompts.Iteration,
      tools: [
        %Tool{name: "get_context", kind: :query},
        %Tool{name: "query_attempt_history", kind: :query},
        %Tool{name: "finish_iteration", kind: :command}
      ],
      terminal_commands: ["finish_iteration"],
      optional?: false,
      concurrency: :iteration_agents
    },
    "iteration_followup" => %Definition{
      id: "iteration_followup",
      activation: :automatic,
      work_kind: :attempt_followup_request,
      prompt_module: Pika.Agent.RolePrompts.IterationFollowup,
      tools: [%Tool{name: "submit_followup_message", kind: :command}],
      terminal_commands: ["submit_followup_message"],
      optional?: true,
      concurrency: 1
    },
    "integration" => %Definition{
      id: "integration",
      activation: :automatic,
      work_kind: :attempt,
      prompt_module: Pika.Agent.RolePrompts.Integration,
      tools: [
        %Tool{name: "get_context", kind: :query},
        %Tool{name: "prepare_best_update", kind: :command},
        %Tool{name: "finish_integration", kind: :command}
      ],
      terminal_commands: ["finish_integration"],
      optional?: false,
      concurrency: 1
    },
    "integration_followup" => %Definition{
      id: "integration_followup",
      activation: :automatic,
      work_kind: :integration_followup_request,
      prompt_module: Pika.Agent.RolePrompts.IntegrationFollowup,
      tools: [%Tool{name: "submit_followup_message", kind: :command}],
      terminal_commands: ["submit_followup_message"],
      optional?: true,
      concurrency: 1
    },
    "progress_summary" => %Definition{
      id: "progress_summary",
      activation: :automatic,
      work_kind: :progress_summary_request,
      prompt_module: Pika.Agent.RolePrompts.ProgressSummary,
      tools: [%Tool{name: "submit_progress_summary", kind: :command}],
      terminal_commands: ["submit_progress_summary"],
      optional?: true,
      concurrency: 1
    }
  }

  @spec all() :: %{String.t() => Definition.t()}
  def all, do: @roles

  @spec fetch(atom() | String.t()) ::
          {:ok, Definition.t()} | {:error, {:unknown_agent_role, String.t()}}
  def fetch(role_id) when is_atom(role_id), do: fetch(Atom.to_string(role_id))

  def fetch(role_id) when is_binary(role_id) do
    case Map.fetch(@roles, role_id) do
      {:ok, definition} -> {:ok, definition}
      :error -> {:error, {:unknown_agent_role, role_id}}
    end
  end

  @spec enabled(Config.t()) :: [Definition.t()]
  def enabled(%Config{} = config) do
    @roles
    |> Map.values()
    |> Enum.filter(&(backend_configs(&1, config) != []))
    |> Enum.sort_by(& &1.id)
  end

  @spec backend_configs(Definition.t(), Config.t()) :: [Config.Agent.t()]
  def backend_configs(%Definition{id: "iteration"}, %Config{} = config),
    do: Config.iteration_agents(config)

  def backend_configs(%Definition{id: role_id}, %Config{} = config) do
    case Config.role_agent(config, role_id) do
      {:ok, agent} -> [agent]
      {:error, {:agent_role_disabled, ^role_id}} -> []
      {:error, {:unknown_agent_role, ^role_id}} -> []
    end
  end

  @spec concurrency(Definition.t(), Config.t()) :: non_neg_integer()
  def concurrency(%Definition{} = definition, %Config{} = config),
    do: length(backend_configs(definition, config))

  @spec validate() :: :ok | {:error, term()}
  def validate do
    Enum.reduce_while(@roles, :ok, fn {role_id, definition}, :ok ->
      tool_names = Enum.map(definition.tools, & &1.name)

      cond do
        role_id != definition.id ->
          {:halt, {:error, {:role_id_mismatch, role_id, definition.id}}}

        Enum.uniq(tool_names) != tool_names ->
          {:halt, {:error, {:duplicate_role_tools, role_id}}}

        not MapSet.subset?(MapSet.new(definition.terminal_commands), MapSet.new(tool_names)) ->
          {:halt, {:error, {:terminal_command_not_exposed, role_id}}}

        Pika.Agent.RolePromptRegistry.fetch(role_id) != {:ok, definition.prompt_module} ->
          {:halt, {:error, {:role_prompt_mismatch, role_id}}}

        true ->
          {:cont, :ok}
      end
    end)
  end

  @spec validate!() :: :ok
  def validate! do
    case validate() do
      :ok -> :ok
      {:error, reason} -> raise "invalid v2 Agent Role registry: #{inspect(reason)}"
    end
  end
end
