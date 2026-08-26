defmodule Pika.Optimization.Config.Agent do
  @moduledoc "One fully expanded Backend configuration; it has no Profile identity."

  @enforce_keys [:backend, :approval_policy, :sandbox]
  defstruct [
    :backend,
    :command,
    :model,
    :reasoning_effort,
    :approval_policy,
    :sandbox,
    env: %{},
    protocol_config: %{},
    options: %{},
    fallbacks: []
  ]

  @type backend :: :codex_app_server | :cursor_acp | :cursor_headless
  @type t :: %__MODULE__{
          backend: backend(),
          command: String.t() | [String.t()] | nil,
          model: String.t() | nil,
          reasoning_effort: String.t() | nil,
          approval_policy: String.t(),
          sandbox: String.t(),
          env: %{optional(String.t()) => String.t()},
          protocol_config: map(),
          options: map(),
          fallbacks: [t()]
        }

  @spec snapshot(t()) :: map()
  def snapshot(%__MODULE__{} = agent) do
    %{
      "backend" => agent.backend,
      "command" => agent.command,
      "model" => agent.model,
      "reasoning_effort" => agent.reasoning_effort,
      "approval_policy" => agent.approval_policy,
      "sandbox_policy" => agent.sandbox,
      "env" => agent.env,
      "protocol_config" => agent.protocol_config
    }
    |> Map.merge(agent.options)
    |> Enum.reject(fn {_key, value} -> is_nil(value) end)
    |> Map.new()
  end

  @spec chain(t()) :: [t(), ...]
  def chain(%__MODULE__{} = agent) do
    [%{agent | fallbacks: []} | Enum.map(agent.fallbacks, &%{&1 | fallbacks: []})]
  end

  @spec chain_sha256(t()) :: String.t()
  def chain_sha256(%__MODULE__{} = agent) do
    agent
    |> chain()
    |> Enum.map(&snapshot/1)
    |> :erlang.term_to_binary([:deterministic])
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end
end

defmodule Pika.Optimization.Config.ReferenceProject do
  @moduledoc "A Git repository exposed read-only to newly created Iteration Attempts."

  @enforce_keys [:id, :url, :description]
  defstruct @enforce_keys ++ [revision: nil]

  @type t :: %__MODULE__{
          id: String.t(),
          url: String.t(),
          description: String.t(),
          revision: String.t() | nil
        }
end

defmodule Pika.Optimization.Config.Role do
  @moduledoc false

  alias Pika.Optimization.Config.Agent

  @enforce_keys [:agent]
  defstruct [:agent, :max_followups, :generator_max_attempts, :regression_feedback_cases]

  @type t :: %__MODULE__{
          agent: Agent.t(),
          max_followups: non_neg_integer() | nil,
          generator_max_attempts: pos_integer() | nil,
          regression_feedback_cases: non_neg_integer() | nil
        }
end

defmodule Pika.Optimization.Config.Iteration do
  @moduledoc false

  alias Pika.Optimization.Config.Agent

  @enforce_keys [:agents, :history_limit, :max_followups, :max_pending_attempts]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          agents: [Agent.t(), ...],
          history_limit: non_neg_integer(),
          max_followups: non_neg_integer(),
          max_pending_attempts: non_neg_integer()
        }
end

defmodule Pika.Optimization.Config.ProgressSummary do
  @moduledoc false

  alias Pika.Optimization.Config.Agent

  @enforce_keys [:agent, :interval_ms, :time_zone, :max_followups]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          agent: Agent.t(),
          interval_ms: pos_integer(),
          time_zone: String.t(),
          max_followups: non_neg_integer()
        }
end

defmodule Pika.Optimization.Config do
  @moduledoc "Parses and validates the incompatible v2 Optimization configuration."

  alias Pika.Optimization.Config.{Agent, Iteration, ProgressSummary, ReferenceProject, Role}

  require Logger

  @root_fields ~w(version repo workspace token reference_projects agents)
  @reference_project_fields ~w(id url description revision)
  @reference_id_pattern ~r/\A[A-Za-z0-9][A-Za-z0-9._-]*\z/
  @reference_sha_pattern ~r/\A[0-9a-f]{40}([0-9a-f]{24})?\z/i
  @reference_url_schemes ~w(http https ssh git file)
  @reserved_agent_fields ~w(
    backend command model reasoning_effort approval_policy sandbox env protocol_config fallbacks
    max_followups generator_max_attempts regression_feedback_cases interval timezone agents
    history_limit max_pending_attempts
  )
  @forbidden_agent_fields ~w(name profile profile_key)

  @enforce_keys [
    :source_path,
    :repo,
    :workspace,
    :baseline_alignment,
    :baseline_verify,
    :iteration,
    :integration
  ]
  defstruct @enforce_keys ++
              [
                :token,
                :baseline_verify_followup,
                :iteration_followup,
                :integration_followup,
                :progress_summary,
                reference_projects: []
              ]

  @type t :: %__MODULE__{
          source_path: Path.t(),
          repo: Path.t(),
          workspace: Path.t(),
          token: String.t() | nil,
          reference_projects: [ReferenceProject.t()],
          baseline_alignment: Role.t(),
          baseline_verify: Role.t(),
          baseline_verify_followup: Role.t() | nil,
          iteration: Iteration.t(),
          iteration_followup: Role.t() | nil,
          integration: Role.t(),
          integration_followup: Role.t() | nil,
          progress_summary: ProgressSummary.t() | nil
        }

  @spec load(Path.t()) :: {:ok, t()} | {:error, {:invalid_v2_config, [String.t()]}}
  def load(path) when is_binary(path) do
    expanded = Path.expand(path)

    with true <- File.regular?(expanded) || error("config: file does not exist: #{expanded}"),
         {:ok, yaml} <- read_yaml(expanded),
         {:ok, config} <- with_config_source(expanded, fn -> build(expanded, yaml) end) do
      {:ok, config}
    else
      {:error, {:invalid_v2_config, _errors}} = error -> error
      false -> error("config: file does not exist: #{expanded}")
    end
  end

  @spec role_agent(t(), atom() | String.t()) ::
          {:ok, Agent.t()} | {:error, {:agent_role_disabled | :unknown_agent_role, String.t()}}
  def role_agent(%__MODULE__{} = config, role_id) do
    role_id = to_string(role_id)

    role =
      case role_id do
        "baseline_alignment" -> config.baseline_alignment
        "baseline_verify" -> config.baseline_verify
        "baseline_verify_followup" -> config.baseline_verify_followup
        "iteration_followup" -> config.iteration_followup
        "integration" -> config.integration
        "integration_followup" -> config.integration_followup
        "progress_summary" -> config.progress_summary
        _other -> :unknown
      end

    case role do
      %Role{agent: agent} -> {:ok, agent}
      %ProgressSummary{agent: agent} -> {:ok, agent}
      nil -> {:error, {:agent_role_disabled, role_id}}
      :unknown -> {:error, {:unknown_agent_role, role_id}}
    end
  end

  @spec iteration_agents(t()) :: [Agent.t(), ...]
  def iteration_agents(%__MODULE__{iteration: %Iteration{agents: agents}}), do: agents

  defp read_yaml(path) do
    case YamlElixir.read_from_file(path) do
      {:ok, value} when is_map(value) ->
        value = value |> stringify_keys() |> expand_yaml_merges()
        {:ok, value}

      {:ok, _value} ->
        error("config: YAML document must be a mapping")

      {:error, reason} ->
        error("config: #{format_reason(reason)}")
    end
  rescue
    error -> error("config: #{Exception.message(error)}")
  end

  defp build(source_path, yaml) do
    with :ok <- reject_unknown(yaml, @root_fields, "config"),
         :ok <- require_equal(yaml["version"], 2, "version: must be 2"),
         {:ok, repo} <- path_value(yaml["repo"], source_path, "repo"),
         {:ok, workspace} <- path_value(yaml["workspace"], source_path, "workspace"),
         {:ok, token} <- access_token(yaml["token"]),
         {:ok, reference_projects} <- reference_projects(yaml["reference_projects"]),
         {:ok, agents} <- required_map(yaml["agents"], "agents"),
         {:ok, baseline_alignment} <- required_role(agents, "baseline_alignment", []),
         {:ok, baseline_verify} <-
           required_role(agents, "baseline_verify", max_followups: {:non_neg_integer, 8}),
         {:ok, baseline_verify_followup} <- optional_followup(agents, "baseline_verify_followup"),
         {:ok, iteration} <- iteration(agents["iteration"]),
         {:ok, iteration_followup} <- optional_followup(agents, "iteration_followup"),
         {:ok, integration} <- integration(agents["integration"]),
         {:ok, integration_followup} <- optional_followup(agents, "integration_followup"),
         {:ok, progress_summary} <- progress_summary(agents["progress_summary"]) do
      {:ok,
       %__MODULE__{
         source_path: source_path,
         repo: repo,
         workspace: workspace,
         token: token,
         reference_projects: reference_projects,
         baseline_alignment: baseline_alignment,
         baseline_verify: baseline_verify,
         baseline_verify_followup: baseline_verify_followup,
         iteration: iteration,
         iteration_followup: iteration_followup,
         integration: integration,
         integration_followup: integration_followup,
         progress_summary: progress_summary
       }}
    end
  end

  defp required_role(agents, role_id, controls) do
    with {:ok, value} <- required_map(agents[role_id], "agents.#{role_id}"),
         {:ok, agent} <- agent(value, role_id, Keyword.keys(controls)),
         {:ok, control_values} <- controls(value, role_id, controls) do
      {:ok, struct!(Role, [agent: agent] ++ control_values)}
    end
  end

  defp optional_followup(agents, role_id) do
    case agents[role_id] do
      nil ->
        {:ok, nil}

      value ->
        with {:ok, value} <- required_map(value, "agents.#{role_id}"),
             {:ok, agent} <- agent(value, role_id, [:generator_max_attempts]),
             {:ok, generator_max_attempts} <-
               integer(
                 value["generator_max_attempts"],
                 3,
                 1,
                 "agents.#{role_id}.generator_max_attempts"
               ) do
          {:ok, %Role{agent: agent, generator_max_attempts: generator_max_attempts}}
        end
    end
  end

  defp iteration(nil), do: error("agents.iteration: is required")

  defp iteration(value) do
    with {:ok, value} <- required_map(value, "agents.iteration"),
         {:ok, raw_agents} <- non_empty_list(value["agents"], "agents.iteration.agents"),
         {:ok, agents} <- map_agents(raw_agents, "iteration"),
         {:ok, history_limit} <-
           integer(value["history_limit"], 20, 0, "agents.iteration.history_limit"),
         {:ok, max_followups} <-
           integer(value["max_followups"], 5, 0, "agents.iteration.max_followups"),
         {:ok, max_pending_attempts} <-
           integer(
             value["max_pending_attempts"],
             0,
             0,
             "agents.iteration.max_pending_attempts"
           ) do
      {:ok,
       %Iteration{
         agents: agents,
         history_limit: history_limit,
         max_followups: max_followups,
         max_pending_attempts: max_pending_attempts
       }}
    end
  end

  defp integration(nil), do: error("agents.integration: is required")

  defp integration(value) do
    with {:ok, value} <- required_map(value, "agents.integration"),
         {:ok, agent} <-
           agent(value, "integration", [:max_followups, :regression_feedback_cases]),
         {:ok, max_followups} <-
           integer(value["max_followups"], 8, 0, "agents.integration.max_followups"),
         {:ok, regression_feedback_cases} <-
           integer(
             value["regression_feedback_cases"],
             3,
             0,
             "agents.integration.regression_feedback_cases"
           ) do
      {:ok,
       %Role{
         agent: agent,
         max_followups: max_followups,
         regression_feedback_cases: regression_feedback_cases
       }}
    end
  end

  defp progress_summary(nil), do: {:ok, nil}

  defp progress_summary(value) do
    with {:ok, value} <- required_map(value, "agents.progress_summary"),
         {:ok, agent} <-
           agent(value, "progress_summary", [:interval, :timezone, :max_followups]),
         {:ok, interval_ms} <- interval(value["interval"] || "5m"),
         {:ok, time_zone} <- time_zone(value["timezone"] || "Asia/Shanghai"),
         {:ok, max_followups} <-
           integer(value["max_followups"], 3, 0, "agents.progress_summary.max_followups") do
      {:ok,
       %ProgressSummary{
         agent: agent,
         interval_ms: interval_ms,
         time_zone: time_zone,
         max_followups: max_followups
       }}
    end
  end

  defp map_agents(raw_agents, role_id) do
    raw_agents
    |> Enum.with_index()
    |> Enum.reduce_while({:ok, []}, fn {value, index}, {:ok, parsed} ->
      with {:ok, value} <- required_map(value, "agents.#{role_id}.agents[#{index}]"),
           {:ok, agent} <- agent(value, "#{role_id}.agents[#{index}]", []) do
        {:cont, {:ok, [agent | parsed]}}
      else
        {:error, _} = error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, agents} -> {:ok, Enum.reverse(agents)}
      error -> error
    end
  end

  defp agent(value, path, control_keys, allow_fallbacks \\ true) do
    forbidden = Enum.filter(@forbidden_agent_fields, &Map.has_key?(value, &1))

    with [] <- forbidden,
         :ok <- reject_nested_fallbacks(value, path, allow_fallbacks),
         {:ok, backend} <- backend(value["backend"], "agents.#{path}.backend"),
         {:ok, command} <- command(value["command"], "agents.#{path}.command"),
         {:ok, model} <- optional_string(value["model"], "agents.#{path}.model"),
         {:ok, reasoning_effort} <-
           optional_string(value["reasoning_effort"], "agents.#{path}.reasoning_effort"),
         {:ok, approval_policy} <-
           permission(
             backend,
             :approval_policy,
             value["approval_policy"],
             "agents.#{path}.approval_policy"
           ),
         {:ok, sandbox} <-
           permission(backend, :sandbox_policy, value["sandbox"], "agents.#{path}.sandbox"),
         {:ok, env} <- string_map(value["env"] || %{}, "agents.#{path}.env"),
         {:ok, protocol_config} <-
           required_map(value["protocol_config"] || %{}, "agents.#{path}.protocol_config"),
         {:ok, fallbacks} <- fallbacks(value["fallbacks"], path, allow_fallbacks) do
      reserved = MapSet.new(@reserved_agent_fields ++ Enum.map(control_keys, &Atom.to_string/1))

      options =
        value
        |> Enum.reject(fn {key, _value} -> MapSet.member?(reserved, key) end)
        |> Map.new()

      if map_size(options) == 0 do
        {:ok,
         %Agent{
           backend: backend,
           command: command,
           model: model,
           reasoning_effort: reasoning_effort,
           approval_policy: approval_policy,
           sandbox: sandbox,
           env: env,
           protocol_config: protocol_config,
           options: %{},
           fallbacks: fallbacks
         }}
      else
        error(
          "agents.#{path}: unsupported Backend fields #{inspect(Map.keys(options) |> Enum.sort())}"
        )
      end
    else
      {:error, _} = error -> error
      [_ | _] -> error("agents.#{path}: Agent Profile fields are not allowed")
    end
  end

  defp reject_nested_fallbacks(value, path, false) do
    if Map.has_key?(value, "fallbacks"),
      do: error("agents.#{path}.fallbacks: nested Backend fallback chains are not allowed"),
      else: :ok
  end

  defp reject_nested_fallbacks(_value, _path, true), do: :ok

  defp fallbacks(nil, _path, _allow_fallbacks), do: {:ok, []}

  defp fallbacks(values, path, true) when is_list(values) do
    values
    |> Enum.with_index()
    |> Enum.reduce_while({:ok, []}, fn {value, index}, {:ok, parsed} ->
      fallback_path = "#{path}.fallbacks[#{index}]"

      with {:ok, value} <- required_map(value, "agents.#{fallback_path}"),
           {:ok, fallback} <- agent(value, fallback_path, [], false) do
        {:cont, {:ok, [fallback | parsed]}}
      else
        {:error, _reason} = error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, parsed} -> {:ok, Enum.reverse(parsed)}
      {:error, _reason} = error -> error
    end
  end

  defp fallbacks(_values, path, true),
    do: error("agents.#{path}.fallbacks: must be a list")

  defp fallbacks(_values, path, false),
    do: error("agents.#{path}.fallbacks: nested Backend fallback chains are not allowed")

  defp controls(value, path, controls) do
    Enum.reduce_while(controls, {:ok, []}, fn {key, {kind, default}}, {:ok, parsed} ->
      field = Atom.to_string(key)

      case kind do
        :non_neg_integer ->
          case integer(value[field], default, 0, "agents.#{path}.#{field}") do
            {:ok, result} -> {:cont, {:ok, [{key, result} | parsed]}}
            {:error, _} = error -> {:halt, error}
          end
      end
    end)
    |> case do
      {:ok, parsed} -> {:ok, Enum.reverse(parsed)}
      error -> error
    end
  end

  defp backend(value, _path)
       when value in ["codex", "codex_app_server", :codex, :codex_app_server],
       do: {:ok, :codex_app_server}

  defp backend(value, path) when value in ["cursor", "cursor_acp", :cursor, :cursor_acp] do
    warning_key = {
      __MODULE__,
      :cursor_acp_deprecation,
      Process.get({__MODULE__, :config_source}, :unknown),
      path
    }

    unless :persistent_term.get(warning_key, false) do
      Logger.warning(
        "#{path}: Cursor ACP is deprecated; migrate this Backend Endpoint to cursor_headless"
      )

      :persistent_term.put(warning_key, true)
    end

    {:ok, :cursor_acp}
  end

  defp backend(value, _path)
       when value in ["cursor_headless", "cursor-headless", :cursor_headless],
       do: {:ok, :cursor_headless}

  defp backend(_value, path),
    do: error("#{path}: must be codex, cursor_acp, or cursor_headless")

  defp permission(backend, kind, value, path) do
    with {:ok, value} <- required_string(value, path) do
      case Pika.AgentBackend.PermissionPolicy.parse(backend, kind, value) do
        {:ok, normalized} -> {:ok, normalized}
        {:error, expected} -> error("#{path}: #{expected}")
      end
    end
  end

  defp command(nil, _path), do: {:ok, nil}
  defp command(value, _path) when is_binary(value) and value != "", do: {:ok, value}

  defp command(value, path) when is_list(value) and value != [] do
    if Enum.all?(value, &(is_binary(&1) and &1 != "")),
      do: {:ok, value},
      else: error("#{path}: must be a command string or string list")
  end

  defp command(_value, path), do: error("#{path}: must be a command string or string list")

  defp interval(value) when is_binary(value) do
    case Regex.run(~r/^(\d+)(ms|s|m|h)$/, String.trim(value), capture: :all_but_first) do
      [amount, unit] ->
        amount = String.to_integer(amount)
        multiplier = %{"ms" => 1, "s" => 1_000, "m" => 60_000, "h" => 3_600_000}[unit]

        if amount > 0,
          do: {:ok, amount * multiplier},
          else: error("agents.progress_summary.interval: must be positive")

      _ ->
        error("agents.progress_summary.interval: must use ms, s, m, or h")
    end
  end

  defp interval(_value), do: error("agents.progress_summary.interval: must be a duration string")

  defp time_zone(value) when is_binary(value) do
    case DateTime.shift_zone(DateTime.utc_now(), value, Tz.TimeZoneDatabase) do
      {:ok, _datetime} ->
        {:ok, value}

      {:error, reason} ->
        error("agents.progress_summary.timezone: invalid time zone #{inspect(value)} (#{reason})")
    end
  end

  defp time_zone(value),
    do: error("agents.progress_summary.timezone: must be a time zone name, got #{inspect(value)}")

  defp with_config_source(source, fun) do
    key = {__MODULE__, :config_source}
    previous = Process.get(key)
    Process.put(key, source)

    try do
      fun.()
    after
      if is_nil(previous), do: Process.delete(key), else: Process.put(key, previous)
    end
  end

  defp integer(nil, default, _minimum, _path), do: {:ok, default}

  defp integer(value, _default, minimum, _path) when is_integer(value) and value >= minimum,
    do: {:ok, value}

  defp integer(_value, _default, minimum, path),
    do: error("#{path}: must be an integer >= #{minimum}")

  defp non_empty_list(value, _path) when is_list(value) and value != [], do: {:ok, value}
  defp non_empty_list(_value, path), do: error("#{path}: must be a non-empty list")

  defp path_value(value, source_path, _field) when is_binary(value) and value != "" do
    base = Path.dirname(source_path)
    {:ok, Path.expand(value, base)}
  end

  defp path_value(_value, _source_path, field), do: error("#{field}: must be a path string")

  defp required_map(value, _path) when is_map(value), do: {:ok, value}
  defp required_map(_value, path), do: error("#{path}: must be a mapping")

  defp required_string(value, _path) when is_binary(value) and value != "", do: {:ok, value}
  defp required_string(_value, path), do: error("#{path}: must be a non-empty string")

  defp optional_string(nil, _path), do: {:ok, nil}
  defp optional_string(value, _path) when is_binary(value) and value != "", do: {:ok, value}
  defp optional_string(_value, path), do: error("#{path}: must be a non-empty string")

  defp access_token(nil), do: {:ok, nil}

  defp access_token(value) when is_binary(value) do
    if String.trim(value) == "",
      do: error("token: must be a non-empty string"),
      else: {:ok, value}
  end

  defp access_token(_value), do: error("token: must be a non-empty string")

  defp reference_projects(nil), do: {:ok, []}

  defp reference_projects(values) when is_list(values) do
    values
    |> Enum.with_index()
    |> Enum.reduce_while({:ok, []}, fn {value, index}, {:ok, parsed} ->
      path = "reference_projects[#{index}]"

      with {:ok, value} <- required_map(value, path),
           :ok <- reject_unknown(value, @reference_project_fields, path),
           {:ok, id} <- required_string(value["id"], "#{path}.id"),
           :ok <- reference_id(id, "#{path}.id"),
           {:ok, url} <- required_string(value["url"], "#{path}.url"),
           :ok <- reference_url(url, "#{path}.url"),
           {:ok, description} <- reference_description(value["description"], id, path),
           {:ok, revision} <- reference_revision(value["revision"], path),
           :ok <- unique_reference_id(parsed, id, path) do
        project = %ReferenceProject{
          id: id,
          url: url,
          description: description,
          revision: revision
        }

        {:cont, {:ok, [project | parsed]}}
      else
        {:error, _reason} = error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, parsed} -> {:ok, Enum.reverse(parsed)}
      {:error, _reason} = error -> error
    end
  end

  defp reference_projects(_value), do: error("reference_projects: must be a list")

  defp reference_id(id, path)
       when byte_size(id) <= 64 and id not in [".", ".."] do
    if Regex.match?(@reference_id_pattern, id),
      do: :ok,
      else: error("#{path}: must safely map to ref/<id>")
  end

  defp reference_id(_id, path),
    do: error("#{path}: must be at most 64 bytes and safely map to ref/<id>")

  defp reference_url(url, path) do
    uri = URI.parse(url)

    valid? =
      byte_size(url) <= 2_048 and not String.starts_with?(url, "-") and
        not Regex.match?(~r/[\x00-\x20\x7f]/, url) and
        (Path.type(url) == :absolute or reference_scp_url?(url) or
           (uri.scheme in @reference_url_schemes and is_binary(uri.path) and
              uri.path not in ["", "/"] and (uri.scheme == "file" or is_binary(uri.host))))

    if valid?, do: :ok, else: error("#{path}: must be a Git URL or absolute local path")
  end

  defp reference_scp_url?(url),
    do: Regex.match?(~r/\A(?:[^@:\/\s]+@)?[^:\/\s]+:[^\/\s][^\s]*\z/, url)

  defp reference_description(nil, id, _path), do: {:ok, "Reference Project #{id}"}

  defp reference_description(value, _id, _path)
       when is_binary(value) and value != "" and byte_size(value) <= 240,
       do: {:ok, value}

  defp reference_description(_value, _id, path),
    do: error("#{path}.description: must be a non-empty string of at most 240 bytes")

  defp reference_revision(nil, _path), do: {:ok, nil}

  defp reference_revision(value, path) when is_binary(value) and value != "" do
    valid? =
      byte_size(value) <= 256 and not String.starts_with?(value, "-") and
        not Regex.match?(~r/[\x00-\x20\x7f~^:?*\[]/, value) and
        (Regex.match?(@reference_sha_pattern, value) or not String.contains?(value, ".."))

    if valid?, do: {:ok, value}, else: error("#{path}.revision: must be a safe Git revision")
  end

  defp reference_revision(_value, path),
    do: error("#{path}.revision: must be a non-empty Git revision")

  defp unique_reference_id(parsed, id, path) do
    if Enum.any?(parsed, &(String.downcase(&1.id) == String.downcase(id))),
      do: error("#{path}.id: duplicate Reference Project id #{inspect(id)}"),
      else: :ok
  end

  defp string_map(value, path) when is_map(value) do
    if Enum.all?(value, fn {key, item} -> is_binary(key) and is_binary(item) end),
      do: {:ok, value},
      else: error("#{path}: keys and values must be strings")
  end

  defp string_map(_value, path), do: error("#{path}: must be a mapping")

  defp reject_unknown(map, allowed, path) do
    unknown = Map.keys(map) -- allowed

    if unknown == [],
      do: :ok,
      else: error("#{path}: unknown fields: #{Enum.sort(unknown) |> Enum.join(", ")}")
  end

  defp require_equal(value, value, _message), do: :ok
  defp require_equal(_actual, _expected, message), do: error(message)

  defp stringify_keys(value) when is_map(value) do
    Map.new(value, fn {key, item} -> {to_string(key), stringify_keys(item)} end)
  end

  defp stringify_keys(value) when is_list(value), do: Enum.map(value, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp expand_yaml_merges(value) when is_list(value), do: Enum.map(value, &expand_yaml_merges/1)

  defp expand_yaml_merges(value) when is_map(value) do
    merge_values =
      value
      |> Enum.filter(fn {key, _item} -> String.starts_with?(key, "<<") end)
      |> Enum.map(&elem(&1, 1))

    own =
      value
      |> Enum.reject(fn {key, _item} -> String.starts_with?(key, "<<") end)
      |> Map.new(fn {key, item} -> {key, expand_yaml_merges(item)} end)

    merge_values
    |> merge_sources()
    |> Map.merge(own)
  end

  defp expand_yaml_merges(value), do: value

  defp merge_sources(nil), do: %{}
  defp merge_sources(value) when is_map(value), do: expand_yaml_merges(value)

  defp merge_sources(values) when is_list(values) do
    Enum.reduce(values, %{}, fn value, merged ->
      if is_map(value), do: Map.merge(merged, expand_yaml_merges(value)), else: merged
    end)
  end

  defp merge_sources(_value), do: %{}

  defp format_reason(%{__exception__: true} = reason), do: Exception.message(reason)
  defp format_reason(reason), do: inspect(reason)

  defp error(message), do: {:error, {:invalid_v2_config, [message]}}
end
