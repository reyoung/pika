defmodule Pika.AgentBackend.Session do
  @moduledoc false
  @enforce_keys [:id, :backend, :backend_protocol, :backend_session_id, :cwd]
  defstruct [
    :id,
    :backend,
    :backend_protocol,
    :backend_session_id,
    :cwd,
    :model,
    :reasoning_effort,
    :jsonl_path
  ]

  @type t :: %__MODULE__{
          id: String.t(),
          backend: atom(),
          backend_protocol: String.t(),
          backend_session_id: String.t(),
          cwd: Path.t(),
          model: String.t() | nil,
          reasoning_effort: atom() | String.t() | nil,
          jsonl_path: Path.t()
        }
end
