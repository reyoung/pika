defmodule Pika.AgentBackend.Event do
  @moduledoc "Provider-neutral event emitted by every Agent Backend."

  @types [
    :session_started,
    :turn_started,
    :message_started,
    :message_delta,
    :message_completed,
    :plan_updated,
    :tool_started,
    :tool_updated,
    :tool_completed,
    :command_output,
    :file_changed,
    :usage_updated,
    :turn_completed,
    :backend_error,
    :process_exited
  ]

  @enforce_keys [:type, :backend, :session_id, :at]
  defstruct [:type, :backend, :session_id, :backend_session_id, :turn_id, :at, data: %{}]

  @type event_type :: unquote(Enum.reduce(@types, &{:|, [], [&1, &2]}))
  @type t :: %__MODULE__{
          type: event_type(),
          backend: atom(),
          session_id: String.t(),
          backend_session_id: String.t() | nil,
          turn_id: String.t() | nil,
          at: DateTime.t(),
          data: map()
        }

  def types, do: @types

  def new(type, backend, session_id, attrs \\ %{}) when type in @types do
    struct!(__MODULE__,
      type: type,
      backend: backend,
      session_id: session_id,
      backend_session_id: Map.get(attrs, :backend_session_id),
      turn_id: Map.get(attrs, :turn_id),
      at: DateTime.utc_now(),
      data: Map.get(attrs, :data, %{})
    )
  end
end
