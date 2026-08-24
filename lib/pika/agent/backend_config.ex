defmodule Pika.Agent.BackendConfig do
  @moduledoc "Selects one fully expanded v2 Backend config and converts it to the shared adapter profile."

  alias Pika.Agent.Work
  alias Pika.AgentBackend.LaunchConfig
  alias Pika.Optimization.Config

  @spec select(Config.t(), Work.t()) :: {:ok, Config.Agent.t()} | {:error, term()}
  def select(%Config{} = config, %Work{role_id: "iteration"} = work) do
    slot = get_in(work.payload, [:attempt, :slot_index]) || attempt_slot(work.id)

    case Enum.at(config.iteration.agents, slot) do
      %Config.Agent{} = agent -> {:ok, agent}
      nil -> {:error, {:iteration_backend_slot_missing, slot}}
    end
  end

  def select(%Config{} = config, %Work{role_id: role_id}) do
    Config.role_agent(config, role_id)
  end

  @spec module(Config.Agent.t(), map()) :: {:ok, module()} | {:error, term()}
  def module(%Config.Agent{backend: backend}, overrides \\ %{}) do
    module =
      overrides[backend] ||
        case backend do
          :codex_app_server -> Pika.AgentBackend.CodexAppServer
          :cursor_acp -> Pika.AgentBackend.CursorACP
        end

    if is_atom(module), do: {:ok, module}, else: {:error, {:unsupported_agent_backend, backend}}
  end

  @spec launch_config(Config.Agent.t(), Path.t()) :: LaunchConfig.t()
  def launch_config(%Config.Agent{} = agent, artifact_dir) do
    {command, args} = command(agent.backend, agent.command)

    %LaunchConfig{
      backend: agent.backend,
      command: command,
      args: args,
      approval_policy: agent.approval_policy,
      sandbox_policy: agent.sandbox,
      env: agent.env,
      protocol_config: agent.protocol_config,
      artifact_dir: artifact_dir
    }
  end

  defp command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp command(:codex_app_server, nil), do: {"codex", []}
  defp command(_backend, [command | args]), do: {command, args}
  defp command(_backend, command) when is_binary(command), do: {command, []}

  defp attempt_slot(work_id) do
    case Integer.parse(work_id) do
      {attempt_id, ""} ->
        case Pika.Repo.query!("SELECT slot_index FROM attempts WHERE id = ?", [attempt_id]).rows do
          [[slot]] -> slot
          [] -> nil
        end

      _other ->
        nil
    end
  end
end
