defmodule Pika.Agent.Role.Work do
  @moduledoc "Stable identity of one domain-owned Agent Work."

  @enforce_keys [:role_id, :kind, :id, :campaign_id]
  defstruct [:role_id, :kind, :id, :campaign_id]

  @type t :: %__MODULE__{
          role_id: String.t(),
          kind: atom(),
          id: String.t(),
          campaign_id: String.t()
        }
end

defmodule Pika.Agent.Role.Tool do
  @moduledoc false

  @enforce_keys [:name, :description, :kind, :input_schema]
  defstruct [:name, :description, :kind, :input_schema]

  @type t :: %__MODULE__{
          name: String.t(),
          description: String.t(),
          kind: :query | :command,
          input_schema: map()
        }
end

defmodule Pika.Agent.Role.Definition do
  @moduledoc false

  @enforce_keys [
    :id,
    :contract_revision,
    :activation,
    :work_kind,
    :profile_key,
    :domain_adapter,
    :template,
    :tools,
    :completion
  ]
  defstruct @enforce_keys ++ [max_followups: 3]

  @type t :: %__MODULE__{
          id: String.t(),
          contract_revision: pos_integer(),
          activation: :automatic | :await_user_kickoff,
          work_kind: atom(),
          profile_key: String.t(),
          domain_adapter: module(),
          max_followups: non_neg_integer(),
          template: %{required(:relative_path) => Path.t(), required(:builtin) => String.t()},
          tools: [Pika.Agent.Role.Tool.t()],
          completion: map()
        }
end

defmodule Pika.Agent.Role.DomainContext do
  @moduledoc false

  @enforce_keys [:facts, :durable_context, :cwd]
  defstruct [:facts, :durable_context, :cwd, skill_roots: []]

  @type t :: %__MODULE__{
          facts: map(),
          durable_context: map(),
          cwd: Path.t(),
          skill_roots: [Path.t()]
        }
end

defmodule Pika.Agent.Role.Context do
  @moduledoc false

  @enforce_keys [:work, :workspace, :facts, :durable_context, :template, :session_mode]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          work: Pika.Agent.Role.Work.t(),
          workspace: map(),
          facts: map(),
          durable_context: map(),
          template: String.t(),
          session_mode: :fresh | :recovering
        }
end

defmodule Pika.Agent.Role.Instructions do
  @moduledoc false

  @enforce_keys [:system, :sha256, :template_sha256, :template_source]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          system: String.t(),
          sha256: String.t(),
          template_sha256: String.t(),
          template_source: :builtin | {:workspace, Path.t()}
        }
end

defmodule Pika.Agent.Role.Progress do
  @moduledoc false

  @enforce_keys [:state, :required_operations, :facts_revision]
  defstruct @enforce_keys

  @type terminal_outcome :: :completed | :accepted | :rejected | :failed | :blocked
  @type t :: %__MODULE__{
          state: :open | {:terminal, terminal_outcome()},
          required_operations: [String.t()],
          facts_revision: non_neg_integer()
        }
end

defmodule Pika.Agent.Role.Prepared do
  @moduledoc false

  @enforce_keys [
    :work,
    :role,
    :definition,
    :workspace,
    :profile,
    :cwd,
    :skill_roots,
    :instructions,
    :activation,
    :progress,
    :operation_receipts,
    :domain_options
  ]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          work: Pika.Agent.Role.Work.t(),
          role: module(),
          definition: Pika.Agent.Role.Definition.t(),
          workspace: map(),
          profile: map(),
          cwd: Path.t(),
          skill_roots: [Path.t()],
          instructions: Pika.Agent.Role.Instructions.t(),
          activation: :await_user_kickoff | {:start_turn, String.t()},
          progress: Pika.Agent.Role.Progress.t(),
          operation_receipts: module(),
          domain_options: map()
        }
end

defmodule Pika.Agent.Role.Invocation do
  @moduledoc false

  @enforce_keys [:operation, :arguments]
  defstruct [:operation, :arguments, :idempotency_key, :request_sha256, :session_id]
end

defmodule Pika.Agent.Role.Outcome do
  @moduledoc false

  @enforce_keys [:value, :progress, :actor_directive]
  defstruct @enforce_keys
end

defmodule Pika.Agent.Role.Error do
  @moduledoc false

  @enforce_keys [:code, :phase, :message]
  defstruct [:code, :phase, :message, :role_id, :work, retryable?: false, details: %{}]
end

defmodule Pika.Agent.Role.DomainAdapter do
  @moduledoc "Domain seam used by a Role without moving domain eligibility into the Role Runtime."

  alias Pika.Agent.Role.{DomainContext, Work}

  @callback prepare(Work.t(), map()) :: {:ok, DomainContext.t()} | {:error, term()}
  @callback invoke(Work.t(), String.t(), map(), map()) :: {:ok, term()} | {:error, term()}
  @callback replay(Work.t(), String.t(), map(), term(), map()) ::
              {:ok, term()} | {:error, term()}

  @callback session_event(Work.t(), atom(), map()) :: :ok | {:error, term()}

  @callback handle_exhaustion(Work.t(), term(), map()) :: {:ok, term()} | {:error, term()}

  @callback operation_receipts() :: module()

  @optional_callbacks replay: 5,
                      session_event: 3,
                      handle_exhaustion: 3,
                      operation_receipts: 0
end

defmodule Pika.Agent.Role do
  @moduledoc "Static contract implemented by every Pika Agent Role."

  alias Pika.Agent.Role.{Context, Definition}

  @callback definition() :: Definition.t()
  @callback build_system_instructions(Context.t()) :: {:ok, String.t()} | {:error, term()}
  @callback initial_prompt(Context.t()) :: {:ok, String.t()} | :none | {:error, term()}
  @callback recovery_prompt(Context.t()) :: {:ok, String.t()} | :none | {:error, term()}
end
