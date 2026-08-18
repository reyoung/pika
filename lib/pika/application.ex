defmodule Pika.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      Pika.MCP.ProbeState,
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor}
    ]

    Supervisor.start_link(children, strategy: :one_for_one, name: Pika.Supervisor)
  end
end
