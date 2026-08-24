defmodule Pika.CLI do
  @moduledoc false

  alias Pika.Optimization.{Bootstrap, Config, Init, Persistence}

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

  def main([flag]) when flag in ["--help", "-h", "help"], do: IO.puts(usage())
  def main(_argv), do: abort(usage())

  def parse_init(argv) do
    {opts, args, invalid} =
      OptionParser.parse(argv,
        strict: [
          repo: :string,
          backend: :string,
          model: :string,
          reasoning_effort: :string,
          iteration_agents: :integer,
          progress_summary: :boolean,
          help: :boolean
        ],
        aliases: [h: :help]
      )

    errors =
      []
      |> maybe_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_error(length(args) > 1, "expected exactly one WORKSPACE argument")
      |> maybe_error(opts[:help] != true and args == [], "WORKSPACE is required")
      |> maybe_error(opts[:help] != true and is_nil(opts[:repo]), "--repo PATH is required")
      |> maybe_error(
        Keyword.get(opts, :backend, "codex") not in ~w(codex cursor),
        "--backend must be codex or cursor"
      )
      |> maybe_error(
        Keyword.get(opts, :iteration_agents, 1) < 1,
        "--iteration-agents must be positive"
      )

    if errors == [] do
      {:ok,
       %{
         help: opts[:help] == true,
         workspace: args |> List.first() |> expand_optional(),
         repo: expand_optional(opts[:repo]),
         backend: Keyword.get(opts, :backend, "codex"),
         model: opts[:model],
         reasoning_effort: Keyword.get(opts, :reasoning_effort, "high"),
         iteration_agents: Keyword.get(opts, :iteration_agents, 1),
         progress_summary: opts[:progress_summary] == true
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
         port: opts[:port] || 8080
       }}
    else
      {:error, Enum.join(errors, "\n")}
    end
  end

  defp run_init(opts) do
    init_opts = [
      backend: opts.backend,
      model: opts.model,
      reasoning_effort: opts.reasoning_effort,
      iteration_agents: opts.iteration_agents,
      progress_summary: opts.progress_summary
    ]

    case Init.run(opts.workspace, opts.repo, init_opts) do
      {:ok, config} ->
        IO.puts("Created Pika v2 Workspace: #{config.workspace}")
        IO.puts("Configuration: #{config.source_path}")
        IO.puts("Run: pika serve --workspace #{config.workspace}")

      {:error, reason} ->
        abort("pika init failed: #{inspect(reason)}")
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
         :ok <- configure_endpoint(opts.host, opts.port),
         :ok <- configure_runtime(config),
         %{token: token} <- Pika.Auth.generate(),
         {:ok, _apps} <- Application.ensure_all_started(:pika),
         snapshot <- Bootstrap.snapshot() do
      browser_host = if opts.host in ["0.0.0.0", "::"], do: "127.0.0.1", else: opts.host
      IO.puts("Pika Workspace: #{config.workspace}")
      IO.puts("Pika Repo: #{config.repo}")
      IO.puts("Pika Optimization: #{snapshot.optimization.id} (#{snapshot.recovery})")
      IO.puts("Pika URL: http://#{browser_host}:#{opts.port}/?token=#{token}")
      wait()
    else
      {:error, reason} -> abort("pika serve failed: #{inspect(reason)}")
      false -> abort("pika serve failed: configuration identity mismatch")
    end
  end

  defp configure_runtime(config) do
    Application.put_env(:pika, :runtime_mode, :v2)
    Application.put_env(:pika, :v2_config_path, config.source_path)

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

  defp configure_endpoint(host, port) do
    with {:ok, ip} <- :inet.parse_address(String.to_charlist(host)) do
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
    else
      _other -> {:error, {:invalid_host, host}}
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

  defp usage, do: init_usage() <> "\n" <> serve_usage()

  defp init_usage do
    """
    Usage: pika init WORKSPACE --repo PATH [options]

    Creates a new, incompatible v2 Workspace for exactly one repository and Optimization.
    The repository must be a clean Git worktree. Pika never pushes or merges to a remote branch.

      --backend codex|cursor       Backend for required Roles (default codex)
      --model MODEL                Optional provider model
      --reasoning-effort EFFORT    Default reasoning effort (default high)
      --iteration-agents N         Iteration concurrency; each entry is expanded (default 1)
      --progress-summary           Enable five-minute Progress Summary in Asia/Shanghai
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
      -h, --help
    """
  end

  defp abort(message) do
    IO.puts(:stderr, message)
    System.halt(2)
  end
end
