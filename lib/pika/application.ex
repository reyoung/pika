defmodule Pika.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    mode = Application.get_env(:pika, :runtime_mode, :test)
    :ok = Pika.Agent.RolePromptRegistry.validate!()
    :ok = Pika.Optimization.RoleRegistry.validate!()
    :ok = Pika.Agent.ToolCatalog.validate()
    children = children(mode)
    strategy = if mode == :v2, do: :rest_for_one, else: :one_for_one

    Supervisor.start_link(children, strategy: strategy, name: Pika.Supervisor)
  end

  defp children(:v2) do
    config_path = Application.fetch_env!(:pika, :v2_config_path)

    workspace =
      config_path
      |> Pika.Optimization.Config.load()
      |> then(fn {:ok, value} -> value.workspace end)

    [
      {Pika.WorkspaceLock, workspace: workspace},
      Pika.Repo,
      {Phoenix.PubSub, name: Pika.PubSub},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor},
      Pika.Agent.Directory,
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.Agent.ActorSupervisor},
      {Pika.Optimization.Bootstrap, config_path: config_path},
      Pika.Baseline.Questions,
      Pika.Optimization.Runtime,
      {Pika.Agent.Symphony,
       workspace: workspace, actor_opts: [question_handler: &Pika.Baseline.Questions.ask/2]},
      PikaWeb.Endpoint
    ]
  end

  defp children(_mode) do
    [
      Pika.MCP.ProbeState,
      {Phoenix.PubSub, name: Pika.PubSub},
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.AgentBackendSessionSupervisor},
      Pika.Agent.Directory,
      {DynamicSupervisor, strategy: :one_for_one, name: Pika.Agent.ActorSupervisor},
      PikaWeb.Endpoint
    ]
  end
end
