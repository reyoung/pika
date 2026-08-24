defmodule PikaWeb.MCPGateway do
  @moduledoc false

  def init(opts), do: opts
  def call(conn, _opts), do: Pika.Agent.MCP.Router.call(conn, Pika.Agent.MCP.Router.init([]))
end
