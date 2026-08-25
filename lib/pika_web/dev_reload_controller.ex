defmodule PikaWeb.DevReloadController do
  use PikaWeb, :controller

  def show(conn, _params) do
    case Application.get_env(:pika, :dev_reload_root) do
      root when is_binary(root) ->
        conn
        |> put_resp_header("cache-control", "no-store")
        |> json(%{fingerprint: Pika.DevReload.fingerprint(root)})

      _other ->
        send_resp(conn, 404, "not found")
    end
  end
end
