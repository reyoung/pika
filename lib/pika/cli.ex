defmodule Pika.CLI do
  @moduledoc false

  alias Pika.Stage0.{Auth, Campaign, Workspace}

  def main(["stage0-demo", repo | argv]) do
    case parse_stage0(argv) do
      {:ok, opts} -> run_stage0(repo, opts)
      {:error, message} -> abort(message)
    end
  end

  def main(_argv), do: abort(usage())

  def parse_stage0(argv) do
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

  defp run_stage0(repo, opts) do
    host = Keyword.get(opts, :host, "127.0.0.1")
    port = Keyword.get(opts, :port, 0) |> choose_port()

    with {:ok, workspace} <- Workspace.prepare(repo, workspace: Keyword.get(opts, :workspace)),
         %{token: token} <- Auth.generate(),
         :ok <- configure_endpoint(host, port),
         {:ok, _apps} <- Application.ensure_all_started(:pika),
         skill <-
           Pika.Phase0.Skill.ensure_latest(
             Path.join(workspace.root, ".pika/skills/ncu-report-skill")
           ),
         {:ok, _campaign} <- start_campaign(workspace, skill, host, port, opts) do
      browser_host = if host in ["0.0.0.0", "::"], do: "127.0.0.1", else: host
      IO.puts("Pika Stage0 Workspace: #{workspace.root}")

      if workspace.source_status != "",
        do:
          IO.puts(
            "Pika Stage0 Source: dirty working tree ignored; cloned committed HEAD #{workspace.source_sha}"
          )

      IO.puts("Pika Stage0 URL: http://#{browser_host}:#{port}/?token=#{token}")
      IO.puts("State is in-memory; Workspace and Artifacts are retained after exit.")
      wait_forever()
    else
      {:error, reason} ->
        abort("stage0-demo failed: #{inspect(reason)}")
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
    Usage: pika stage0-demo <repo> [options]

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
