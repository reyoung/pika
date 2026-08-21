defmodule Pika.Config do
  @moduledoc false

  alias Pika.AgentBackend.PermissionPolicy
  alias Pika.Paths

  @root_fields ~w(server backend campaign prompts sync)
  @server_fields ~w(host port)
  @backend_fields ~w(type command model reasoning_effort approval_policy sandbox_policy protocol_config)
  @campaign_fields ~w(plan max_attempts history_n iteration_agents integration_agent reference_catalog stop_conditions)
  @prompt_fields ~w(alignment setup_merge baseline plan iteration integration sync)
  @sync_fields ~w(remote branch)
  @agent_profile_fields ~w(name backend command model reasoning_effort approval_policy sandbox_policy env protocol_config)
  @backend_types ~w(codex_app_server cursor_acp)

  defstruct [
    :source_path,
    :workspace,
    :repo,
    :host,
    :port,
    :backend,
    :campaign,
    :prompts,
    :sync,
    :snapshot,
    :existing_snapshot
  ]

  def load(path, opts) when is_binary(path) and is_list(opts) do
    with {:ok, workspace} <- required_workspace(opts),
         {:ok, yaml} <- read_yaml(path),
         {:ok, existing} <- read_existing(workspace),
         {:ok, config} <- validate(path, workspace, yaml, existing, opts),
         :ok <- compare_immutable(config) do
      {:ok, config}
    end
  end

  defp required_workspace(opts) do
    case Keyword.get(opts, :workspace) do
      value when is_binary(value) and value != "" -> Paths.canonical(value)
      _ -> {:error, {:invalid_config, ["workspace: --workspace is required"]}}
    end
  end

  defp read_yaml(path) do
    with {:ok, source_path} <- canonical_file(path),
         {:ok, value} <- YamlElixir.read_from_file(source_path) do
      cond do
        is_nil(value) -> {:ok, %{}}
        is_map(value) -> {:ok, stringify_keys(value)}
        true -> {:error, {:invalid_config, ["config: YAML document must be a mapping"]}}
      end
    else
      {:error, %YamlElixir.ParsingError{} = error} ->
        {:error, {:invalid_config, ["config: #{Exception.message(error)}"]}}

      {:error, :enoent} ->
        {:error, {:invalid_config, ["config: file does not exist: #{path}"]}}

      {:error, reason} ->
        {:error, {:invalid_config, ["config: #{inspect(reason)}"]}}
    end
  end

  defp canonical_file(path) do
    expanded = Path.expand(path)

    if File.regular?(expanded) do
      with {:ok, parent} <- Paths.canonical(Path.dirname(expanded)) do
        {:ok, Path.join(parent, Path.basename(expanded))}
      end
    else
      {:error, :enoent}
    end
  end

  defp read_existing(workspace) do
    path = Path.join(workspace, "config.json")

    cond do
      not File.exists?(path) ->
        {:ok, nil}

      not File.regular?(path) ->
        {:error, {:invalid_workspace, "config.json is not a regular file"}}

      true ->
        case File.read(path) do
          {:ok, contents} ->
            case Jason.decode(contents) do
              {:ok, %{"schema_version" => 1} = value} ->
                {:ok, value}

              {:ok, _} ->
                {:error, {:invalid_workspace, "unsupported config.json schema"}}

              {:error, error} ->
                {:error, {:invalid_workspace, "invalid config.json: #{inspect(error)}"}}
            end

          {:error, reason} ->
            {:error, {:invalid_workspace, "cannot read config.json: #{inspect(reason)}"}}
        end
    end
  end

  defp validate(path, workspace, yaml, existing, opts) do
    errors =
      unknown_fields(yaml, @root_fields, "config") ++
        unknown_fields(map(yaml["server"]), @server_fields, "server") ++
        unknown_fields(map(yaml["backend"]), @backend_fields, "backend") ++
        unknown_fields(map(yaml["campaign"]), @campaign_fields, "campaign") ++
        unknown_fields(map(yaml["prompts"]), @prompt_fields, "prompts") ++
        unknown_fields(map(yaml["sync"]), @sync_fields, "sync") ++
        section_errors(yaml)

    server = map(yaml["server"])
    backend = map(yaml["backend"])
    campaign = map(yaml["campaign"])
    prompts = map(yaml["prompts"])
    sync = map(yaml["sync"])

    host = Keyword.get(opts, :host) || server["host"] || "127.0.0.1"
    port = Keyword.get(opts, :port) || server["port"] || 8080
    type = backend["type"] || "codex_app_server"
    command = backend["command"] || default_command(type)
    model = backend["model"]
    reasoning_effort = backend["reasoning_effort"]
    permission_defaults = PermissionPolicy.defaults(type)

    approval_policy =
      normalize_permission(
        type,
        :approval_policy,
        backend["approval_policy"] || permission_defaults.approval_policy
      )

    sandbox_policy =
      normalize_permission(
        type,
        :sandbox_policy,
        backend["sandbox_policy"] || permission_defaults.sandbox_policy
      )

    protocol_config = backend["protocol_config"] || %{}
    plan = Map.get(campaign, "plan", false)
    max_attempts = Map.get(campaign, "max_attempts")
    history_n = Map.get(campaign, "history_n", 10)
    iteration_agents = Map.get(campaign, "iteration_agents", default_iteration_agents(type))
    integration_agent = Map.get(campaign, "integration_agent", default_integration_agent(type))
    reference_catalog = Map.get(campaign, "reference_catalog", [])
    stop_conditions = Map.get(campaign, "stop_conditions", %{})

    errors =
      errors ++
        validate_host(host) ++
        validate_port(port) ++
        validate_backend(
          type,
          command,
          model,
          reasoning_effort,
          approval_policy,
          sandbox_policy,
          protocol_config
        ) ++
        validate_campaign(
          plan,
          max_attempts,
          history_n,
          iteration_agents,
          integration_agent,
          reference_catalog,
          stop_conditions,
          type
        ) ++
        validate_prompts(prompts) ++
        validate_sync(sync)

    with [] <- errors,
         {:ok, repo} <- resolve_repo(Keyword.get(opts, :repo), existing),
         {:ok, source_path} <- canonical_file(path) do
      prompt_paths = resolve_prompt_paths(prompts, source_path)

      backend_config =
        %{
          "type" => type,
          "command" => normalize_command(command),
          "approval_policy" => approval_policy,
          "sandbox_policy" => sandbox_policy,
          "protocol_config" => protocol_config
        }
        |> put_optional("model", model)
        |> put_optional("reasoning_effort", reasoning_effort)

      immutable = %{
        "workspace" => workspace,
        "repo_mode" => if(repo, do: "managed_repo", else: "owned_repo"),
        "managed_repo" => if(repo, do: %{"canonical_path" => repo}, else: nil),
        "listen" => %{"host" => host, "port" => port},
        "backend" => backend_config,
        "prompts" => prompt_paths
      }

      mutable = %{
        "plan" => plan,
        "max_attempts" => max_attempts,
        "history_n" => history_n,
        "iteration_agents" => normalize_iteration_agents(iteration_agents, type, command),
        "integration_agent" => normalize_integration_agent(integration_agent, type, command),
        "reference_catalog" => reference_catalog,
        "stop_conditions" => stop_conditions,
        "sync" => %{"remote" => sync["remote"], "branch" => sync["branch"]}
      }

      {:ok,
       %__MODULE__{
         source_path: source_path,
         workspace: workspace,
         repo: repo,
         host: host,
         port: port,
         backend: immutable["backend"],
         campaign: mutable,
         prompts: prompt_paths,
         sync: mutable["sync"],
         snapshot: %{
           "schema_version" => 1,
           "immutable" => immutable,
           "mutable" => mutable
         },
         existing_snapshot: existing
       }}
    else
      [_ | _] = errors -> {:error, {:invalid_config, errors}}
      {:error, _} = error -> error
    end
  end

  defp resolve_repo(repo, _existing) when is_binary(repo) and repo != "",
    do: Paths.canonical(repo)

  defp resolve_repo(nil, %{
         "immutable" => %{
           "repo_mode" => "managed_repo",
           "managed_repo" => %{"canonical_path" => path}
         }
       }),
       do: {:ok, path}

  defp resolve_repo(nil, _existing), do: {:ok, nil}

  defp compare_immutable(%__MODULE__{existing_snapshot: nil}), do: :ok

  defp compare_immutable(%__MODULE__{existing_snapshot: existing, snapshot: current}) do
    old = immutable_comparison(existing["immutable"])
    new = immutable_comparison(current["immutable"])

    if old == new do
      :ok
    else
      differences =
        old
        |> flattened()
        |> Enum.reduce([], fn {path, value}, acc ->
          case Map.fetch(flattened(new), path) do
            {:ok, ^value} ->
              acc

            {:ok, new_value} ->
              [
                "#{path}: immutable value changed from #{inspect(value)} to #{inspect(new_value)}"
                | acc
              ]

            :error ->
              ["#{path}: immutable value is missing" | acc]
          end
        end)
        |> Enum.reverse()

      {:error, {:immutable_config_changed, differences}}
    end
  end

  defp immutable_comparison(immutable) do
    managed = immutable["managed_repo"]
    backend = Map.take(immutable["backend"] || %{}, ~w(type command protocol_config))

    %{
      "workspace" => immutable["workspace"],
      "repo_mode" => immutable["repo_mode"],
      "managed_repo" => if(managed, do: Map.take(managed, ["canonical_path"]), else: nil),
      "listen" => immutable["listen"],
      "backend" => backend,
      "prompts" => immutable["prompts"] || %{}
    }
  end

  defp section_errors(yaml) do
    for field <- @root_fields,
        value = yaml[field],
        not is_nil(value) and not is_map(value),
        do: "#{field}: must be a mapping"
  end

  defp validate_host(host) when is_binary(host) do
    case :inet.parse_address(String.to_charlist(host)) do
      {:ok, _} -> []
      _ -> ["server.host: must be a numeric IPv4 or IPv6 address"]
    end
  end

  defp validate_host(_), do: ["server.host: must be a string"]

  defp validate_port(port) when is_integer(port) and port in 1..65_535, do: []
  defp validate_port(_), do: ["server.port: must be an integer from 1 through 65535"]

  defp validate_backend(
         type,
         command,
         model,
         reasoning_effort,
         approval_policy,
         sandbox_policy,
         protocol_config
       ) do
    []
    |> maybe_error(
      type not in @backend_types,
      "backend.type: must be codex_app_server or cursor_acp"
    )
    |> maybe_error(
      not valid_command?(command),
      "backend.command: must be a command string or a non-empty list of strings"
    )
    |> Kernel.++(validate_optional_string(model, "backend.model"))
    |> Kernel.++(validate_effort(reasoning_effort, "backend"))
    |> Kernel.++(validate_permission(type, :approval_policy, approval_policy, "backend"))
    |> Kernel.++(validate_permission(type, :sandbox_policy, sandbox_policy, "backend"))
    |> maybe_error(not is_map(protocol_config), "backend.protocol_config: must be a mapping")
  end

  defp validate_campaign(
         plan,
         max_attempts,
         history_n,
         iteration_agents,
         integration_agent,
         references,
         stop_conditions,
         default_backend
       ) do
    stop_mode =
      if is_map(stop_conditions), do: Map.get(stop_conditions, "mode", "all_goals"), else: nil

    []
    |> maybe_error(not is_boolean(plan), "campaign.plan: must be true or false")
    |> maybe_error(
      not (is_nil(max_attempts) or (is_integer(max_attempts) and max_attempts >= 0)),
      "campaign.max_attempts: must be null or a non-negative integer"
    )
    |> maybe_error(
      not (is_integer(history_n) and history_n >= 0),
      "campaign.history_n: must be a non-negative integer"
    )
    |> Kernel.++(validate_iteration_agents(iteration_agents, default_backend))
    |> Kernel.++(validate_integration_agent(integration_agent, default_backend))
    |> maybe_error(not is_list(references), "campaign.reference_catalog: must be a list")
    |> maybe_error(not is_map(stop_conditions), "campaign.stop_conditions: must be a mapping")
    |> maybe_error(
      stop_mode not in ["all_goals", "any_goal"],
      "campaign.stop_conditions.mode: must be all_goals or any_goal"
    )
  end

  defp validate_prompts(prompts) do
    Enum.flat_map(prompts, fn {name, value} ->
      if is_binary(value) and String.trim(value) != "",
        do: [],
        else: ["prompts.#{name}: must be a non-empty path string"]
    end)
  end

  defp validate_sync(sync) do
    Enum.flat_map(~w(remote branch), fn field ->
      case sync[field] do
        nil -> []
        value when is_binary(value) and value != "" -> []
        _ -> ["sync.#{field}: must be a non-empty string when set"]
      end
    end)
  end

  defp validate_iteration_agents(agents, default_backend) when is_list(agents) and agents != [] do
    agents
    |> Enum.with_index()
    |> Enum.flat_map(fn
      {agent, index} when is_map(agent) ->
        prefix = "campaign.iteration_agents[#{index}]"
        agent = stringify_keys(agent)
        backend = agent["backend"] || default_backend

        unknown_fields(agent, @agent_profile_fields, prefix) ++
          validate_optional_backend(agent["backend"], prefix) ++
          validate_optional_command(agent["command"], prefix) ++
          validate_optional_string(agent["name"], "#{prefix}.name") ++
          validate_optional_string(agent["model"], "#{prefix}.model") ++
          validate_effort(agent["reasoning_effort"], prefix) ++
          validate_permission(backend, :approval_policy, agent["approval_policy"], prefix) ++
          validate_permission(backend, :sandbox_policy, agent["sandbox_policy"], prefix) ++
          if(is_nil(agent["env"]) or is_map(agent["env"]),
            do: [],
            else: ["#{prefix}.env: must be a mapping"]
          ) ++
          if(is_nil(agent["protocol_config"]) or is_map(agent["protocol_config"]),
            do: [],
            else: ["#{prefix}.protocol_config: must be a mapping"]
          )

      {_agent, index} ->
        ["campaign.iteration_agents[#{index}]: must be a mapping"]
    end)
  end

  defp validate_iteration_agents(_agents, _default_backend),
    do: ["campaign.iteration_agents: must be a non-empty list"]

  defp validate_integration_agent(agent, default_backend) when is_map(agent) do
    prefix = "campaign.integration_agent"
    agent = stringify_keys(agent)
    backend = agent["backend"] || default_backend

    unknown_fields(agent, @agent_profile_fields, prefix) ++
      validate_optional_backend(agent["backend"], prefix) ++
      validate_optional_command(agent["command"], prefix) ++
      validate_optional_string(agent["name"], "#{prefix}.name") ++
      validate_optional_string(agent["model"], "#{prefix}.model") ++
      validate_effort(agent["reasoning_effort"], prefix) ++
      validate_permission(backend, :approval_policy, agent["approval_policy"], prefix) ++
      validate_permission(backend, :sandbox_policy, agent["sandbox_policy"], prefix) ++
      if(is_nil(agent["env"]) or is_map(agent["env"]),
        do: [],
        else: ["#{prefix}.env: must be a mapping"]
      ) ++
      if(is_nil(agent["protocol_config"]) or is_map(agent["protocol_config"]),
        do: [],
        else: ["#{prefix}.protocol_config: must be a mapping"]
      )
  end

  defp validate_integration_agent(_agent, _default_backend),
    do: ["campaign.integration_agent: must be a mapping"]

  defp validate_optional_backend(nil, _prefix), do: []

  defp validate_optional_backend(value, prefix),
    do: if(value in @backend_types, do: [], else: ["#{prefix}.backend: invalid backend"])

  defp validate_optional_command(nil, _prefix), do: []

  defp validate_optional_command(value, prefix),
    do: if(valid_command?(value), do: [], else: ["#{prefix}.command: invalid command"])

  defp validate_optional_string(nil, _field), do: []
  defp validate_optional_string(value, _field) when is_binary(value) and value != "", do: []
  defp validate_optional_string(_value, field), do: ["#{field}: must be a non-empty string"]

  defp validate_effort(nil, _prefix), do: []

  defp validate_effort(value, prefix) do
    if value in ~w(low medium high xhigh max ultra),
      do: [],
      else: ["#{prefix}.reasoning_effort: invalid effort"]
  end

  defp validate_permission(_backend, _kind, nil, _prefix), do: []

  defp validate_permission(backend, kind, value, prefix) do
    case PermissionPolicy.parse(backend, kind, value) do
      {:ok, _value} -> []
      {:error, message} -> ["#{prefix}.#{kind}: #{message} for #{backend}"]
    end
  end

  defp default_iteration_agents(type),
    do: [%{"backend" => type, "reasoning_effort" => "high"}]

  defp default_integration_agent(type),
    do: %{"name" => "integration", "backend" => type, "reasoning_effort" => "high"}

  defp normalize_iteration_agents(agents, default_type, default_command) do
    Enum.with_index(agents)
    |> Enum.map(fn {agent, index} ->
      agent = stringify_keys(agent)
      type = agent["backend"] || default_type
      permissions = PermissionPolicy.defaults(type)

      %{
        "name" => agent["name"] || "slot-#{index + 1}",
        "backend" => type,
        "command" =>
          normalize_command(agent["command"] || command_for(type, default_type, default_command)),
        "model" => agent["model"],
        "reasoning_effort" => agent["reasoning_effort"] || "high",
        "approval_policy" =>
          normalize_permission(
            type,
            :approval_policy,
            agent["approval_policy"] || permissions.approval_policy
          ),
        "sandbox_policy" =>
          normalize_permission(
            type,
            :sandbox_policy,
            agent["sandbox_policy"] || permissions.sandbox_policy
          ),
        "env" => agent["env"] || %{},
        "protocol_config" => agent["protocol_config"] || %{}
      }
    end)
  end

  defp normalize_integration_agent(agent, default_type, default_command) do
    agent = stringify_keys(agent)
    type = agent["backend"] || default_type
    permissions = PermissionPolicy.defaults(type)

    %{
      "name" => agent["name"] || "integration",
      "backend" => type,
      "command" =>
        normalize_command(agent["command"] || command_for(type, default_type, default_command)),
      "model" => agent["model"],
      "reasoning_effort" => agent["reasoning_effort"] || "high",
      "approval_policy" =>
        normalize_permission(
          type,
          :approval_policy,
          agent["approval_policy"] || permissions.approval_policy
        ),
      "sandbox_policy" =>
        normalize_permission(
          type,
          :sandbox_policy,
          agent["sandbox_policy"] || permissions.sandbox_policy
        ),
      "env" => agent["env"] || %{},
      "protocol_config" => agent["protocol_config"] || %{}
    }
  end

  defp command_for(type, type, default_command), do: default_command
  defp command_for(type, _default_type, _default_command), do: default_command(type)

  defp normalize_permission(backend, kind, value) do
    case PermissionPolicy.parse(backend, kind, value) do
      {:ok, normalized} -> normalized
      {:error, _message} -> value
    end
  end

  defp resolve_prompt_paths(prompts, source_path) do
    Map.new(prompts, fn {kind, path} ->
      resolved =
        if Path.type(path) == :absolute,
          do: Path.expand(path),
          else: Path.expand(path, Path.dirname(source_path))

      {kind, resolved}
    end)
  end

  defp maybe_error(errors, false, _message), do: errors
  defp maybe_error(errors, true, message), do: errors ++ [message]

  defp put_optional(map, _key, nil), do: map
  defp put_optional(map, key, value), do: Map.put(map, key, value)

  defp valid_command?(command) when is_binary(command), do: String.trim(command) != ""

  defp valid_command?([first | rest]),
    do: Enum.all?([first | rest], &(is_binary(&1) and &1 != ""))

  defp valid_command?(_), do: false

  defp normalize_command(command) when is_binary(command), do: [command]
  defp normalize_command(command), do: command

  defp default_command("cursor_acp"), do: ["cursor-agent", "acp"]
  defp default_command(_), do: ["codex", "app-server", "--listen", "stdio://"]

  defp unknown_fields(map, allowed, prefix) do
    map
    |> Map.keys()
    |> Enum.reject(&(&1 in allowed))
    |> Enum.sort()
    |> Enum.map(&"#{prefix}.#{&1}: unknown field")
  end

  defp map(value) when is_map(value), do: value
  defp map(_), do: %{}

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp flattened(map), do: flatten(map, nil, %{})

  defp flatten(map, prefix, acc) when is_map(map) do
    Enum.reduce(map, acc, fn {key, value}, inner ->
      path = if prefix, do: "#{prefix}.#{key}", else: key
      if is_map(value), do: flatten(value, path, inner), else: Map.put(inner, path, value)
    end)
  end
end
