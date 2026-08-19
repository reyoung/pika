defmodule PikaWeb.APIAuth do
  @moduledoc false

  import Plug.Conn

  def init(opts), do: opts

  def call(conn, _opts) do
    authorization = get_req_header(conn, "authorization") |> List.first()

    if Pika.Auth.bearer_authenticated?(authorization) do
      conn
    else
      conn
      |> put_resp_content_type("application/json")
      |> send_resp(401, Jason.encode!(%{error: "unauthorized"}))
      |> halt()
    end
  end
end
