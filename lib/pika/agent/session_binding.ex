defmodule Pika.Agent.SessionBinding do
  @moduledoc "In-memory identity frozen into one v2 Actor Session token."

  @enforce_keys [
    :actor,
    :role_id,
    :work_kind,
    :work_id,
    :session_id,
    :context_file,
    :work_root
  ]
  defstruct @enforce_keys ++ [catalog: []]

  @type t :: %__MODULE__{
          actor: pid(),
          role_id: String.t(),
          work_kind: atom() | String.t(),
          work_id: String.t(),
          session_id: String.t(),
          context_file: Path.t(),
          work_root: Path.t(),
          catalog: [map()]
        }
end
