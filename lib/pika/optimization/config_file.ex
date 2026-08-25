defmodule Pika.Optimization.ConfigFile do
  @moduledoc "Builds and renders the fully expanded v2 Workspace configuration."

  alias Pika.AgentBackend.PermissionPolicy
  alias Pika.Optimization.Config
  alias Pika.Optimization.Config.{Agent, Iteration, ProgressSummary, Role}

  @spec new(Path.t(), Path.t(), keyword()) :: Config.t()
  def new(source_path, repo, workspace, opts \\ []) do
    default_agent =
      agent(
        Keyword.get(opts, :backend, "codex"),
        Keyword.get(opts, :model),
        Keyword.get(opts, :reasoning_effort, "high")
      )

    configured = Keyword.get(opts, :agents, %{})
    iteration_agents = Map.get(configured, :iteration, duplicate(default_agent, opts))

    %Config{
      source_path: Path.expand(source_path),
      repo: Path.expand(repo),
      workspace: Path.expand(workspace),
      token: Keyword.get(opts, :token) || Pika.Auth.random_token(),
      baseline_alignment: %Role{
        agent: Map.get(configured, :baseline_alignment, default_agent)
      },
      baseline_verify: %Role{
        agent: Map.get(configured, :baseline_verify, default_agent),
        max_followups: 8
      },
      baseline_verify_followup: optional_role(configured, :baseline_verify_followup, :disabled),
      iteration: %Iteration{
        agents: iteration_agents,
        history_limit: 20,
        max_followups: 5,
        max_pending_attempts: 0
      },
      iteration_followup: optional_role(configured, :iteration_followup, :disabled),
      integration: %Role{
        agent: Map.get(configured, :integration, default_agent),
        max_followups: 8,
        regression_feedback_cases: 3
      },
      integration_followup: optional_role(configured, :integration_followup, :disabled),
      progress_summary: progress_summary(configured, opts, default_agent)
    }
  end

  @spec agent(atom() | String.t(), String.t() | nil, String.t() | nil) :: Agent.t()
  def agent(backend, model, reasoning_effort) do
    backend = normalize_backend(backend)
    permissions = PermissionPolicy.defaults(backend)

    %Agent{
      backend: backend,
      model: model,
      reasoning_effort: reasoning_effort,
      approval_policy: permissions.approval_policy,
      sandbox: permissions.sandbox_policy,
      env: %{},
      protocol_config: %{},
      options: %{}
    }
  end

  @spec render(Config.t()) :: String.t()
  def render(%Config{} = config) do
    [
      "version: 2\n",
      "repo: #{scalar(config.repo)}\n",
      "workspace: #{scalar(config.workspace)}\n\n",
      optional("token", config.token, ""),
      if(config.token, do: "\n", else: ""),
      "agents:\n",
      role("baseline_alignment", config.baseline_alignment),
      role("baseline_verify", config.baseline_verify,
        max_followups: config.baseline_verify.max_followups
      ),
      optional_role_yaml("baseline_verify_followup", config.baseline_verify_followup),
      iteration(config.iteration),
      optional_role_yaml("iteration_followup", config.iteration_followup),
      role("integration", config.integration,
        max_followups: config.integration.max_followups,
        regression_feedback_cases: config.integration.regression_feedback_cases
      ),
      optional_role_yaml("integration_followup", config.integration_followup),
      progress_summary_yaml(config.progress_summary)
    ]
    |> IO.iodata_to_binary()
  end

  defp duplicate(agent, opts) do
    count = Keyword.get(opts, :iteration_agents, 1)
    List.duplicate(agent, count)
  end

  defp optional_role(configured, key, default) do
    case Map.get(configured, key, default) do
      :disabled -> nil
      nil -> nil
      %Agent{} = configured_agent -> %Role{agent: configured_agent, generator_max_attempts: 3}
    end
  end

  defp progress_summary(configured, opts, default_agent) do
    case Map.get(configured, :progress_summary, :unspecified) do
      :unspecified ->
        if Keyword.get(opts, :progress_summary, false) do
          summary_agent = %{
            default_agent
            | reasoning_effort: "low"
          }

          summary(summary_agent)
        end

      nil ->
        nil

      %Agent{} = configured_agent ->
        summary(configured_agent)
    end
  end

  defp summary(agent) do
    %ProgressSummary{
      agent: agent,
      interval_ms: 5 * 60 * 1_000,
      time_zone: "Asia/Shanghai",
      max_followups: 3
    }
  end

  defp role(name, %Role{agent: agent}, controls \\ []) do
    [
      "\n  #{name}:\n",
      agent_yaml(agent, "    "),
      Enum.map(controls, fn {key, value} -> "    #{key}: #{value}\n" end)
    ]
  end

  defp optional_role_yaml(_name, nil), do: ""

  defp optional_role_yaml(name, %Role{} = configured_role) do
    role(name, configured_role, generator_max_attempts: configured_role.generator_max_attempts)
  end

  defp iteration(%Iteration{} = iteration) do
    [
      "\n  iteration:\n",
      "    history_limit: #{iteration.history_limit}\n",
      "    max_followups: #{iteration.max_followups}\n",
      "    max_pending_attempts: #{iteration.max_pending_attempts}\n",
      "    agents:\n",
      Enum.map(iteration.agents, fn agent ->
        ["      - backend: #{short_backend(agent.backend)}\n", agent_yaml_tail(agent, "        ")]
      end)
    ]
  end

  defp progress_summary_yaml(nil), do: ""

  defp progress_summary_yaml(%ProgressSummary{} = summary) do
    [
      "\n  progress_summary:\n",
      "    interval: #{duration(summary.interval_ms)}\n",
      "    timezone: #{scalar(summary.time_zone)}\n",
      agent_yaml(summary.agent, "    "),
      "    max_followups: #{summary.max_followups}\n"
    ]
  end

  defp agent_yaml(%Agent{} = configured_agent, indent) do
    [
      "#{indent}backend: #{short_backend(configured_agent.backend)}\n",
      agent_yaml_tail(configured_agent, indent)
    ]
  end

  defp agent_yaml_tail(%Agent{} = configured_agent, indent) do
    [
      optional("command", configured_agent.command, indent),
      optional("model", configured_agent.model, indent),
      optional("reasoning_effort", configured_agent.reasoning_effort, indent),
      "#{indent}approval_policy: #{scalar(configured_agent.approval_policy)}\n",
      "#{indent}sandbox: #{scalar(configured_agent.sandbox)}\n",
      optional_map("env", configured_agent.env, indent),
      optional_map("protocol_config", configured_agent.protocol_config, indent)
    ]
  end

  defp optional(_key, nil, _indent), do: ""
  defp optional(key, value, indent), do: "#{indent}#{key}: #{scalar(value)}\n"

  defp optional_map(_key, value, _indent) when value == %{}, do: ""
  defp optional_map(key, value, indent), do: "#{indent}#{key}: #{scalar(value)}\n"

  defp duration(milliseconds) when rem(milliseconds, 3_600_000) == 0,
    do: "#{div(milliseconds, 3_600_000)}h"

  defp duration(milliseconds) when rem(milliseconds, 60_000) == 0,
    do: "#{div(milliseconds, 60_000)}m"

  defp duration(milliseconds) when rem(milliseconds, 1_000) == 0,
    do: "#{div(milliseconds, 1_000)}s"

  defp duration(milliseconds), do: "#{milliseconds}ms"

  defp scalar(value), do: Jason.encode!(value)

  defp normalize_backend(value) when value in [:cursor_acp, "cursor_acp", "cursor"],
    do: :cursor_acp

  defp normalize_backend(_value), do: :codex_app_server

  defp short_backend(:cursor_acp), do: "cursor"
  defp short_backend(_backend), do: "codex"
end
