defmodule Pika.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    mode = Application.get_env(:pika, :runtime_mode, :preview)
    children = children(mode)
    strategy = if mode == :serve, do: :rest_for_one, else: :one_for_one

    Supervisor.start_link(children, strategy: strategy, name: Pika.Supervisor)
  end

  defp children(:serve) do
    plan = Application.fetch_env!(:pika, :workspace_plan)

    [
      {Pika.WorkspaceLock, plan},
      Pika.Repo,
      {Phoenix.PubSub, name: Pika.PubSub},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.CampaignSupervisor},
      Pika.Runtime,
      Pika.CampaignBootstrap,
      Pika.AttemptCoordinator,
      Pika.IntegrationCoordinator,
      Pika.SyncCoordinator,
      PikaWeb.Endpoint
    ]
  end

  defp children(_mode) do
    [
      Pika.MCP.ProbeState,
      {Phoenix.PubSub, name: Pika.PubSub},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.CampaignSupervisor},
      PikaWeb.Endpoint
    ]
  end
end
