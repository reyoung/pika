defmodule PikaWeb.EntryAuth do
  @moduledoc false

  def init(opts), do: opts
  def call(conn, _opts), do: PikaWeb.Auth.call(conn, [])
end
