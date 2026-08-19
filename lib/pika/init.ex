defmodule Pika.Init do
  @moduledoc false

  alias Pika.{Config, FileSystem, ModelCatalog, Paths, Workspace, WorkspaceLock}

  @efforts ~w(low medium high xhigh max ultra)
  @model_display_limit 15
  @prompt_templates [
    alignment: {"alignment/alignment.md.eex", "prompts/alignment/alignment.md.eex"},
    setup_merge: {"alignment/setup_merge.md.eex", "prompts/alignment/setup_merge.md.eex"},
    baseline: {"alignment/baseline.md.eex", "prompts/alignment/baseline.md.eex"},
    plan: {"attempt/plan.md.eex", "prompts/attempt/plan.md.eex"},
    iteration: {"attempt/iteration.md.eex", "prompts/attempt/iteration.md.eex"},
    integration: {"integration/integration.md.eex", "prompts/integration/integration.md.eex"},
    sync: {"sync/sync.md.eex", "prompts/sync/sync.md.eex"}
  ]

  def run(opts) when is_list(opts) do
    IO.puts("Pika Workspace initializer")

    with {:ok, settings} <- collect(opts),
         :ok <- validate_locations(settings),
         yaml <- render_config(settings),
         {:ok, workspace} <- initialize(settings, yaml) do
      result = %{
        workspace: workspace,
        config_path: settings.config_path,
        repo: settings.repo,
        mode: settings.mode
      }

      print_success(result)
      {:ok, result}
    end
  end

  def render_config(settings) when is_map(settings) do
    agents =
      1..settings.iteration_agents
      |> Enum.map_join("\n", fn index ->
        model =
          if settings.iteration_model,
            do: "\n      model: #{yaml_string(settings.iteration_model)}",
            else: ""

        """
            - name: #{yaml_string("#{backend_label(settings.iteration_backend)}-#{index}")}
              backend: #{settings.iteration_backend}#{model}
              reasoning_effort: #{settings.iteration_effort}
        """
        |> String.trim_trailing()
      end)

    alignment_model =
      if settings.alignment_model,
        do: "\n  model: #{yaml_string(settings.alignment_model)}",
        else: ""

    sync =
      case settings.sync do
        nil ->
          ""

        %{remote: remote, branch: branch} ->
          """

          sync:
            remote: #{yaml_string(remote)}
            branch: #{yaml_string(branch)}
          """
      end

    max_attempts = if is_nil(settings.max_attempts), do: "null", else: settings.max_attempts

    prompts =
      Enum.map_join(@prompt_templates, "\n", fn {kind, {_source, destination}} ->
        "  #{kind}: #{yaml_string(Path.join(settings.workspace, destination))}"
      end)

    """
    server:
      host: #{yaml_string(settings.host)}
      port: #{settings.port}

    # Alignment and Baseline Boundary Agent backend.
    backend:
      type: #{settings.alignment_backend}#{alignment_model}
      reasoning_effort: #{settings.alignment_effort}
      protocol_config: {}

    prompts:
    #{prompts}

    campaign:
      plan: false
      max_attempts: #{max_attempts}
      history_n: 10
      iteration_agents:
    #{agents}
      reference_catalog: []
      stop_conditions:
        mode: all_goals
    #{sync}
    """
    |> String.trim()
    |> Kernel.<>("\n")
  end

  defp collect(opts) do
    cwd = System.get_env("PIKA_CLI_CWD") || File.cwd!()

    with {:ok, {mode, repo}} <- collect_repo(opts, cwd),
         workspace_default <- default_workspace(mode, repo, cwd),
         {:ok, workspace} <-
           choose(opts, :workspace, "Workspace path", workspace_default, &non_empty_path(&1, cwd)),
         {:ok, workspace} <- Paths.canonical(workspace),
         config_default <- Path.join(workspace, "pika.yaml"),
         {:ok, config_path} <-
           choose(opts, :config, "Configuration file", config_default, &non_empty_path(&1, cwd)),
         {:ok, config_path} <- Paths.canonical(config_path),
         {:ok, host} <- choose(opts, :host, "Listen host", "127.0.0.1", &parse_host/1),
         {:ok, port} <- choose(opts, :port, "Listen port", 8080, &parse_port/1),
         {:ok, alignment_backend} <-
           collect_backend(
             inherit_backend(opts, :alignment_backend),
             :alignment_backend,
             "Alignment/Baseline Agent",
             "codex"
           ),
         {:ok, alignment_model, model_cache} <-
           collect_model(
             opts,
             :alignment_model,
             alignment_backend,
             "Alignment/Baseline Agent",
             %{}
           ),
         {:ok, alignment_effort} <-
           choose(
             opts,
             :alignment_effort,
             "Alignment/Baseline Agent reasoning effort",
             "high",
             &parse_effort/1
           ),
         {:ok, iteration_backend} <-
           collect_backend(
             inherit_backend(opts, :iteration_backend),
             :iteration_backend,
             "Iteration Agent",
             backend_label(alignment_backend)
           ),
         iteration_opts <- inherit_option(opts, :iteration_model, :model),
         {:ok, iteration_model, _model_cache} <-
           collect_model(
             iteration_opts,
             :iteration_model,
             iteration_backend,
             "Iteration Agent",
             model_cache
           ),
         effort_opts <- inherit_option(opts, :iteration_effort, :effort),
         {:ok, iteration_effort} <-
           choose(
             effort_opts,
             :iteration_effort,
             "Iteration Agent reasoning effort",
             "high",
             &parse_effort/1
           ),
         {:ok, iteration_agents} <-
           choose(
             opts,
             :iteration_agents,
             "Concurrent Iteration Agents",
             1,
             &parse_positive_integer/1
           ),
         {:ok, max_attempts} <-
           choose(
             opts,
             :max_attempts,
             "Maximum attempts (blank for unlimited)",
             "",
             &parse_optional_non_negative_integer/1
           ),
         {:ok, sync} <- collect_sync(opts, mode, repo) do
      {:ok,
       %{
         workspace: workspace,
         config_path: config_path,
         mode: mode,
         repo: repo,
         host: host,
         port: port,
         alignment_backend: alignment_backend,
         alignment_model: alignment_model,
         alignment_effort: alignment_effort,
         iteration_backend: iteration_backend,
         iteration_model: iteration_model,
         iteration_effort: iteration_effort,
         iteration_agents: iteration_agents,
         max_attempts: max_attempts,
         sync: sync
       }}
    end
  end

  defp collect_repo(opts, cwd) do
    detected = git_root(cwd)
    default_mode = if detected, do: "managed", else: "owned"

    mode_result =
      cond do
        Keyword.has_key?(opts, :repo) -> {:ok, :managed_repo}
        opts[:owned] -> {:ok, :owned_repo}
        opts[:yes] -> parse_repo_mode(default_mode)
        true -> ask_valid("Repository mode (managed/owned)", default_mode, &parse_repo_mode/1)
      end

    with {:ok, mode} <- mode_result do
      case mode do
        :owned_repo ->
          {:ok, {mode, nil}}

        :managed_repo ->
          default = detected || cwd

          with {:ok, repo} <-
                 choose(
                   opts,
                   :repo,
                   "Git repository to manage",
                   default,
                   &non_empty_path(&1, cwd)
                 ),
               {:ok, repo} <- Paths.canonical(repo) do
            {:ok, {mode, repo}}
          end
      end
    end
  end

  defp collect_sync(opts, mode, repo) do
    requested =
      cond do
        opts[:no_sync] ->
          {:ok, false}

        Keyword.has_key?(opts, :sync_remote) or Keyword.has_key?(opts, :sync_branch) ->
          {:ok, true}

        opts[:yes] ->
          {:ok, false}

        true ->
          ask_valid("Configure Git sync? (y/N)", "n", &parse_boolean/1)
      end

    with {:ok, requested} <- requested do
      if requested do
        branch_default = current_branch(mode, repo) || "main"

        with {:ok, remote} <-
               choose(opts, :sync_remote, "Sync remote", "origin", &parse_non_empty_string/1),
             {:ok, branch} <-
               choose(
                 opts,
                 :sync_branch,
                 "Sync branch",
                 branch_default,
                 &parse_non_empty_string/1
               ) do
          {:ok, %{remote: remote, branch: branch}}
        end
      else
        {:ok, nil}
      end
    end
  end

  defp collect_backend(opts, key, agent, default) do
    cond do
      Keyword.has_key?(opts, key) -> parse_backend(opts[key])
      opts[:yes] -> parse_backend(default)
      true -> ask_backend(agent, default)
    end
  end

  defp ask_backend(agent, default) do
    default_index = if backend_label(default) == "cursor", do: "2", else: "1"

    IO.puts("\n#{agent} backend type:")
    IO.puts("  1) Codex · Codex App Server")
    IO.puts("  2) Cursor · Agent Client Protocol")

    value =
      case IO.gets("Select #{agent} backend [#{default_index}]: ") do
        nil -> default_index
        input -> input |> String.trim() |> use_default(default_index)
      end

    case String.downcase(value) do
      choice when choice in ["1", "codex", "codex_app_server"] ->
        {:ok, "codex_app_server"}

      choice when choice in ["2", "cursor", "cursor_acp"] ->
        {:ok, "cursor_acp"}

      _ ->
        IO.puts(:stderr, "Invalid value: choose 1 for Codex or 2 for Cursor")
        ask_backend(agent, default)
    end
  end

  defp collect_model(opts, key, backend, agent, cache) do
    cond do
      Keyword.has_key?(opts, key) ->
        with {:ok, model} <- parse_optional_string(opts[key]), do: {:ok, model, cache}

      opts[:yes] ->
        {:ok, nil, cache}

      true ->
        with {:ok, models, cache} <- load_models(opts, backend, cache),
             {:ok, model} <- ask_model(agent, backend, models) do
          {:ok, model, cache}
        end
    end
  end

  defp load_models(opts, backend, cache) do
    case Map.fetch(cache, backend) do
      {:ok, models} ->
        {:ok, models, cache}

      :error ->
        catalog = Keyword.get(opts, :model_catalog, &ModelCatalog.list/1)
        IO.puts("Loading available #{backend_label(backend)} models…")

        models =
          case catalog.(backend) do
            {:ok, models} when is_list(models) ->
              models

            {:error, reason} ->
              IO.puts(:stderr, "Could not load provider model list: #{inspect(reason)}")
              []
          end

        {:ok, models, Map.put(cache, backend, models)}
    end
  end

  defp ask_model(agent, backend, models) do
    shown = Enum.take(models, @model_display_limit)
    custom_index = length(shown) + 2

    IO.puts("\nAvailable #{agent} models (#{backend_label(backend)}):")
    IO.puts("  1) Provider default (recommended)")

    Enum.with_index(shown, 2)
    |> Enum.each(fn {model, index} ->
      label = model[:label] || model.id
      description = compact_description(model[:description])
      suffix = if description, do: " — #{description}", else: ""
      IO.puts("  #{index}) #{model.id} · #{label}#{suffix}")
    end)

    hidden_count = max(length(models) - length(shown), 0)

    if hidden_count > 0 do
      IO.puts(
        "  … #{hidden_count} more provider models; use Custom model id for any unlisted model"
      )
    end

    IO.puts("  #{custom_index}) Custom model id")

    selection =
      case IO.gets("Select model [1]: ") do
        nil -> "1"
        input -> String.trim(input) |> use_default("1")
      end

    case Integer.parse(selection) do
      {1, ""} ->
        {:ok, nil}

      {index, ""} when index >= 2 and index < custom_index ->
        {:ok, Enum.at(shown, index - 2).id}

      {^custom_index, ""} ->
        ask_valid("Custom model id", "", &parse_non_empty_string/1)

      _ ->
        IO.puts(:stderr, "Invalid value: choose a listed number")
        ask_model(agent, backend, models)
    end
  end

  defp compact_description(nil), do: nil

  defp compact_description(description) when is_binary(description) do
    description
    |> String.replace(~r/\s+/, " ")
    |> String.trim()
    |> String.slice(0, 96)
    |> empty_to_nil()
  end

  defp compact_description(_description), do: nil

  defp choose(opts, key, label, default, parser) do
    cond do
      Keyword.has_key?(opts, key) -> parser.(opts[key])
      opts[:yes] -> parser.(default)
      true -> ask_valid(label, default, parser)
    end
  end

  defp inherit_backend(opts, key) do
    if Keyword.has_key?(opts, key) or not Keyword.has_key?(opts, :backend),
      do: opts,
      else: Keyword.put(opts, key, opts[:backend])
  end

  defp inherit_option(opts, key, legacy_key) do
    if Keyword.has_key?(opts, key) or not Keyword.has_key?(opts, legacy_key),
      do: opts,
      else: Keyword.put(opts, key, opts[legacy_key])
  end

  defp ask_valid(label, default, parser) do
    suffix = if default in [nil, ""], do: "", else: " [#{default}]"

    value =
      case IO.gets("#{label}#{suffix}: ") do
        nil -> default
        input -> input |> String.trim() |> use_default(default)
      end

    case parser.(value) do
      {:ok, parsed} ->
        {:ok, parsed}

      {:error, message} ->
        IO.puts(:stderr, "Invalid value: #{message}")
        ask_valid(label, default, parser)
    end
  end

  defp use_default("", default), do: default
  defp use_default(value, _default), do: value

  defp validate_locations(settings) do
    cond do
      File.exists?(settings.config_path) ->
        {:error, {:config_already_exists, settings.config_path}}

      settings.mode == :managed_repo and within?(settings.workspace, settings.repo) ->
        {:error, {:workspace_inside_managed_repo, settings.workspace, settings.repo}}

      settings.mode == :managed_repo and within?(settings.config_path, settings.repo) ->
        {:error, {:config_inside_managed_repo, settings.config_path, settings.repo}}

      within?(settings.config_path, settings.workspace) and
          Path.dirname(settings.config_path) != settings.workspace ->
        {:error, {:config_must_be_at_workspace_root, settings.config_path}}

      true ->
        :ok
    end
  end

  defp initialize(settings, yaml) do
    with {:ok, templates} <- load_prompt_templates(),
         {:ok, temporary} <- write_temporary_config(yaml) do
      try do
        with {:ok, initial_config} <- Config.load(temporary, config_opts(settings)),
             {:ok, initial_plan} <- Workspace.plan(initial_config),
             :ok <- require_fresh(initial_plan),
             :ok <- write_config(settings.config_path, yaml),
             {:ok, config} <- Config.load(settings.config_path, config_opts(settings)),
             {:ok, plan} <- Workspace.plan(config),
             :ok <- require_fresh(plan),
             {:ok, workspace} <- activate(plan, templates) do
          {:ok, workspace}
        end
      after
        _ = File.rm(temporary)
      end
    end
  end

  defp write_temporary_config(yaml) do
    suffix = :crypto.strong_rand_bytes(12) |> Base.url_encode64(padding: false)
    path = Path.join(System.tmp_dir!(), "pika-init-#{suffix}.yaml")

    case FileSystem.atomic_write(path, yaml) do
      :ok -> {:ok, path}
      {:error, reason} -> {:error, {:temporary_config_write_failed, reason}}
    end
  end

  defp write_config(path, yaml) do
    case FileSystem.atomic_write(path, yaml) do
      :ok -> :ok
      {:error, reason} -> {:error, {:config_write_failed, path, reason}}
    end
  end

  defp require_fresh(%{fresh?: true}), do: :ok

  defp require_fresh(%{config: config}),
    do: {:error, {:workspace_already_initialized, config.workspace}}

  defp activate(plan, templates) do
    case WorkspaceLock.start({plan, []}) do
      {:ok, lock} ->
        try do
          workspace = WorkspaceLock.workspace(lock)

          with :ok <- touch_database(workspace),
               :ok <- install_prompt_templates(workspace, templates) do
            {:ok, workspace}
          end
        after
          if Process.alive?(lock), do: GenServer.stop(lock)
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp load_prompt_templates do
    Enum.reduce_while(@prompt_templates, {:ok, []}, fn {kind, {source, destination}},
                                                       {:ok, templates} ->
      path = Application.app_dir(:pika, Path.join("priv/prompts", source))

      case File.read(path) do
        {:ok, contents} ->
          template = %{kind: kind, destination: destination, contents: contents}
          {:cont, {:ok, [template | templates]}}

        {:error, reason} ->
          {:halt, {:error, {:prompt_template_read_failed, kind, path, reason}}}
      end
    end)
    |> case do
      {:ok, templates} -> {:ok, Enum.reverse(templates)}
      {:error, _reason} = error -> error
    end
  end

  defp install_prompt_templates(workspace, templates) do
    Enum.reduce_while(templates, :ok, fn template, :ok ->
      path = Path.join(workspace.root, template.destination)

      case FileSystem.atomic_write(path, template.contents) do
        :ok ->
          {:cont, :ok}

        {:error, reason} ->
          {:halt, {:error, {:prompt_template_write_failed, template.kind, path, reason}}}
      end
    end)
  end

  defp touch_database(workspace) do
    case File.touch(workspace.database) do
      :ok -> :ok
      {:error, reason} -> {:error, {:database_create_failed, workspace.database, reason}}
    end
  end

  defp config_opts(settings) do
    [workspace: settings.workspace]
    |> then(fn opts ->
      if settings.repo, do: Keyword.put(opts, :repo, settings.repo), else: opts
    end)
  end

  defp print_success(result) do
    command = start_command(result)

    IO.puts("""

    Initialized Pika Workspace: #{result.workspace.root}
    Repo mode: #{result.mode}
    Configuration: #{result.config_path}

    Start Pika with:
      #{command}
    """)
  end

  defp start_command(result) do
    if result.config_path == Path.join(result.workspace.root, "pika.yaml") do
      "cd #{shell_quote(result.workspace.root)}\n  pika serve"
    else
      ["pika", "serve", "--workspace", result.workspace.root, "--config", result.config_path]
      |> Enum.map_join(" ", &shell_quote/1)
    end
  end

  defp default_workspace(:managed_repo, repo, _cwd),
    do: Path.join([Path.dirname(repo), ".pika-workspaces", Path.basename(repo)])

  defp default_workspace(:owned_repo, _repo, cwd), do: Path.join(cwd, "pika-workspace")

  defp git_root(cwd) do
    case System.cmd("git", ["rev-parse", "--show-toplevel"], cd: cwd, stderr_to_stdout: true) do
      {path, 0} -> path |> String.trim() |> Path.expand()
      _ -> nil
    end
  rescue
    ErlangError -> nil
  end

  defp current_branch(:managed_repo, repo) do
    case System.cmd("git", ["branch", "--show-current"], cd: repo, stderr_to_stdout: true) do
      {branch, 0} -> branch |> String.trim() |> empty_to_nil()
      _ -> nil
    end
  rescue
    ErlangError -> nil
  end

  defp current_branch(_mode, _repo), do: nil

  defp empty_to_nil(""), do: nil
  defp empty_to_nil(value), do: value

  defp within?(path, root) do
    relative = Path.relative_to(path, root)

    relative == "." or
      (Path.type(relative) == :relative and relative != ".." and
         not String.starts_with?(relative, "../"))
  end

  defp parse_repo_mode(value) when is_binary(value) do
    case value |> String.trim() |> String.downcase() do
      mode when mode in ["managed", "m"] -> {:ok, :managed_repo}
      mode when mode in ["owned", "o"] -> {:ok, :owned_repo}
      _ -> {:error, "expected managed or owned"}
    end
  end

  defp parse_backend(value) when is_binary(value) do
    case value |> String.trim() |> String.downcase() do
      "codex" -> {:ok, "codex_app_server"}
      "codex_app_server" -> {:ok, "codex_app_server"}
      "cursor" -> {:ok, "cursor_acp"}
      "cursor_acp" -> {:ok, "cursor_acp"}
      _ -> {:error, "expected codex or cursor"}
    end
  end

  defp parse_backend(_value), do: {:error, "expected codex or cursor"}

  defp parse_effort(value) when is_atom(value), do: parse_effort(Atom.to_string(value))

  defp parse_effort(value) when is_binary(value) do
    normalized = value |> String.trim() |> String.downcase()

    if normalized in @efforts,
      do: {:ok, normalized},
      else: {:error, "expected #{Enum.join(@efforts, ", ")}"}
  end

  defp parse_effort(_value), do: {:error, "invalid reasoning effort"}

  defp parse_host(value) when is_binary(value) do
    normalized = String.trim(value)

    case :inet.parse_address(String.to_charlist(normalized)) do
      {:ok, _address} -> {:ok, normalized}
      _ -> {:error, "expected a numeric IPv4 or IPv6 address"}
    end
  end

  defp parse_host(_value), do: {:error, "expected a numeric IPv4 or IPv6 address"}

  defp parse_port(value),
    do: parse_integer(value, 1..65_535, "expected an integer from 1 through 65535")

  defp parse_positive_integer(value),
    do: parse_integer(value, 1..1_000_000, "expected a positive integer")

  defp parse_optional_non_negative_integer(value) when value in [nil, ""], do: {:ok, nil}

  defp parse_optional_non_negative_integer(value),
    do: parse_integer(value, 0..1_000_000_000, "expected a non-negative integer or blank")

  defp parse_integer(value, range, message) when is_integer(value) do
    if value in range, do: {:ok, value}, else: {:error, message}
  end

  defp parse_integer(value, range, message) when is_binary(value) do
    case Integer.parse(String.trim(value)) do
      {integer, ""} -> if integer in range, do: {:ok, integer}, else: {:error, message}
      _ -> {:error, message}
    end
  end

  defp parse_integer(_value, _range, message), do: {:error, message}

  defp parse_optional_string(value) when value in [nil, ""], do: {:ok, nil}
  defp parse_optional_string(value), do: parse_non_empty_string(value)

  defp parse_non_empty_string(value) when is_binary(value) do
    case String.trim(value) do
      "" -> {:error, "expected a non-empty value"}
      normalized -> {:ok, normalized}
    end
  end

  defp parse_non_empty_string(_value), do: {:error, "expected a non-empty value"}

  defp non_empty_path(value, cwd) do
    with {:ok, path} <- parse_non_empty_string(value) do
      {:ok, Path.expand(path, cwd)}
    end
  end

  defp parse_boolean(value) when is_boolean(value), do: {:ok, value}

  defp parse_boolean(value) when is_binary(value) do
    case value |> String.trim() |> String.downcase() do
      answer when answer in ["y", "yes"] -> {:ok, true}
      answer when answer in ["n", "no"] -> {:ok, false}
      _ -> {:error, "expected yes or no"}
    end
  end

  defp parse_boolean(_value), do: {:error, "expected yes or no"}

  defp backend_label(backend) when backend in ["cursor", "cursor_acp"], do: "cursor"
  defp backend_label(_backend), do: "codex"

  defp yaml_string(value), do: Jason.encode!(value)

  defp shell_quote(value) do
    if Regex.match?(~r/^[A-Za-z0-9_@%+=:,\.\/-]+$/, value) do
      value
    else
      "'" <> String.replace(value, "'", "'\"'\"'") <> "'"
    end
  end
end
