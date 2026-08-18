defmodule Pika.AgentBackend.Profile do
  @moduledoc false
  @enforce_keys [:backend]
  defstruct [
    :backend,
    :command,
    args: [],
    env: %{},
    protocol_config: %{},
    artifact_dir: "artifacts/phase-0"
  ]

  @type t :: %__MODULE__{
          backend: atom(),
          command: String.t() | nil,
          args: [String.t()],
          env: map(),
          protocol_config: map(),
          artifact_dir: Path.t()
        }

  def normalize(%__MODULE__{} = profile), do: profile
  def normalize(profile) when is_map(profile), do: struct!(__MODULE__, profile)
end
