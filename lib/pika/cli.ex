defmodule Pika.CLI do
  @moduledoc false

  alias Pika.Optimization.{
    Bootstrap,
    Config,
    Init,
    InteractiveConfig,
    Persistence,
    Reconfiguration
  }

  @reasoning_efforts ~w(low medium high xhigh max ultra)

  def main([command | argv]) when command in ["init", "init-v2"] do
    case parse_init(argv) do
      {:ok, %{help: true}} -> IO.puts(init_usage())
      {:ok, opts} -> run_init(opts)
      {:error, message} -> abort(message <> "\n\n" <> init_usage())
    end
  end

  def main(["serve" | argv]) do
    case parse_serve(argv) do
      {:ok, %{help: true}} -> IO.puts(serve_usage())
      {:ok, opts} -> run_serve(opts)
      {:error, message} -> abort(message <> "\n\n" <> serve_usage())
    end
  end

  def main([command | argv]) when command in ["reconfiguration", "reconfigure"] do
    case parse_reconfiguration(argv) do
      {:ok, %{help: true}} -> IO.puts(reconfiguration_usage())
      {:ok, opts} -> run_reconfiguration(opts)
      {:error, message} -> abort(message <> "\n\n" <> reconfiguration_usage())
    end
  end

  def main([flag]) when flag in ["--help", "-h", "help"], do: IO.puts(usage())
  def main(_argv), do: abort(usage())

  def parse_init(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          repo: :string,
          token: :string,
          backend: :string,
          model: :string,
          reasoning_effort: :string,
          iteration_agents: :integer,
          baseline_verify_followup: :boolean,
          iteration_followup: :boolean,
          integration_followup: :boolean,
          progress_summary: :boolean,
          yes: :boolean,
          help: :boolean
        ],
        aliases: [y: :yes, h: :help]
      )

    errors =
      []
      |> maybe_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_error(length(args) > 1, "expected at most one WORKSPACE argument")
      |> maybe_error(
        not is_nil(opts[:backend]) and opts[:backend] not in ~w(codex cursor),
        "--backend must be codex or cursor"
      )
      |> maybe_error(
        not is_nil(opts[:reasoning_effort]) and opts[:reasoning_effort] not in @reasoning_efforts,
        "--reasoning-effort must be one of #{Enum.join(@reasoning_efforts, ", ")}"
      )
      |> maybe_error(
        not is_nil(opts[:iteration_agents]) and opts[:iteration_agents] < 1,
        "--iteration-agents must be positive"
      )
      |> maybe_error(
        opts[:help] != true and opts[:yes] == true and args == [],
        "WORKSPACE is required with --yes"
      )
      |> maybe_error(
        opts[:help] != true and opts[:yes] == true and is_nil(opts[:repo]),
        "--repo PATH is required with --yes"
      )

    if errors == [] do
      {:ok,
       %{
         help: opts[:help] == true,
         workspace: args |> List.first() |> expand_optional(),
         repo: expand_optional(opts[:repo]),
         token: opts[:token],
         backend: opts[:backend],
         model: opts[:model],
         reasoning_effort: opts[:reasoning_effort],
         iteration_agents: opts[:iteration_agents],
         baseline_verify_followup: opts[:baseline_verify_followup],
         iteration_followup: opts[:iteration_followup],
         integration_followup: opts[:integration_followup],
         progress_summary: opts[:progress_summary],
         yes: opts[:yes] == true
       }}
    else
      {:error, Enum.join(errors, "\n")}
    end
  end

  def parse_reconfiguration(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          workspace: :string,
          config: :string,
          role: :string,
          all: :boolean,
          backend: :string,
          model: :string,
          reasoning_effort: :string,
          iteration_agents: :integer,
          enable: :boolean,
          yes: :boolean,
          help: :boolean
        ],
        aliases: [y: :yes, h: :help]
      )

    cwd = invocation_cwd()
    workspace = expand_from(opts[:workspace], cwd) || discover_workspace(cwd)
    config_path = expand_from(opts[:config], cwd) || default_config(workspace)

    errors =
      []
      |> maybe_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_error(args != [], "unexpected arguments: #{inspect(args)}")
      |> maybe_error(
        opts[:help] != true and is_nil(workspace),
        "--workspace is required outside a v2 Workspace"
      )
      |> maybe_error(
        opts[:help] != true and is_nil(config_path),
        "WORKSPACE/pika.yaml is required"
      )
      |> maybe_error(
        opts[:all] == true and not is_nil(opts[:role]),
        "--all and --role cannot be combined"
      )
      |> maybe_error(
        not is_nil(opts[:backend]) and opts[:backend] not in ~w(codex cursor),
        "--backend must be codex or cursor"
      )
      |> maybe_error(
        not is_nil(opts[:reasoning_effort]) and opts[:reasoning_effort] not in @reasoning_efforts,
        "--reasoning-effort must be one of #{Enum.join(@reasoning_efforts, ", ")}"
      )
      |> maybe_error(
        not is_nil(opts[:iteration_agents]) and opts[:iteration_agents] < 1,
        "--iteration-agents must be positive"
      )

    if errors == [] do
      {:ok,
       %{
         help: opts[:help] == true,
         workspace: workspace,
         config: config_path,
         role: opts[:role],
         all: opts[:all] == true,
         backend: opts[:backend],
         model: opts[:model],
         reasoning_effort: opts[:reasoning_effort],
         iteration_agents: opts[:iteration_agents],
         enabled: opts[:enable],
         yes: opts[:yes] == true
       }}
    else
      {:error, Enum.join(errors, "\n")}
    end
  end

  def parse_serve(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          workspace: :string,
          config: :string,
          host: :string,
          port: :integer,
          reload: :boolean,
          autoreload: :boolean,
          help: :boolean
        ],
        aliases: [h: :help]
      )

    cwd = invocation_cwd()
    workspace = expand_from(opts[:workspace], cwd) || discover_workspace(cwd)
    config_path = expand_from(opts[:config], cwd) || default_config(workspace)

    errors =
      []
      |> maybe_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_error(args != [], "unexpected arguments: #{inspect(args)}")
      |> maybe_error(
        opts[:help] != true and is_nil(workspace),
        "--workspace is required outside a v2 Workspace"
      )
      |> maybe_error(
        opts[:help] != true and is_nil(config_path),
        "WORKSPACE/pika.yaml is required"
      )
      |> maybe_error(
        not is_nil(opts[:port]) and opts[:port] not in 1..65_535,
        "--port must be from 1 through 65535"
      )

    if errors == [] do
      {:ok,
       %{
         help: opts[:help] == true,
         workspace: workspace,
         config: config_path,
         host: opts[:host] || "127.0.0.1",
         port: opts[:port] || 8080,
         reload: opts[:reload] == true or opts[:autoreload] == true
       }}
    else
      {:error, Enum.join(errors, "\n")}
    end
  end

  defp run_init(opts) do
    result =
      with {:ok, settings} <- InteractiveConfig.collect_init(opts),
           {:ok, config} <-
             Init.run(settings.workspace, settings.repo,
               token: settings.token,
               agents: settings.agents
             ) do
        {:ok, config}
      end

    case result do
      {:ok, config} ->
        IO.puts("Created Pika v2 Workspace: #{config.workspace}")
        IO.puts("Configuration: #{config.source_path}")
        IO.puts("Run: pika serve --workspace #{config.workspace}")

      {:error, reason} ->
        abort("pika init failed: #{inspect(reason)}")
    end
  end

  defp run_reconfiguration(opts) do
    case Reconfiguration.run(opts.workspace, opts.config, opts) do
      {:ok, _config} -> :ok
      {:error, reason} -> abort("pika reconfiguration failed: #{inspect(reason)}")
    end
  end

  defp run_serve(opts) do
    with {:ok, config} <- Config.load(opts.config),
         true <-
           Path.expand(config.workspace) == Path.expand(opts.workspace) ||
             {:error, :workspace_mismatch},
         true <-
           Path.expand(config.source_path) ==
             Path.expand(Path.join(config.workspace, "pika.yaml")) ||
             {:error, {:config_must_be_workspace_pika_yaml, config.workspace}},
         :ok <- validate_reload(opts.reload),
         :ok <- configure_endpoint(opts.host, opts.port, opts.reload),
         :ok <- configure_runtime(config, opts.reload),
         %{token: token} <- Pika.Auth.configure(config.token),
         {:ok, _apps} <- Application.ensure_all_started(:pika),
         snapshot <- Bootstrap.snapshot() do
      browser_host = if opts.host in ["0.0.0.0", "::"], do: "127.0.0.1", else: opts.host
      IO.puts("Pika Workspace: #{config.workspace}")
      IO.puts("Pika Repo: #{config.repo}")
      IO.puts("Pika Optimization: #{snapshot.optimization.id} (#{snapshot.recovery})")

      if opts.reload do
        IO.puts("Pika Auto-reload: enabled for Elixir, JS, and CSS source changes")
      end

      IO.puts(
        "Pika URL: http://#{browser_host}:#{opts.port}/?token=#{URI.encode_www_form(token)}"
      )

      wait()
    else
      {:error, reason} -> abort("pika serve failed: #{inspect(reason)}")
      false -> abort("pika serve failed: configuration identity mismatch")
    end
  end

  defp configure_runtime(config, reload?) do
    Application.put_env(:pika, :runtime_mode, :v2)
    Application.put_env(:pika, :v2_config_path, config.source_path)
    Application.put_env(:pika, :dev_reload, reload?)

    if reload? do
      Application.put_env(:pika, :dev_reload_root, File.cwd!())
    else
      Application.delete_env(:pika, :dev_reload_root)
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

  defp configure_endpoint(host, port, reload?) do
    with {:ok, ip} <- :inet.parse_address(String.to_charlist(host)) do
      current = Application.get_env(:pika, PikaWeb.Endpoint, [])

      watchers =
        if reload? do
          [esbuild: {Esbuild, :install_and_run, [:default, ~w(--watch)]}]
        else
          []
        end

      Application.put_env(
        :pika,
        PikaWeb.Endpoint,
        Keyword.merge(current,
          server: true,
          watchers: watchers,
          http: [ip: ip, port: port],
          url: [host: host, port: port]
        )
      )

      :ok
    else
      _other -> {:error, {:invalid_host, host}}
    end
  end

  defp validate_reload(false), do: :ok

  defp validate_reload(true) do
    mix_running? =
      Code.ensure_loaded?(Mix.Project) and not is_nil(Process.whereis(Mix.ProjectStack))

    if mix_running? and Code.ensure_loaded?(Esbuild) do
      :ok
    else
      {:error, :autoreload_requires_source_checkout}
    end
  end

  defp wait do
    receive do
      :stop -> :ok
    after
      1_000 ->
        case Persistence.current() do
          %{status: status} when status in ["completed", "stopped"] -> :ok
          %{status: "failed", stop_reason: reason} -> abort("Pika Optimization failed: #{reason}")
          _optimization -> wait()
        end
    end
  end

  defp discover_workspace(cwd) do
    if File.regular?(Path.join(cwd, "pika.yaml")), do: cwd, else: nil
  end

  defp default_config(nil), do: nil
  defp default_config(workspace), do: Path.join(workspace, "pika.yaml")
  defp invocation_cwd, do: System.get_env("PIKA_CLI_CWD") || File.cwd!()
  defp expand_from(nil, _cwd), do: nil
  defp expand_from(path, cwd), do: Path.expand(path, cwd)
  defp expand_optional(nil), do: nil
  defp expand_optional(path), do: Path.expand(path, invocation_cwd())
  defp maybe_error(errors, true, message), do: errors ++ [message]
  defp maybe_error(errors, false, _message), do: errors

  defp usage, do: init_usage() <> "\n" <> serve_usage() <> "\n" <> reconfiguration_usage()

  defp init_usage do
    """
    Usage: pika init [WORKSPACE] [--repo PATH] [options]

    Creates a new, incompatible v2 Workspace for exactly one repository and Optimization.
    Without --yes, starts an interactive wizard for every Agent configuration. The repository
    must be a clean Git worktree. Pika never pushes or merges to a remote branch.

      --backend codex|cursor       Default Backend for Agent prompts
      --token TOKEN                Fixed access token (default: random 256-bit token)
      --model MODEL                Default provider model for Agent prompts
      --reasoning-effort EFFORT    Default effort: low|medium|high|xhigh|max|ultra
      --iteration-agents N         Iteration concurrency; every Agent is configured separately
      --baseline-verify-followup   Enable the optional Baseline Verify Follow-up Agent
      --iteration-followup         Enable the optional Iteration Follow-up Agent
      --integration-followup       Enable the optional Integration Follow-up Agent
      --progress-summary           Enable five-minute Progress Summary in Asia/Shanghai
      -y, --yes                    Non-interactive; accept defaults for unspecified settings
      -h, --help
    """
  end

  defp serve_usage do
    """
    Usage: pika serve [--workspace PATH] [--config PATH] [options]

    Starts the singleton v2 Optimization from WORKSPACE/pika.yaml. SQLite uses WAL/FULL
    durability. Backend crashes always create a new Session from messages.jsonl; provider
    resume is not used. Baseline Review and Agent questions are handled in the Web UI.

      --workspace PATH   Auto-detected from the current directory when pika.yaml exists
      --config PATH      Must resolve to WORKSPACE/pika.yaml
      --host IP          Listen host (default 127.0.0.1)
      --port PORT        Listen port (default 8080)
      --reload           Hot-compile Elixir, watch assets, and refresh the browser (source only)
      --autoreload       Alias for --reload
      -h, --help
    """
  end

  defp reconfiguration_usage do
    """
    Usage: pika reconfiguration [options]

    Interactively updates Backend, provider model, and reasoning effort for any configured
    Agent Role. New Sessions reload pika.yaml; active Sessions keep their frozen configuration.

      --workspace PATH             Auto-detected when the current directory has pika.yaml
      --config PATH                Must resolve to WORKSPACE/pika.yaml
      --role ROLE                  Configure one Role without the section menu
      --all                        Configure every Agent Role
      --backend codex|cursor       Use this Backend for the selected Agent(s)
      --model MODEL                Use a model id; "default" selects the provider default
      --reasoning-effort EFFORT    low|medium|high|xhigh|max|ultra
      --iteration-agents N         Change Iteration concurrency
      --enable, --no-enable        Enable or disable a selected optional Role
      -y, --yes                    Keep current values for unspecified settings
      -h, --help
    """
  end

  defp abort(message) do
    IO.puts(:stderr, message)
    System.halt(2)
  end
end
