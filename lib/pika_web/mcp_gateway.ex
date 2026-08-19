defmodule PikaWeb.MCPGateway do
  @moduledoc false

  def init(opts), do: opts

  def call(conn, _opts) do
    if campaign_token?(conn) do
      Pika.Stage0.MCP.Router.call(conn, Pika.Stage0.MCP.Router.init([]))
    else
      Pika.MCP.Router.call(conn, Pika.MCP.Router.init([]))
    end
  end

  defp campaign_token?(conn) do
    case Plug.Conn.get_req_header(conn, "authorization") do
      ["Bearer " <> token] -> Pika.Stage0.Campaign.authorize(token) == :ok
      _ -> false
    end
  end
end
