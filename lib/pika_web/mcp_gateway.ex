defmodule PikaWeb.MCPGateway do
  @moduledoc false

  def init(opts), do: opts

  def call(conn, _opts) do
    if Application.get_env(:pika, :runtime_mode, :stage0) == :serve do
      Pika.MCP.Router.call(conn, Pika.MCP.Router.init([]))
    else
      Pika.Stage0.MCP.Router.call(conn, Pika.Stage0.MCP.Router.init([]))
    end
  end
end
