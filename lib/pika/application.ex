defmodule Pika.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      Pika.MCP.ProbeState,
      {Phoenix.PubSub, name: Pika.PubSub},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.Stage0.CampaignSupervisor},
      PikaWeb.Endpoint
    ]

    Supervisor.start_link(children, strategy: :one_for_one, name: Pika.Supervisor)
  end
end
