defmodule Pika.CLI do
  @moduledoc false

  alias Pika.PreviewAuth, as: Auth
  alias Pika.Alignment.{Campaign, Workspace}
  alias Pika.AgentBackend.PermissionPolicy

  def main(["init" | argv]) do
    case parse_init(argv) do
      {:ok, opts} ->
        if opts[:help] do
          IO.puts(init_usage())
        else
          case Pika.Init.run(opts) do
            {:ok, _result} -> :ok
            {:error, reason} -> abort(format_error("init", reason))
          end
        end

      {:error, message} ->
        abort(message <> "\n\n" <> init_usage())
    end
  end

  def main(["serve" | argv]) do
    case parse_serve(argv) do
      {:ok, opts} -> run_serve(opts)
      {:error, message} -> abort(message)
    end
  end

  def main(["preview", repo | argv]) do
    case parse_preview(argv) do
      {:ok, opts} -> run_preview(repo, opts)
      {:error, message} -> abort(message)
    end
  end

  def main(_argv), do: abort(usage())

  def parse_init(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          workspace: :string,
          repo: :string,
          owned: :boolean,
          config: :string,
          host: :string,
          port: :integer,
          backend: :string,
          alignment_backend: :string,
          alignment_model: :string,
          alignment_effort: :string,
          alignment_approval_policy: :string,
          alignment_sandbox_policy: :string,
          iteration_backend: :string,
          iteration_model: :string,
          iteration_effort: :string,
          iteration_approval_policy: :string,
          iteration_sandbox_policy: :string,
          integration_backend: :string,
          integration_model: :string,
          integration_effort: :string,
          integration_approval_policy: :string,
          integration_sandbox_policy: :string,
          progress_summary: :boolean,
          summary_backend: :string,
          summary_model: :string,
          summary_effort: :string,
          summary_interval_minutes: :integer,
          model: :string,
          effort: :string,
          iteration_agents: :integer,
          max_attempts: :integer,
          max_unverified_attempts: :integer,
          sync_remote: :string,
          sync_branch: :string,
          no_sync: :boolean,
          yes: :boolean,
          help: :boolean
        ],
        aliases: [y: :yes, h: :help]
      )

    positional_workspace = if length(args) == 1, do: List.first(args)
    alignment_backend = opts[:alignment_backend] || opts[:backend] || "codex"
    iteration_backend = opts[:iteration_backend] || opts[:backend] || alignment_backend
    integration_backend = opts[:integration_backend] || opts[:backend] || alignment_backend

    errors =
      []
      |> maybe_cli_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_cli_error(length(args) > 1, "expected at most one WORKSPACE argument")
      |> maybe_cli_error(
        not is_nil(positional_workspace) and not is_nil(opts[:workspace]),
        "WORKSPACE cannot be passed both positionally and with --workspace"
      )
      |> maybe_cli_error(
        opts[:owned] == true and not is_nil(opts[:repo]),
        "--owned and --repo cannot be combined"
      )
      |> maybe_cli_error(
        opts[:no_sync] == true and
          (not is_nil(opts[:sync_remote]) or not is_nil(opts[:sync_branch])),
        "--no-sync cannot be combined with --sync-remote or --sync-branch"
      )
      |> maybe_cli_error(
        not is_nil(opts[:backend]) and opts[:backend] not in ~w(codex cursor),
        "--backend must be codex or cursor"
      )
      |> maybe_cli_error(
        not is_nil(opts[:alignment_backend]) and
          opts[:alignment_backend] not in ~w(codex cursor),
        "--alignment-backend must be codex or cursor"
      )
      |> maybe_cli_error(
        not is_nil(opts[:iteration_backend]) and
          opts[:iteration_backend] not in ~w(codex cursor),
        "--iteration-backend must be codex or cursor"
      )
      |> maybe_cli_error(
        not is_nil(opts[:integration_backend]) and
          opts[:integration_backend] not in ~w(codex cursor),
        "--integration-backend must be codex or cursor"
      )
      |> maybe_cli_error(
        not is_nil(opts[:summary_backend]) and opts[:summary_backend] not in ~w(codex cursor),
        "--summary-backend must be codex or cursor"
      )
      |> maybe_cli_error(
        not is_nil(opts[:effort]) and opts[:effort] not in ~w(low medium high xhigh max ultra),
        "invalid --effort"
      )
      |> maybe_cli_error(
        not is_nil(opts[:alignment_effort]) and
          opts[:alignment_effort] not in ~w(low medium high xhigh max ultra),
        "invalid --alignment-effort"
      )
      |> maybe_cli_error(
        not is_nil(opts[:iteration_effort]) and
          opts[:iteration_effort] not in ~w(low medium high xhigh max ultra),
        "invalid --iteration-effort"
      )
      |> maybe_cli_error(
        not is_nil(opts[:integration_effort]) and
          opts[:integration_effort] not in ~w(low medium high xhigh max ultra),
        "invalid --integration-effort"
      )
      |> maybe_cli_error(
        not is_nil(opts[:summary_effort]) and
          opts[:summary_effort] not in ~w(low medium high xhigh max ultra),
        "invalid --summary-effort"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          alignment_backend,
          :approval_policy,
          opts[:alignment_approval_policy]
        ),
        "invalid --alignment-approval-policy for #{alignment_backend}"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          alignment_backend,
          :sandbox_policy,
          opts[:alignment_sandbox_policy]
        ),
        "invalid --alignment-sandbox-policy for #{alignment_backend}"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          iteration_backend,
          :approval_policy,
          opts[:iteration_approval_policy]
        ),
        "invalid --iteration-approval-policy for #{iteration_backend}"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          iteration_backend,
          :sandbox_policy,
          opts[:iteration_sandbox_policy]
        ),
        "invalid --iteration-sandbox-policy for #{iteration_backend}"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          integration_backend,
          :approval_policy,
          opts[:integration_approval_policy]
        ),
        "invalid --integration-approval-policy for #{integration_backend}"
      )
      |> maybe_cli_error(
        not valid_init_permission?(
          integration_backend,
          :sandbox_policy,
          opts[:integration_sandbox_policy]
        ),
        "invalid --integration-sandbox-policy for #{integration_backend}"
      )
      |> maybe_cli_error(
        not is_nil(opts[:port]) and opts[:port] not in 1..65_535,
        "--port must be from 1 through 65535"
      )
      |> maybe_cli_error(
        not is_nil(opts[:iteration_agents]) and opts[:iteration_agents] < 1,
        "--iteration-agents must be positive"
      )
      |> maybe_cli_error(
        not is_nil(opts[:max_attempts]) and opts[:max_attempts] < 0,
        "--max-attempts must be non-negative"
      )
      |> maybe_cli_error(
        not is_nil(opts[:max_unverified_attempts]) and opts[:max_unverified_attempts] < 0,
        "--max-unverified-attempts must be non-negative"
      )
      |> maybe_cli_error(
        not is_nil(opts[:summary_interval_minutes]) and opts[:summary_interval_minutes] < 1,
        "--summary-interval-minutes must be positive"
      )

    case errors do
      [] ->
        workspace = opts[:workspace] || positional_workspace
        {:ok, if(workspace, do: Keyword.put(opts, :workspace, workspace), else: opts)}

      values ->
        {:error, Enum.join(values, "\n")}
    end
  end

  def parse_serve(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          workspace: :string,
          repo: :string,
          config: :string,
          host: :string,
          port: :integer
        ]
      )

    cwd = invocation_cwd()
    workspace = expand_from(opts[:workspace], cwd) || discover_workspace(cwd)
    config = expand_from(opts[:config], cwd) || default_workspace_config(workspace)
    repo = expand_from(opts[:repo], cwd)

    errors =
      []
      |> maybe_cli_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_cli_error(args != [], "unexpected arguments: #{inspect(args)}")
      |> maybe_cli_error(
        is_nil(workspace),
        "--workspace is required when not running inside a Pika Workspace"
      )
      |> maybe_cli_error(
        is_nil(config),
        "--config is required when WORKSPACE/pika.yaml does not exist"
      )

    case errors do
      [] ->
        {:ok,
         opts
         |> Keyword.put(:workspace, workspace)
         |> Keyword.put(:config, config)
         |> put_if_present(:repo, repo)}

      values ->
        {:error, Enum.join(values, "\n")}
    end
  end

  def parse_preview(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          backend: :string,
          model: :string,
          effort: :string,
          host: :string,
          port: :integer,
          workspace: :string,
          skill_root: :keep,
          no_backend: :boolean,
          skip_reference_resolution: :boolean
        ]
      )

    cond do
      invalid != [] ->
        {:error, "invalid options: #{inspect(invalid)}"}

      args != [] ->
        {:error, "unexpected arguments: #{inspect(args)}"}

      Keyword.get(opts, :backend, "codex") not in ["codex", "cursor"] ->
        {:error, "--backend must be codex or cursor"}

      Keyword.get(opts, :effort, "high") not in ~w(low medium high xhigh max ultra) ->
        {:error, "invalid --effort"}

      true ->
        {:ok, opts}
    end
  end

  defp run_serve(opts) do
    config_opts =
      [workspace: opts[:workspace]]
      |> put_if_present(:repo, opts[:repo])
      |> put_if_present(:host, opts[:host])
      |> put_if_present(:port, opts[:port])

    with {:ok, config} <- Pika.Config.load(opts[:config], config_opts),
         preflight <-
           Pika.Preflight.run([
             config.backend,
             config.campaign["integration_agent"] | config.campaign["iteration_agents"]
           ]),
         {:ok, plan} <- Pika.Workspace.plan(config),
         :ok <- configure_serve(config, plan, preflight),
         %{token: token} <- Pika.Auth.generate(),
         {:ok, _apps} <- Application.ensure_all_started(:pika),
         snapshot <- Pika.Runtime.snapshot() do
      browser_host = if config.host in ["0.0.0.0", "::"], do: "127.0.0.1", else: config.host
      IO.puts("Pika Workspace: #{snapshot.workspace.root}")
      IO.puts("Pika Repo mode: #{snapshot.workspace.mode}")
      IO.puts("Pika Campaign: #{snapshot.campaign.id} (#{snapshot.recovery |> recovery_label()})")
      IO.puts("Pika URL: http://#{browser_host}:#{config.port}/?token=#{token}")
      wait_forever()
    else
      {:error, reason} -> abort(format_error(reason))
    end
  end

  defp configure_serve(config, plan, preflight) do
    with :ok <- configure_endpoint(config.host, config.port) do
      Application.put_env(:pika, :runtime_mode, :serve)
      Application.put_env(:pika, :workspace_plan, plan)
      Application.put_env(:pika, :preflight, preflight)

      if map_size(config.prompts) > 0 do
        defaults = Application.fetch_env!(:pika, Pika.PromptCatalog)

        overrides =
          Enum.reduce(config.prompts, defaults, fn {kind, path}, acc ->
            Keyword.put(acc, String.to_existing_atom(kind), path)
          end)

        Application.put_env(:pika, Pika.PromptCatalog, overrides)
      end

      Application.put_env(:pika, Pika.Repo,
        database: Path.join(config.workspace, "pika.sqlite3"),
        pool_size: 1,
        journal_mode: :wal,
        synchronous: :full,
        foreign_keys: :on,
        busy_timeout: 5_000,
        log: false
      )

      :ok
    end
  end

  defp run_preview(repo, opts) do
    host = Keyword.get(opts, :host, "127.0.0.1")
    port = Keyword.get(opts, :port, 0) |> choose_port()

    with {:ok, workspace} <- Workspace.prepare(repo, workspace: Keyword.get(opts, :workspace)),
         %{token: token} <- Auth.generate(),
         :ok <- configure_endpoint(host, port),
         {:ok, _apps} <- Application.ensure_all_started(:pika),
         skill <-
           Pika.SkillRegistry.ensure_latest(
             Path.join(workspace.root, ".pika/skills/ncu-report-skill")
           ),
         {:ok, _campaign} <- start_campaign(workspace, skill, host, port, opts) do
      browser_host = if host in ["0.0.0.0", "::"], do: "127.0.0.1", else: host
      IO.puts("Pika Preview Workspace: #{workspace.root}")

      if workspace.source_status != "",
        do:
          IO.puts(
            "Pika Preview Source: dirty working tree ignored; cloned committed HEAD #{workspace.source_sha}"
          )

      IO.puts("Pika Preview URL: http://#{browser_host}:#{port}/?token=#{token}")
      IO.puts("State is in-memory; Workspace and Artifacts are retained after exit.")
      wait_forever()
    else
      {:error, reason} ->
        abort("preview failed: #{inspect(reason)}")
    end
  end

  defp start_campaign(workspace, skill, host, port, opts) do
    backend =
      if Keyword.get(opts, :backend, "codex") == "cursor",
        do: :cursor_acp,
        else: :codex_app_server

    mcp_host = if host in ["0.0.0.0", "::"], do: "127.0.0.1", else: host

    Campaign.start(
      workspace: workspace,
      backend: backend,
      model: Keyword.get(opts, :model),
      reasoning_effort: Keyword.get(opts, :effort, "high") |> String.to_atom(),
      start_backend: not Keyword.get(opts, :no_backend, false),
      resolve_references: not Keyword.get(opts, :skip_reference_resolution, false),
      mcp_url: "http://#{mcp_host}:#{port}/mcp",
      skill: skill,
      skill_roots: Keyword.get_values(opts, :skill_root) |> Enum.map(&Path.expand/1)
    )
  end

  defp configure_endpoint(host, port) do
    with {:ok, ip} <- parse_ip(host) do
      current = Application.get_env(:pika, PikaWeb.Endpoint, [])

      Application.put_env(
        :pika,
        PikaWeb.Endpoint,
        Keyword.merge(current,
          server: true,
          http: [ip: ip, port: port],
          url: [host: host, port: port]
        )
      )

      :ok
    end
  end

  defp put_if_present(options, _key, nil), do: options
  defp put_if_present(options, key, value), do: Keyword.put(options, key, value)

  defp discover_workspace(cwd) do
    if File.regular?(Path.join(cwd, "pika.yaml")) or
         File.regular?(Path.join(cwd, "config.json")),
       do: cwd,
       else: nil
  end

  defp default_workspace_config(nil), do: nil

  defp default_workspace_config(workspace) do
    path = Path.join(Path.expand(workspace), "pika.yaml")
    if File.regular?(path), do: path, else: nil
  end

  defp invocation_cwd, do: System.get_env("PIKA_CLI_CWD") || File.cwd!()

  defp expand_from(nil, _cwd), do: nil
  defp expand_from(path, cwd), do: Path.expand(path, cwd)

  defp maybe_cli_error(errors, false, _message), do: errors
  defp maybe_cli_error(errors, true, message), do: errors ++ [message]

  defp recovery_label(:initialized), do: "initialized"
  defp recovery_label(:recovered), do: "recovered"
  defp recovery_label("initialized"), do: "initialized"
  defp recovery_label("recovered"), do: "recovered"
  defp recovery_label(%{"status" => "blocked"}), do: "blocked"
  defp recovery_label({:blocked, _reason}), do: "blocked"
  defp recovery_label(value), do: inspect(value)

  defp format_error({:invalid_config, errors}),
    do: "configuration validation failed:\n" <> Enum.map_join(errors, "\n", &"  - #{&1}")

  defp format_error({:immutable_config_changed, errors}),
    do: "immutable configuration changed:\n" <> Enum.map_join(errors, "\n", &"  - #{&1}")

  defp format_error(reason), do: "pika serve failed: #{inspect(reason)}"

  defp format_error(command, {:invalid_config, errors}),
    do:
      "pika #{command} failed: configuration validation failed:\n" <>
        Enum.map_join(errors, "\n", &"  - #{&1}")

  defp format_error(command, {:immutable_config_changed, errors}),
    do:
      "pika #{command} failed: immutable configuration changed:\n" <>
        Enum.map_join(errors, "\n", &"  - #{&1}")

  defp format_error(command, reason), do: "pika #{command} failed: #{inspect(reason)}"

  defp parse_ip(host) do
    case :inet.parse_address(String.to_charlist(host)) do
      {:ok, ip} -> {:ok, ip}
      _ -> {:error, {:invalid_host, host}}
    end
  end

  defp choose_port(0), do: Pika.MCP.ProbeServer.available_port()
  defp choose_port(port) when port in 1..65_535, do: port
  defp choose_port(port), do: abort("invalid port: #{port}")

  defp wait_forever do
    receive do
      :stop -> :ok
    end
  end

  defp abort(message) do
    IO.puts(:stderr, message)
    System.halt(2)
  end

  defp usage do
    """
    Usage: pika init [WORKSPACE] [options]

    Usage: pika serve [--workspace PATH] [--config PIKA_YAML] [options]

      --repo PATH        Manage an existing clean Git repository
      --host IP          Override server.host (default 127.0.0.1)
      --port PORT        Override server.port (default 8080)

    From an initialized Workspace containing pika.yaml, simply run: pika serve

    Usage: pika preview <repo> [options]

      --backend codex|cursor
      --model MODEL
      --effort low|medium|high|xhigh|max|ultra
      --host IP
      --port PORT
      --workspace EMPTY_DIRECTORY
      --skill-root PATH
    """
  end

  defp init_usage do
    """
    Usage: pika init [WORKSPACE] [options]

      --repo PATH              Manage an existing clean Git repository
      --owned                  Create a new repository inside the Workspace
      --config PATH            Configuration output (default WORKSPACE/pika.yaml)
      --host IP                Listen host (default 127.0.0.1)
      --port PORT              Listen port (default 8080)
      --alignment-backend B    Alignment/Baseline backend: codex|cursor
      --alignment-model MODEL  Alignment/Baseline model (default: provider default)
      --alignment-effort E     Alignment/Baseline reasoning effort (default high)
      --alignment-approval-policy P
                               Backend-specific Alignment/Baseline approval policy
      --alignment-sandbox-policy P
                               Backend-specific Alignment/Baseline sandbox policy
      --iteration-backend B    Iteration Agent backend: codex|cursor
      --iteration-model MODEL  Iteration Agent model (default: provider default)
      --iteration-effort E     Iteration Agent reasoning effort (default high)
      --iteration-approval-policy P
                               Backend-specific Iteration approval policy
      --iteration-sandbox-policy P
                               Backend-specific Iteration sandbox policy
      Codex approval P         never|on_request|untrusted
      Codex sandbox P          danger_full_access|workspace_write|read_only
      Cursor approval P        force|auto_review
      Cursor sandbox P         disabled|enabled
      --backend codex|cursor   Set both backends (compatibility shorthand)
      --model MODEL            Compatibility alias for --iteration-model
      --effort EFFORT          Compatibility alias for --iteration-effort
      --iteration-agents N     Concurrent Iteration Agents (default 1)
      --progress-summary       Enable periodic AI progress summaries (default off)
      --summary-backend B      Summary backend: codex|cursor
      --summary-model MODEL    Summary model (default: provider default)
      --summary-effort E       Summary reasoning effort (default medium)
      --summary-interval-minutes N
                               Summary interval (default 10)
      --max-attempts N         Campaign attempt limit (default unlimited)
      --sync-remote NAME       Configure a Git sync remote
      --sync-branch NAME       Configure a Git sync branch
      --no-sync                Do not configure Git sync
      -y, --yes                Accept defaults for unspecified settings
      -h, --help               Show this help
    """
  end

  defp valid_init_permission?(_backend, _kind, nil), do: true

  defp valid_init_permission?(backend, kind, value) do
    normalized_backend =
      case backend do
        "cursor" -> :cursor_acp
        "cursor_acp" -> :cursor_acp
        _ -> :codex_app_server
      end

    match?({:ok, _value}, PermissionPolicy.parse(normalized_backend, kind, value))
  end
end
