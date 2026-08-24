defmodule Pika.Agent.Work do
  @moduledoc "Stable singleton-Optimization identity for one v2 Role Work."

  @enforce_keys [:role_id, :kind, :id]
  defstruct @enforce_keys ++ [payload: %{}]

  @type t :: %__MODULE__{
          role_id: String.t(),
          kind: atom(),
          id: String.t(),
          payload: map()
        }

  def key(%__MODULE__{} = work), do: {work.role_id, work.kind, work.id}
end
