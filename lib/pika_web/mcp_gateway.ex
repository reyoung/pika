defmodule PikaWeb.MCPGateway do
  @moduledoc false

  def init(opts), do: opts

  def call(conn, _opts) do
    cond do
      sync_token?(conn) ->
        Pika.Sync.MCP.Router.call(conn, Pika.Sync.MCP.Router.init([]))

      integration_token?(conn) ->
        Pika.Integration.MCP.Router.call(conn, Pika.Integration.MCP.Router.init([]))

      attempt_token?(conn) ->
        Pika.Optimization.MCP.Router.call(conn, Pika.Optimization.MCP.Router.init([]))

      campaign_token?(conn) ->
        Pika.Alignment.MCP.Router.call(conn, Pika.Alignment.MCP.Router.init([]))

      true ->
        Pika.MCP.Router.call(conn, Pika.MCP.Router.init([]))
    end
  end

  defp sync_token?(conn) do
    case Plug.Conn.get_req_header(conn, "authorization") do
      ["Bearer " <> token] -> Pika.SyncCoordinator.authorize(token) == :ok
      _ -> false
    end
  end

  defp integration_token?(conn) do
    case Plug.Conn.get_req_header(conn, "authorization") do
      ["Bearer " <> token] -> Pika.IntegrationCoordinator.authorize(token) == :ok
      _ -> false
    end
  end

  defp attempt_token?(conn) do
    case Plug.Conn.get_req_header(conn, "authorization") do
      ["Bearer " <> token] -> Pika.AttemptCoordinator.authorize(token) == :ok
      _ -> false
    end
  end

  defp campaign_token?(conn) do
    case Plug.Conn.get_req_header(conn, "authorization") do
      ["Bearer " <> token] -> Pika.Alignment.Campaign.authorize(token) == :ok
      _ -> false
    end
  end
end
