defmodule PikaWeb.CodeReloader do
  @moduledoc false

  @behaviour Plug

  @impl true
  def init(opts), do: Phoenix.CodeReloader.init(opts)

  @impl true
  def call(conn, opts) do
    if Application.get_env(:pika, :dev_reload, false) do
      Phoenix.CodeReloader.call(conn, opts)
    else
      conn
    end
  end
end
