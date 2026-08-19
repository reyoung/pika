defmodule Pika.CLI do
  @moduledoc false

  alias Pika.PreviewAuth, as: Auth
  alias Pika.Alignment.{Campaign, Workspace}

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

    errors =
      []
      |> maybe_cli_error(invalid != [], "invalid options: #{inspect(invalid)}")
      |> maybe_cli_error(args != [], "unexpected arguments: #{inspect(args)}")
      |> maybe_cli_error(is_nil(opts[:workspace]), "--workspace is required")
      |> maybe_cli_error(is_nil(opts[:config]), "--config is required")

    case errors do
      [] -> {:ok, opts}
      values -> {:error, Enum.join(values, "\n")}
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
         preflight <- Pika.Preflight.run(config.backend),
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
        busy_timeout: 5_000
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
    Usage: pika serve --workspace PATH --config PIKA_YAML [options]

      --repo PATH        Manage an existing clean Git repository
      --host IP          Override server.host (default 127.0.0.1)
      --port PORT        Override server.port (default 8080)

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
end
