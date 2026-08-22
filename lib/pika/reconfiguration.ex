defmodule Pika.Reconfiguration do
  @moduledoc false

  alias Pika.{Config, FileSystem, ModelCatalog}
  alias Pika.AgentBackend.PermissionPolicy

  @efforts ~w(low medium high xhigh max ultra)

  def run(opts) when is_list(opts) do
    IO.puts("Pika Workspace reconfiguration")

    with {:ok, config} <- Config.load(opts[:config], workspace: opts[:workspace]),
         {:ok, section} <- choose_section(opts),
         {:ok, settings} <- collect_settings(section, config, opts),
         {:ok, source} <- File.read(opts[:config]),
         {:ok, updated} <- replace_section(source, section, settings),
         :ok <- validate_candidate(updated, config, opts),
         :ok <- FileSystem.atomic_write(opts[:config], updated) do
      IO.puts("Updated #{opts[:config]}")

      if section == :server,
        do: IO.puts("Restart pika serve to apply the new listen address."),
        else:
          IO.puts(
            "New Agent Sessions will use this configuration; active Sessions keep their frozen profile."
          )

      {:ok, Map.put(%{config_path: opts[:config]}, section, settings)}
    end
  end

  defp collect_summary(config, opts) do
    current = config.campaign["progress_summary"]
    interactive? = opts[:yes] != true

    with {:ok, enabled} <-
           boolean_setting(
             opts,
             :progress_summary,
             "Enable Progress Summary Agent?",
             current["enabled"],
             interactive?
           ),
         {:ok, backend} <- backend_setting(opts, current["backend"], enabled, interactive?),
         {:ok, model} <-
           profile_model_setting(
             opts,
             :summary_model,
             backend,
             "Progress Summary Agent",
             current["model"],
             enabled and interactive?
           ),
         {:ok, effort} <-
           enum_setting(
             opts,
             :summary_effort,
             "Summary reasoning effort",
             current["reasoning_effort"] || "medium",
             @efforts,
             enabled and interactive?
           ),
         {:ok, interval} <-
           integer_setting(
             opts,
             :summary_interval_minutes,
             "Summary interval in minutes",
             current["interval_minutes"] || 10,
             enabled and interactive?
           ) do
      {:ok,
       %{
         enabled: enabled,
         backend: backend,
         model: empty_to_nil(model),
         effort: effort,
         interval_minutes: interval
       }}
    end
  end

  defp collect_settings(:progress_summary, config, opts), do: collect_summary(config, opts)

  defp collect_settings(:server, config, opts) do
    interactive? = opts[:yes] != true

    with {:ok, host} <- string_setting(opts, :host, "Listen host", config.host, interactive?),
         {:ok, port} <-
           positive_integer_setting(opts, :port, "Listen port", config.port, interactive?) do
      {:ok, %{host: host, port: port}}
    end
  end

  defp collect_settings(:alignment_agent, config, opts) do
    collect_profile(
      Map.put(config.backend, "backend", config.backend["type"]),
      opts,
      :alignment,
      "Alignment/Baseline Agent"
    )
  end

  defp collect_settings(:iteration_agents, config, opts) do
    current = List.first(config.campaign["iteration_agents"])
    interactive? = opts[:yes] != true

    with {:ok, profile} <- collect_profile(current, opts, :iteration, "Iteration Agent"),
         {:ok, count} <-
           positive_integer_setting(
             opts,
             :iteration_agents,
             "Concurrent Iteration Agents",
             length(config.campaign["iteration_agents"]),
             interactive?
           ) do
      {:ok, Map.put(profile, :count, count)}
    end
  end

  defp collect_settings(:integration_agent, config, opts),
    do:
      collect_profile(
        config.campaign["integration_agent"],
        opts,
        :integration,
        "Integration Agent"
      )

  defp collect_settings(:integration_followup_agent, config, opts) do
    current = config.campaign["integration_followup_agent"]
    interactive? = opts[:yes] != true

    with {:ok, backend} <-
           profile_backend_setting(
             opts,
             :followup_backend,
             current["backend"],
             "FollowUp",
             interactive?
           ),
         {:ok, model} <-
           profile_model_setting(
             opts,
             :followup_model,
             backend,
             "Integration FollowUp Agent",
             current["model"],
             interactive?
           ),
         {:ok, effort} <-
           enum_setting(
             opts,
             :followup_effort,
             "FollowUp reasoning effort",
             current["reasoning_effort"] || "medium",
             @efforts,
             interactive?
           ),
         defaults <- PermissionPolicy.defaults(backend),
         permission_default <- fn field ->
           if short_backend(current["backend"]) == backend,
             do: current[field],
             else: Map.fetch!(defaults, String.to_existing_atom(field))
         end,
         {:ok, approval_policy} <-
           permission_setting(
             opts,
             :followup_approval_policy,
             backend,
             :approval_policy,
             "Integration FollowUp Agent",
             permission_default.("approval_policy"),
             interactive?
           ),
         {:ok, sandbox_policy} <-
           permission_setting(
             opts,
             :followup_sandbox_policy,
             backend,
             :sandbox_policy,
             "Integration FollowUp Agent",
             permission_default.("sandbox_policy"),
             interactive?
           ) do
      {:ok,
       %{
         backend: backend,
         model: empty_to_nil(model),
         effort: effort,
         approval_policy: approval_policy,
         sandbox_policy: sandbox_policy
       }}
    end
  end

  defp collect_settings(:campaign_limits, config, opts) do
    interactive? = opts[:yes] != true

    with {:ok, max_attempts} <-
           optional_non_negative_setting(
             opts,
             :max_attempts,
             "Maximum attempts (blank for unlimited)",
             config.campaign["max_attempts"],
             interactive?
           ),
         {:ok, max_unverified} <-
           non_negative_setting(
             opts,
             :max_unverified_attempts,
             "Maximum unverified attempts",
             config.campaign["max_unverified_attempts"],
             interactive?
           ) do
      {:ok, %{max_attempts: max_attempts, max_unverified_attempts: max_unverified}}
    end
  end

  defp collect_settings(:sync, config, opts) do
    interactive? = opts[:yes] != true

    current_enabled =
      Keyword.has_key?(opts, :sync_remote) or Keyword.has_key?(opts, :sync_branch) or
        not is_nil(config.sync["remote"]) or not is_nil(config.sync["branch"])

    with {:ok, enabled} <-
           boolean_setting(
             opts,
             :sync_enabled,
             "Enable Sync configuration?",
             current_enabled,
             interactive?
           ),
         {:ok, remote} <-
           string_setting(
             opts,
             :sync_remote,
             "Sync remote",
             config.sync["remote"] || "origin",
             enabled and interactive?
           ),
         {:ok, branch} <-
           string_setting(
             opts,
             :sync_branch,
             "Sync branch",
             config.sync["branch"] || "main",
             enabled and interactive?
           ) do
      {:ok, %{enabled: enabled, remote: remote, branch: branch}}
    end
  end

  defp collect_profile(current, opts, prefix, label) do
    interactive? = opts[:yes] != true
    backend_key = String.to_atom("#{prefix}_backend")
    model_key = String.to_atom("#{prefix}_model")
    effort_key = String.to_atom("#{prefix}_effort")
    approval_key = String.to_atom("#{prefix}_approval_policy")
    sandbox_key = String.to_atom("#{prefix}_sandbox_policy")

    with {:ok, backend} <-
           profile_backend_setting(
             opts,
             backend_key,
             current["backend"] || current["type"],
             label,
             interactive?
           ),
         {:ok, model} <-
           profile_model_setting(
             opts,
             model_key,
             backend,
             label,
             current["model"],
             interactive?
           ),
         {:ok, effort} <-
           enum_setting(
             opts,
             effort_key,
             "#{label} reasoning effort",
             current["reasoning_effort"] || "high",
             @efforts,
             interactive?
           ),
         defaults <- PermissionPolicy.defaults(backend),
         {:ok, approval} <-
           permission_setting(
             opts,
             approval_key,
             backend,
             :approval_policy,
             label,
             profile_default(current, backend, "approval_policy", defaults.approval_policy),
             interactive?
           ),
         {:ok, sandbox} <-
           permission_setting(
             opts,
             sandbox_key,
             backend,
             :sandbox_policy,
             label,
             profile_default(current, backend, "sandbox_policy", defaults.sandbox_policy),
             interactive?
           ) do
      {:ok,
       %{
         backend: backend,
         model: empty_to_nil(model),
         effort: effort,
         approval_policy: approval,
         sandbox_policy: sandbox
       }}
    end
  end

  defp profile_default(current, backend, field, fallback) do
    if short_backend(current["backend"] || current["type"]) == backend,
      do: current[field] || fallback,
      else: fallback
  end

  defp choose_section(opts) do
    cond do
      Enum.any?([:host, :port], &Keyword.has_key?(opts, &1)) ->
        {:ok, :server}

      Enum.any?(
        [
          :alignment_backend,
          :alignment_model,
          :alignment_effort,
          :alignment_approval_policy,
          :alignment_sandbox_policy
        ],
        &Keyword.has_key?(opts, &1)
      ) ->
        {:ok, :alignment_agent}

      Enum.any?(
        [
          :iteration_backend,
          :iteration_model,
          :iteration_effort,
          :iteration_approval_policy,
          :iteration_sandbox_policy,
          :iteration_agents
        ],
        &Keyword.has_key?(opts, &1)
      ) ->
        {:ok, :iteration_agents}

      Enum.any?(
        [
          :integration_backend,
          :integration_model,
          :integration_effort,
          :integration_approval_policy,
          :integration_sandbox_policy
        ],
        &Keyword.has_key?(opts, &1)
      ) ->
        {:ok, :integration_agent}

      Enum.any?(
        [
          :followup_backend,
          :followup_model,
          :followup_effort,
          :followup_approval_policy,
          :followup_sandbox_policy
        ],
        &Keyword.has_key?(opts, &1)
      ) ->
        {:ok, :integration_followup_agent}

      Enum.any?([:max_attempts, :max_unverified_attempts], &Keyword.has_key?(opts, &1)) ->
        {:ok, :campaign_limits}

      Enum.any?([:sync_remote, :sync_branch, :sync_enabled], &Keyword.has_key?(opts, &1)) ->
        {:ok, :sync}

      opts[:yes] == true ->
        {:ok, :progress_summary}

      true ->
        IO.puts("Configurable sections:")
        IO.puts("  1) Server listen address")
        IO.puts("  2) Alignment/Baseline Agent")
        IO.puts("  3) Iteration Agents")
        IO.puts("  4) Integration Agent")
        IO.puts("  5) Integration FollowUp Agent")
        IO.puts("  6) Progress Summary Agent")
        IO.puts("  7) Campaign limits")
        IO.puts("  8) Sync")

        case prompt("Select section", "1") do
          "1" -> {:ok, :server}
          "2" -> {:ok, :alignment_agent}
          "3" -> {:ok, :iteration_agents}
          "4" -> {:ok, :integration_agent}
          "5" -> {:ok, :integration_followup_agent}
          "6" -> {:ok, :progress_summary}
          "7" -> {:ok, :campaign_limits}
          "8" -> {:ok, :sync}
          _ -> {:error, {:invalid_reconfiguration_section, "choose 1 through 8"}}
        end
    end
  end

  defp boolean_setting(opts, key, label, default, interactive?) do
    cond do
      Keyword.has_key?(opts, key) -> {:ok, opts[key]}
      not interactive? -> {:ok, default}
      true -> parse_boolean(prompt(label, if(default, do: "Y", else: "N")))
    end
  end

  defp backend_setting(_opts, current, false, _interactive?), do: {:ok, short_backend(current)}

  defp backend_setting(opts, current, true, interactive?) do
    default = short_backend(current)

    cond do
      opts[:summary_backend] ->
        {:ok, opts[:summary_backend]}

      not interactive? ->
        {:ok, default}

      true ->
        IO.puts("\nProgress Summary Agent backend type:")
        IO.puts("  1) Codex · Codex App Server")
        IO.puts("  2) Cursor · Agent Client Protocol")

        case parse_backend_choice(
               prompt(
                 "Select Progress Summary Agent backend",
                 if(default == "cursor", do: "2", else: "1")
               )
             ) do
          {:ok, backend} -> {:ok, backend}
          {:error, {_kind, value}} -> {:error, {:invalid_summary_backend, value}}
        end
    end
  end

  defp profile_backend_setting(opts, key, current, label, interactive?) do
    default = short_backend(current)

    cond do
      Keyword.has_key?(opts, key) ->
        parse_backend_choice(opts[key])

      not interactive? ->
        {:ok, default}

      true ->
        IO.puts("\n#{label} backend type:")
        IO.puts("  1) Codex · Codex App Server")
        IO.puts("  2) Cursor · Agent Client Protocol")

        prompt("Select #{label} backend", if(default == "cursor", do: "2", else: "1"))
        |> parse_backend_choice()
    end
  end

  defp parse_backend_choice(value) when value in ~w(1 codex codex_app_server), do: {:ok, "codex"}
  defp parse_backend_choice(value) when value in ~w(2 cursor cursor_acp), do: {:ok, "cursor"}
  defp parse_backend_choice(value), do: {:error, {:invalid_agent_backend, value}}

  defp profile_model_setting(opts, key, backend, label, default, interactive?) do
    cond do
      Keyword.has_key?(opts, key) -> {:ok, empty_to_nil(opts[key])}
      not interactive? -> {:ok, default}
      true -> choose_model(opts, backend, label, default)
    end
  end

  defp choose_model(opts, backend, label, default) do
    catalog = Keyword.get(opts, :model_catalog, &ModelCatalog.list/1)
    IO.puts("Loading available #{String.capitalize(backend)} models…")

    models =
      case catalog.(config_backend(backend)) do
        {:ok, models} when is_list(models) ->
          models

        {:error, reason} ->
          IO.puts(:stderr, "Could not load provider model list: #{inspect(reason)}")
          []
      end
      |> Enum.take(15)

    IO.puts("\nAvailable #{label} models (#{String.capitalize(backend)}):")
    IO.puts("  1) Provider default (recommended)")

    Enum.with_index(models, 2)
    |> Enum.each(fn {model, index} ->
      id = model[:id] || model["id"]
      name = model[:label] || model["label"] || id
      IO.puts("  #{index}) #{id} · #{name}")
    end)

    custom_index = length(models) + 2
    IO.puts("  #{custom_index}) Custom model id")

    selected =
      prompt("Select #{label} model", Integer.to_string(model_default_index(models, default)))

    case Integer.parse(selected) do
      {1, ""} ->
        {:ok, nil}

      {index, ""} when index >= 2 and index < custom_index ->
        model = Enum.at(models, index - 2)
        {:ok, model[:id] || model["id"]}

      {^custom_index, ""} ->
        {:ok, empty_to_nil(prompt("Custom model id", default || ""))}

      _ ->
        {:error, {:invalid_model_selection, selected}}
    end
  end

  defp model_default_index(models, default) do
    case Enum.find_index(models, fn model -> (model[:id] || model["id"]) == default end) do
      nil -> 1
      index -> index + 2
    end
  end

  defp string_setting(opts, key, label, default, interactive?) do
    cond do
      Keyword.has_key?(opts, key) -> {:ok, opts[key]}
      interactive? -> {:ok, prompt(label, default || "")}
      true -> {:ok, default}
    end
  end

  defp permission_setting(opts, key, backend, kind, label, default, interactive?) do
    cond do
      Keyword.has_key?(opts, key) ->
        PermissionPolicy.parse(backend, kind, opts[key])

      not interactive? ->
        {:ok, default}

      true ->
        options = PermissionPolicy.options(backend, kind)
        default_index = (Enum.find_index(options, &(&1.value == default)) || 0) + 1
        title = if kind == :approval_policy, do: "approval policy", else: "sandbox policy"
        IO.puts("\n#{label} #{title}:")

        Enum.with_index(options, 1)
        |> Enum.each(fn {option, index} ->
          IO.puts("  #{index}) #{option.label} · #{option.description}")
        end)

        selected = prompt("Select #{label} #{title}", Integer.to_string(default_index))

        value =
          case Integer.parse(selected) do
            {index, ""} when index >= 1 and index <= length(options) ->
              Enum.at(options, index - 1).value

            _ ->
              selected
          end

        PermissionPolicy.parse(backend, kind, value)
    end
  end

  defp enum_setting(opts, key, label, default, allowed, interactive?) do
    value =
      cond do
        Keyword.has_key?(opts, key) -> opts[key]
        interactive? -> choose_enum(label, default, allowed)
        true -> default
      end

    if value in allowed, do: {:ok, value}, else: {:error, {:invalid_summary_effort, value}}
  end

  defp choose_enum(label, default, allowed) do
    default_index = (Enum.find_index(allowed, &(&1 == default)) || 0) + 1

    IO.puts("\n#{label}:")

    Enum.with_index(allowed, 1)
    |> Enum.each(fn {value, index} ->
      IO.puts("  #{index}) #{value} · #{effort_description(value)}")
    end)

    selected = prompt("Select #{label}", Integer.to_string(default_index))

    case Integer.parse(selected) do
      {index, ""} when index >= 1 and index <= length(allowed) ->
        Enum.at(allowed, index - 1)

      _ ->
        selected
    end
  end

  defp effort_description("low"), do: "Fastest responses with minimal reasoning"
  defp effort_description("medium"), do: "Balanced speed and reasoning"
  defp effort_description("high"), do: "Thorough reasoning (recommended)"
  defp effort_description("xhigh"), do: "More reasoning for difficult tasks"
  defp effort_description("max"), do: "Maximum supported reasoning"
  defp effort_description("ultra"), do: "Deepest supported reasoning"

  defp integer_setting(opts, key, label, default, interactive?) do
    value =
      if Keyword.has_key?(opts, key),
        do: opts[key],
        else: if(interactive?, do: prompt(label, default), else: default)

    case Integer.parse(to_string(value)) do
      {number, ""} when number > 0 -> {:ok, number}
      _ -> {:error, {:invalid_summary_interval, value}}
    end
  end

  defp positive_integer_setting(opts, key, label, default, interactive?),
    do: integer_setting(opts, key, label, default, interactive?)

  defp non_negative_setting(opts, key, label, default, interactive?) do
    value = setting_value(opts, key, label, default, interactive?)

    case Integer.parse(to_string(value)) do
      {number, ""} when number >= 0 -> {:ok, number}
      _ -> {:error, {:invalid_non_negative_integer, key, value}}
    end
  end

  defp optional_non_negative_setting(opts, key, label, default, interactive?) do
    shown_default = if is_nil(default), do: "", else: default
    value = setting_value(opts, key, label, shown_default, interactive?)

    case to_string(value) do
      "" ->
        {:ok, nil}

      string ->
        case Integer.parse(string) do
          {number, ""} when number >= 0 -> {:ok, number}
          _ -> {:error, {:invalid_optional_non_negative_integer, key, value}}
        end
    end
  end

  defp setting_value(opts, key, label, default, interactive?) do
    cond do
      Keyword.has_key?(opts, key) -> opts[key]
      interactive? -> prompt(label, default)
      true -> default
    end
  end

  defp parse_boolean(value) do
    case String.downcase(value) do
      value when value in ~w(y yes true 1) -> {:ok, true}
      value when value in ~w(n no false 0) -> {:ok, false}
      value -> {:error, {:invalid_boolean, value}}
    end
  end

  defp prompt(label, default) do
    suffix = if to_string(default) == "", do: "", else: " [#{default}]"
    value = IO.gets("#{label}#{suffix}: ") |> to_string() |> String.trim()
    if value == "", do: to_string(default), else: value
  end

  defp render_summary(settings) do
    permissions = PermissionPolicy.defaults(settings.backend)
    model = if settings.model, do: "\n    model: #{Jason.encode!(settings.model)}", else: ""

    """
      progress_summary:
        enabled: #{settings.enabled}
        interval_minutes: #{settings.interval_minutes}
        name: progress-summary
        backend: #{config_backend(settings.backend)}#{model}
        reasoning_effort: #{settings.effort}
        approval_policy: #{permissions.approval_policy}
        sandbox_policy: #{summary_sandbox(settings.backend)}
    """
    |> String.trim_trailing()
  end

  defp replace_progress_summary(source, replacement) do
    pattern = ~r/^  progress_summary:\n(?:^(?: {4,}.*|\s*)\n)*/m

    if Regex.match?(pattern, source) do
      {:ok, Regex.replace(pattern, source, replacement <> "\n", global: false)}
    else
      {:error, :progress_summary_section_not_found}
    end
  end

  defp replace_section(source, :progress_summary, settings),
    do: replace_progress_summary(source, render_summary(settings))

  defp replace_section(source, :server, settings) do
    replace_yaml_section(
      source,
      "server",
      """
      server:
        host: #{Jason.encode!(settings.host)}
        port: #{settings.port}
      """
    )
  end

  defp replace_section(source, :alignment_agent, settings) do
    model = optional_model(settings.model, 2)

    replace_yaml_section(
      source,
      "backend",
      """
      backend:
        type: #{config_backend(settings.backend)}#{model}
        reasoning_effort: #{settings.effort}
        approval_policy: #{settings.approval_policy}
        sandbox_policy: #{settings.sandbox_policy}
        protocol_config: {}
      """
    )
  end

  defp replace_section(source, :iteration_agents, settings) do
    model = optional_model(settings.model, 6)

    agents =
      1..settings.count
      |> Enum.map_join("\n", fn index ->
        "    - name: #{settings.backend}-#{index}\n" <>
          "      backend: #{config_backend(settings.backend)}#{model}\n" <>
          "      reasoning_effort: #{settings.effort}\n" <>
          "      approval_policy: #{settings.approval_policy}\n" <>
          "      sandbox_policy: #{settings.sandbox_policy}"
      end)

    replace_campaign_section(source, "iteration_agents", "  iteration_agents:\n#{agents}")
  end

  defp replace_section(source, :integration_agent, settings),
    do:
      replace_campaign_section(
        source,
        "integration_agent",
        render_agent_profile("integration_agent", "integration", settings)
      )

  defp replace_section(source, :campaign_limits, settings) do
    max_attempts = if is_nil(settings.max_attempts), do: "null", else: settings.max_attempts

    with {:ok, source} <- replace_campaign_scalar(source, "max_attempts", max_attempts),
         {:ok, source} <-
           replace_campaign_scalar(
             source,
             "max_unverified_attempts",
             settings.max_unverified_attempts
           ) do
      {:ok, source}
    end
  end

  defp replace_section(source, :sync, settings) do
    if not settings.enabled do
      {:ok, Regex.replace(~r/^sync:\n(?:^(?: {2,}.*|\s*)\n)*/m, source, "", global: false)}
    else
      rendered =
        """
        sync:
          remote: #{Jason.encode!(settings.remote)}
          branch: #{Jason.encode!(settings.branch)}
        """

      if Regex.match?(~r/^sync:\n(?:^(?: {2,}.*|\s*)\n)*/m, source),
        do: replace_yaml_section(source, "sync", rendered),
        else:
          {:ok, String.trim_trailing(source) <> "\n\n" <> String.trim_trailing(rendered) <> "\n"}
    end
  end

  defp replace_section(source, :integration_followup_agent, settings) do
    replacement = render_followup(settings)
    pattern = ~r/^  integration_followup_agent:\n(?:^(?: {4,}.*|\s*)\n)*/m

    if Regex.match?(pattern, source),
      do: {:ok, Regex.replace(pattern, source, replacement <> "\n", global: false)},
      else: insert_after_integration(source, replacement)
  end

  defp render_followup(settings) do
    model = if settings.model, do: "\n    model: #{Jason.encode!(settings.model)}", else: ""

    """
      integration_followup_agent:
        name: integration-followup
        backend: #{config_backend(settings.backend)}#{model}
        reasoning_effort: #{settings.effort}
        approval_policy: #{settings.approval_policy}
        sandbox_policy: #{settings.sandbox_policy}
    """
    |> String.trim_trailing()
  end

  defp render_agent_profile(key, name, settings) do
    model = optional_model(settings.model, 4)

    """
      #{key}:
        name: #{name}
        backend: #{config_backend(settings.backend)}#{model}
        reasoning_effort: #{settings.effort}
        approval_policy: #{settings.approval_policy}
        sandbox_policy: #{settings.sandbox_policy}
    """
    |> String.trim_trailing()
  end

  defp optional_model(nil, _indent), do: ""

  defp optional_model(model, indent),
    do: "\n#{String.duplicate(" ", indent)}model: #{Jason.encode!(model)}"

  defp replace_yaml_section(source, key, rendered) do
    pattern = Regex.compile!("^#{Regex.escape(key)}:\\n(?:^(?: {2,}.*|\\s*)\\n)*", "m")

    if Regex.match?(pattern, source),
      do:
        {:ok,
         Regex.replace(pattern, source, String.trim_trailing(rendered) <> "\n", global: false)},
      else: {:error, {:configuration_section_not_found, key}}
  end

  defp replace_campaign_section(source, key, rendered) do
    pattern = Regex.compile!("^  #{Regex.escape(key)}:\\n(?:^(?: {4,}.*|\\s*)\\n)*", "m")

    if Regex.match?(pattern, source),
      do:
        {:ok,
         Regex.replace(pattern, source, String.trim_trailing(rendered) <> "\n", global: false)},
      else: {:error, {:configuration_section_not_found, key}}
  end

  defp replace_campaign_scalar(source, key, value) do
    pattern = Regex.compile!("^  #{Regex.escape(key)}:.*$", "m")

    if Regex.match?(pattern, source),
      do: {:ok, Regex.replace(pattern, source, "  #{key}: #{value}", global: false)},
      else: {:error, {:configuration_field_not_found, key}}
  end

  defp insert_after_integration(source, replacement) do
    pattern = ~r/(^  integration_agent:\n(?:^(?: {4,}.*|\s*)\n)*)/m

    if Regex.match?(pattern, source),
      do: {:ok, Regex.replace(pattern, source, "\\1" <> replacement <> "\n", global: false)},
      else: {:error, :integration_agent_section_not_found}
  end

  defp validate_candidate(contents, config, opts) do
    candidate =
      Path.join(
        Path.dirname(opts[:config]),
        ".#{Path.basename(opts[:config])}.reconfiguration-candidate"
      )

    try do
      with :ok <- FileSystem.atomic_write(candidate, contents),
           {:ok, _candidate_config} <-
             Config.load(candidate, workspace: opts[:workspace], repo: config.repo) do
        :ok
      end
    after
      File.rm(candidate)
    end
  end

  defp short_backend("cursor_acp"), do: "cursor"
  defp short_backend("cursor"), do: "cursor"
  defp short_backend(_), do: "codex"
  defp empty_to_nil(""), do: nil
  defp empty_to_nil(value), do: value
  defp summary_sandbox("cursor"), do: "enabled"
  defp summary_sandbox(_), do: "read_only"
  defp config_backend("cursor"), do: "cursor_acp"
  defp config_backend(_), do: "codex_app_server"
end
