defmodule Pika.Optimization.InteractiveConfig do
  @moduledoc "Interactive collection of v2 per-Agent Backend configuration."

  alias Pika.AgentBackend.PermissionPolicy
  alias Pika.ModelCatalog
  alias Pika.Optimization.Config
  alias Pika.Optimization.Config.{Agent, ProgressSummary, Role}
  alias Pika.Optimization.ConfigFile

  @efforts ~w(low medium high xhigh max ultra)
  @roles ~w(
    baseline_alignment baseline_verify baseline_verify_followup iteration iteration_followup
    integration integration_followup progress_summary
  )

  @spec collect_init(map() | keyword()) ::
          {:ok, %{workspace: Path.t(), repo: Path.t(), agents: map()}} | {:error, term()}
  def collect_init(opts) do
    opts = normalize_opts(opts)
    interactive? = opts[:yes] != true
    cwd = System.get_env("PIKA_CLI_CWD") || File.cwd!()
    backend = opts[:backend] || "codex"
    model = opts[:model]
    effort = opts[:reasoning_effort] || "high"
    default_agent = ConfigFile.agent(backend, model, effort)
    followup_agent = %{default_agent | reasoning_effort: "medium"}
    summary_agent = reader_agent(%{default_agent | reasoning_effort: "low"})
    random_token = Pika.Auth.random_token()

    IO.puts("Pika v2 Workspace initializer")

    with {:ok, repo} <- path(opts[:repo], "Repository path", cwd, interactive?),
         workspace_default <- Path.join(cwd, "pika-workspace"),
         {:ok, workspace} <-
           path(opts[:workspace], "Workspace path", workspace_default, interactive?),
         {:ok, token} <- token(opts[:token], random_token, interactive?),
         {:ok, baseline_alignment, cache} <-
           collect_agent("Baseline Alignment Agent", default_agent, opts, %{}, interactive?),
         {:ok, baseline_verify, cache} <-
           collect_agent("Baseline Verify Agent", default_agent, opts, cache, interactive?),
         {:ok, baseline_verify_followup, cache} <-
           collect_optional_agent(
             "Baseline Verify Follow-up Agent",
             :baseline_verify_followup,
             followup_agent,
             opts,
             cache,
             interactive?
           ),
         {:ok, iteration_count} <-
           positive_integer(
             opts[:iteration_agents],
             "Concurrent Iteration Agents",
             1,
             interactive?
           ),
         {:ok, iteration_agents, cache} <-
           collect_agents(
             "Iteration Agent",
             iteration_count,
             List.duplicate(default_agent, iteration_count),
             opts,
             cache,
             interactive?
           ),
         {:ok, iteration_followup, cache} <-
           collect_optional_agent(
             "Iteration Follow-up Agent",
             :iteration_followup,
             followup_agent,
             opts,
             cache,
             interactive?
           ),
         {:ok, integration, cache} <-
           collect_agent("Integration Agent", default_agent, opts, cache, interactive?),
         {:ok, integration_followup, cache} <-
           collect_optional_agent(
             "Integration Follow-up Agent",
             :integration_followup,
             followup_agent,
             opts,
             cache,
             interactive?
           ),
         {:ok, progress_summary, _cache} <-
           collect_optional_agent(
             "Progress Summary Agent",
             :progress_summary,
             summary_agent,
             opts,
             cache,
             interactive?
           ) do
      {:ok,
       %{
         workspace: workspace,
         repo: repo,
         token: token,
         agents: %{
           baseline_alignment: baseline_alignment,
           baseline_verify: baseline_verify,
           baseline_verify_followup: baseline_verify_followup,
           iteration: iteration_agents,
           iteration_followup: iteration_followup,
           integration: integration,
           integration_followup: integration_followup,
           progress_summary: progress_summary
         }
       }}
    end
  end

  @spec choose_role() :: String.t() | :all
  def choose_role do
    IO.puts("\nConfiguration to update:")

    Enum.with_index(@roles, 1)
    |> Enum.each(fn {role, index} -> IO.puts("  #{index}) #{role_label(role)}") end)

    all_index = length(@roles) + 1
    IO.puts("  #{all_index}) All Agent configurations")

    case prompt("Select configuration", Integer.to_string(all_index)) do
      value ->
        case Integer.parse(value) do
          {^all_index, ""} ->
            :all

          {index, ""} when index >= 1 and index <= length(@roles) ->
            Enum.at(@roles, index - 1)

          _other ->
            IO.puts(:stderr, "Invalid value: choose a listed number")
            choose_role()
        end
    end
  end

  @spec validate_role(String.t() | atom()) :: {:ok, String.t() | :all} | {:error, term()}
  def validate_role(role) when role in [:all, "all"], do: {:ok, :all}

  def validate_role(role) do
    normalized = to_string(role) |> String.trim() |> String.replace("-", "_")

    if normalized in @roles,
      do: {:ok, normalized},
      else: {:error, {:unknown_configuration_role, to_string(role)}}
  end

  @spec reconfigure(Config.t(), String.t() | :all, map() | keyword()) ::
          {:ok, Config.t()} | {:error, term()}
  def reconfigure(%Config{} = config, role, opts) do
    opts = normalize_opts(opts)
    interactive? = opts[:yes] != true
    roles = if role == :all, do: @roles, else: [role]

    Enum.reduce_while(roles, {:ok, config, %{}}, fn role_id, {:ok, current, cache} ->
      case reconfigure_role(current, role_id, opts, cache, interactive?) do
        {:ok, updated, next_cache} -> {:cont, {:ok, updated, next_cache}}
        {:error, _reason} = error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, updated, _cache} -> {:ok, updated}
      {:error, _reason} = error -> error
    end
  end

  defp reconfigure_role(config, "baseline_alignment", opts, cache, interactive?) do
    with {:ok, agent, cache} <-
           collect_agent(
             "Baseline Alignment Agent",
             config.baseline_alignment.agent,
             opts,
             cache,
             interactive?
           ) do
      {:ok, %{config | baseline_alignment: %{config.baseline_alignment | agent: agent}}, cache}
    end
  end

  defp reconfigure_role(config, "baseline_verify", opts, cache, interactive?) do
    with {:ok, agent, cache} <-
           collect_agent(
             "Baseline Verify Agent",
             config.baseline_verify.agent,
             opts,
             cache,
             interactive?
           ) do
      {:ok, %{config | baseline_verify: %{config.baseline_verify | agent: agent}}, cache}
    end
  end

  defp reconfigure_role(config, "baseline_verify_followup", opts, cache, interactive?) do
    fallback = followup_default(config.baseline_verify.agent)

    with {:ok, agent, cache} <-
           collect_optional_agent(
             "Baseline Verify Follow-up Agent",
             :enabled,
             optional_agent(config.baseline_verify_followup) || fallback,
             opts,
             cache,
             interactive?,
             enabled?: not is_nil(config.baseline_verify_followup)
           ) do
      role = optional_role(agent, config.baseline_verify_followup)
      {:ok, %{config | baseline_verify_followup: role}, cache}
    end
  end

  defp reconfigure_role(config, "iteration", opts, cache, interactive?) do
    current_agents = config.iteration.agents

    with {:ok, count} <-
           positive_integer(
             opts[:iteration_agents],
             "Concurrent Iteration Agents",
             length(current_agents),
             interactive?
           ),
         defaults <- resize_agents(current_agents, count),
         {:ok, agents, cache} <-
           collect_agents("Iteration Agent", count, defaults, opts, cache, interactive?) do
      iteration = %{config.iteration | agents: agents}
      {:ok, %{config | iteration: iteration}, cache}
    end
  end

  defp reconfigure_role(config, "iteration_followup", opts, cache, interactive?) do
    fallback = followup_default(List.first(config.iteration.agents))

    with {:ok, agent, cache} <-
           collect_optional_agent(
             "Iteration Follow-up Agent",
             :enabled,
             optional_agent(config.iteration_followup) || fallback,
             opts,
             cache,
             interactive?,
             enabled?: not is_nil(config.iteration_followup)
           ) do
      role = optional_role(agent, config.iteration_followup)
      {:ok, %{config | iteration_followup: role}, cache}
    end
  end

  defp reconfigure_role(config, "integration", opts, cache, interactive?) do
    with {:ok, agent, cache} <-
           collect_agent(
             "Integration Agent",
             config.integration.agent,
             opts,
             cache,
             interactive?
           ) do
      {:ok, %{config | integration: %{config.integration | agent: agent}}, cache}
    end
  end

  defp reconfigure_role(config, "integration_followup", opts, cache, interactive?) do
    fallback = followup_default(config.integration.agent)

    with {:ok, agent, cache} <-
           collect_optional_agent(
             "Integration Follow-up Agent",
             :enabled,
             optional_agent(config.integration_followup) || fallback,
             opts,
             cache,
             interactive?,
             enabled?: not is_nil(config.integration_followup)
           ) do
      role = optional_role(agent, config.integration_followup)
      {:ok, %{config | integration_followup: role}, cache}
    end
  end

  defp reconfigure_role(config, "progress_summary", opts, cache, interactive?) do
    current = config.progress_summary
    fallback = reader_agent(config.baseline_alignment.agent)

    with {:ok, agent, cache} <-
           collect_optional_agent(
             "Progress Summary Agent",
             :enabled,
             optional_agent(current) || fallback,
             opts,
             cache,
             interactive?,
             enabled?: not is_nil(current)
           ) do
      summary = progress_summary(agent, current)
      {:ok, %{config | progress_summary: summary}, cache}
    end
  end

  defp collect_agents(label, count, defaults, opts, cache, interactive?) do
    1..count
    |> Enum.reduce_while({:ok, [], cache}, fn index, {:ok, agents, current_cache} ->
      default = Enum.at(defaults, index - 1) || List.last(defaults)

      case collect_agent(
             "#{label} #{index}",
             default,
             opts,
             current_cache,
             interactive?
           ) do
        {:ok, agent, next_cache} -> {:cont, {:ok, [agent | agents], next_cache}}
        {:error, _reason} = error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, agents, cache} -> {:ok, Enum.reverse(agents), cache}
      {:error, _reason} = error -> error
    end
  end

  defp collect_optional_agent(label, key, default, opts, cache, interactive?, settings \\ []) do
    enabled_default = Keyword.get(settings, :enabled?, false)

    with {:ok, enabled?} <- enabled(opts, key, label, enabled_default, interactive?) do
      if enabled? do
        collect_agent(label, default, opts, cache, interactive?)
      else
        {:ok, nil, cache}
      end
    end
  end

  defp collect_agent(label, %Agent{} = current, opts, cache, interactive?) do
    with {:ok, backend} <- backend(opts[:backend], label, current.backend, interactive?),
         current_model <- if(backend == current.backend, do: current.model, else: nil),
         {:ok, model, cache} <-
           model(opts, label, backend, current_model, cache, interactive?),
         {:ok, effort} <-
           effort(opts[:reasoning_effort], label, current.reasoning_effort, interactive?) do
      {:ok, update_agent(current, backend, model, effort), cache}
    end
  end

  defp backend(nil, _label, current, false), do: {:ok, normalize_backend(current)}

  defp backend(value, _label, _current, _interactive?) when not is_nil(value),
    do: parse_backend(value)

  defp backend(nil, label, current, true) do
    current = short_backend(current)
    default = if current == "cursor", do: "2", else: "1"
    IO.puts("\n#{label} backend:")
    IO.puts("  1) Codex · Codex App Server")
    IO.puts("  2) Cursor · Agent Client Protocol")

    case prompt("Select #{label} backend", default) |> parse_backend() do
      {:ok, backend} ->
        {:ok, backend}

      {:error, _reason} ->
        IO.puts(:stderr, "Invalid value: choose 1 for Codex or 2 for Cursor")
        backend(nil, label, current, true)
    end
  end

  defp model(opts, label, backend, current, cache, interactive?) do
    model_value = opts[:model]

    cond do
      not is_nil(model_value) -> selected_model(model_value, cache)
      not interactive? -> {:ok, current, cache}
      true -> choose_model(opts, label, backend, current, cache)
    end
  end

  defp selected_model(model, cache) do
    case String.trim(model) do
      value when value in ["", "default"] -> {:ok, nil, cache}
      value -> {:ok, value, cache}
    end
  end

  defp choose_model(opts, label, backend, current, cache) do
    with {:ok, models, cache} <- load_models(opts, backend, cache),
         {:ok, model} <- ask_model(label, backend, models, current) do
      {:ok, model, cache}
    end
  end

  defp load_models(opts, backend, cache) do
    case Map.fetch(cache, backend) do
      {:ok, models} ->
        {:ok, models, cache}

      :error ->
        catalog = opts[:model_catalog] || (&ModelCatalog.list/1)
        IO.puts("Loading available #{String.capitalize(short_backend(backend))} models…")

        models =
          case catalog.(Atom.to_string(backend)) do
            {:ok, models} when is_list(models) ->
              models

            {:error, reason} ->
              IO.puts(:stderr, "Could not load provider model list: #{inspect(reason)}")
              []
          end

        {:ok, models, Map.put(cache, backend, models)}
    end
  end

  defp ask_model(label, backend, models, current) do
    IO.puts("\nAvailable #{label} models (#{String.capitalize(short_backend(backend))}):")
    IO.puts("  1) Provider default")

    Enum.with_index(models, 2)
    |> Enum.each(fn {model, index} ->
      id = model_value(model, :id)
      name = model_value(model, :label) || id
      description = compact_description(model_value(model, :description))
      suffix = if description, do: " — #{description}", else: ""
      IO.puts("  #{index}) #{id} · #{name}#{suffix}")
    end)

    custom_index = length(models) + 2
    IO.puts("  #{custom_index}) Custom model id")
    default = model_default(models, current, custom_index)

    case prompt("Select #{label} model", Integer.to_string(default)) |> Integer.parse() do
      {1, ""} ->
        {:ok, nil}

      {index, ""} when index >= 2 and index < custom_index ->
        {:ok, model_value(Enum.at(models, index - 2), :id)}

      {^custom_index, ""} ->
        ask_non_empty("Custom model id", current || "")

      _other ->
        IO.puts(:stderr, "Invalid value: choose a listed number")
        ask_model(label, backend, models, current)
    end
  end

  defp effort(nil, _label, current, false), do: parse_effort(current || "high")

  defp effort(value, _label, _current, _interactive?) when not is_nil(value),
    do: parse_effort(value)

  defp effort(nil, label, current, true) do
    current = if current in @efforts, do: current, else: "high"
    default = Enum.find_index(@efforts, &(&1 == current)) + 1
    IO.puts("\n#{label} reasoning effort:")

    Enum.with_index(@efforts, 1)
    |> Enum.each(fn {value, index} ->
      IO.puts("  #{index}) #{value} · #{effort_description(value)}")
    end)

    case prompt("Select #{label} reasoning effort", Integer.to_string(default))
         |> Integer.parse() do
      {index, ""} when index >= 1 and index <= length(@efforts) ->
        {:ok, Enum.at(@efforts, index - 1)}

      _other ->
        IO.puts(:stderr, "Invalid value: choose a listed number")
        effort(nil, label, current, true)
    end
  end

  defp enabled(opts, key, label, default, interactive?) do
    value = opts[key]

    cond do
      is_boolean(value) -> {:ok, value}
      not interactive? -> {:ok, default}
      true -> ask_boolean("Enable #{label}?", default)
    end
  end

  defp positive_integer(value, _label, _default, _interactive?)
       when is_integer(value) and value > 0,
       do: {:ok, value}

  defp positive_integer(value, _label, _default, false) when not is_nil(value),
    do: {:error, {:invalid_positive_integer, value}}

  defp positive_integer(nil, _label, default, false), do: {:ok, default}

  defp positive_integer(value, label, default, true) do
    selected =
      if is_nil(value), do: prompt(label, Integer.to_string(default)), else: to_string(value)

    case Integer.parse(selected) do
      {integer, ""} when integer > 0 ->
        {:ok, integer}

      _other ->
        IO.puts(:stderr, "Invalid value: enter a positive integer")
        positive_integer(nil, label, default, true)
    end
  end

  defp path(value, _label, _default, _interactive?) when is_binary(value) and value != "",
    do: {:ok, Path.expand(value, System.get_env("PIKA_CLI_CWD") || File.cwd!())}

  defp path(nil, label, default, true) do
    cwd = System.get_env("PIKA_CLI_CWD") || File.cwd!()
    {:ok, prompt(label, default) |> Path.expand(cwd)}
  end

  defp path(nil, label, _default, false), do: {:error, {:missing_required_path, label}}

  defp token(value, _default, _interactive?) when is_binary(value) do
    if String.trim(value) == "", do: {:error, :empty_access_token}, else: {:ok, value}
  end

  defp token(nil, default, false), do: {:ok, default}

  defp token(nil, default, true) do
    case prompt("Access token", default) do
      "" -> {:error, :empty_access_token}
      value -> {:ok, value}
    end
  end

  defp update_agent(current, backend, model, effort) do
    if current.backend == backend do
      %{current | model: model, reasoning_effort: effort}
    else
      permissions = PermissionPolicy.defaults(backend)

      %{
        current
        | backend: backend,
          model: model,
          reasoning_effort: effort,
          approval_policy: permissions.approval_policy,
          sandbox: permissions.sandbox_policy
      }
    end
  end

  defp optional_agent(%Role{agent: agent}), do: agent
  defp optional_agent(%ProgressSummary{agent: agent}), do: agent
  defp optional_agent(nil), do: nil

  defp optional_role(nil, _current), do: nil

  defp optional_role(agent, %Role{} = current), do: %{current | agent: agent}
  defp optional_role(agent, nil), do: %Role{agent: agent, generator_max_attempts: 3}

  defp progress_summary(nil, _current), do: nil

  defp progress_summary(agent, %ProgressSummary{} = current), do: %{current | agent: agent}

  defp progress_summary(agent, nil) do
    %ProgressSummary{
      agent: agent,
      interval_ms: 5 * 60 * 1_000,
      time_zone: "Asia/Shanghai",
      max_followups: 3
    }
  end

  defp followup_default(agent), do: %{agent | reasoning_effort: "medium"}

  defp reader_agent(%Agent{} = agent) do
    %{agent | reasoning_effort: "low"}
  end

  defp resize_agents(agents, count) do
    fallback = List.last(agents)
    Enum.map(0..(count - 1), &(Enum.at(agents, &1) || fallback))
  end

  defp model_default(_models, nil, _custom_index), do: 1

  defp model_default(models, current, custom_index) do
    case Enum.find_index(models, &(model_value(&1, :id) == current)) do
      nil -> custom_index
      index -> index + 2
    end
  end

  defp model_value(model, key) when is_map(model),
    do: Map.get(model, key) || Map.get(model, Atom.to_string(key))

  defp compact_description(nil), do: nil

  defp compact_description(description) when is_binary(description) do
    description
    |> String.replace(~r/\s+/, " ")
    |> String.trim()
    |> String.slice(0, 120)
    |> case do
      "" -> nil
      value -> value
    end
  end

  defp compact_description(_description), do: nil

  defp parse_backend(value)
       when value in [1, "1", :codex, :codex_app_server, "codex", "codex_app_server"],
       do: {:ok, :codex_app_server}

  defp parse_backend(value) when value in [2, "2", :cursor, :cursor_acp, "cursor", "cursor_acp"],
    do: {:ok, :cursor_acp}

  defp parse_backend(value), do: {:error, {:invalid_backend, value}}

  defp normalize_backend(value) do
    case parse_backend(value) do
      {:ok, backend} -> backend
      {:error, _reason} -> :codex_app_server
    end
  end

  defp parse_effort(value) when is_atom(value), do: parse_effort(Atom.to_string(value))

  defp parse_effort(value) when is_binary(value) do
    normalized = value |> String.trim() |> String.downcase()

    if normalized in @efforts,
      do: {:ok, normalized},
      else: {:error, {:invalid_reasoning_effort, value}}
  end

  defp parse_effort(value), do: {:error, {:invalid_reasoning_effort, value}}

  defp ask_boolean(label, default) do
    default_label = if default, do: "Y/n", else: "y/N"

    case IO.gets("#{label} [#{default_label}]: ") do
      nil ->
        {:ok, default}

      :eof ->
        {:ok, default}

      input ->
        case input |> String.trim() |> String.downcase() do
          "" ->
            {:ok, default}

          value when value in ["y", "yes"] ->
            {:ok, true}

          value when value in ["n", "no"] ->
            {:ok, false}

          _other ->
            IO.puts(:stderr, "Invalid value: enter yes or no")
            ask_boolean(label, default)
        end
    end
  end

  defp ask_non_empty(label, default) do
    value = prompt(label, default)

    if value == "" do
      IO.puts(:stderr, "Invalid value: enter a non-empty model id")
      ask_non_empty(label, default)
    else
      {:ok, value}
    end
  end

  defp prompt(label, default) do
    suffix = if default in [nil, ""], do: "", else: " [#{default}]"

    case IO.gets("#{label}#{suffix}: ") do
      nil ->
        default || ""

      :eof ->
        default || ""

      input ->
        case String.trim(input) do
          "" -> default || ""
          value -> value
        end
    end
  end

  defp role_label("baseline_alignment"), do: "Baseline Alignment Agent"
  defp role_label("baseline_verify"), do: "Baseline Verify Agent"
  defp role_label("baseline_verify_followup"), do: "Baseline Verify Follow-up Agent"
  defp role_label("iteration"), do: "Iteration Agents"
  defp role_label("iteration_followup"), do: "Iteration Follow-up Agent"
  defp role_label("integration"), do: "Integration Agent"
  defp role_label("integration_followup"), do: "Integration Follow-up Agent"
  defp role_label("progress_summary"), do: "Progress Summary Agent"

  defp effort_description("low"), do: "fastest, minimal reasoning"
  defp effort_description("medium"), do: "balanced speed and reasoning"
  defp effort_description("high"), do: "thorough reasoning"
  defp effort_description("xhigh"), do: "more reasoning for difficult tasks"
  defp effort_description("max"), do: "maximum supported reasoning"
  defp effort_description("ultra"), do: "deepest supported reasoning"

  defp short_backend(value) when value in [:cursor_acp, "cursor_acp", "cursor"], do: "cursor"
  defp short_backend(_value), do: "codex"

  defp normalize_opts(opts) when is_map(opts), do: Map.to_list(opts)
  defp normalize_opts(opts) when is_list(opts), do: opts
end
