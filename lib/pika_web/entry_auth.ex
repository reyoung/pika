defmodule PikaWeb.EntryAuth do
  @moduledoc false

  import Plug.Conn
  alias Phoenix.Controller

  def init(opts), do: opts

  def call(conn, _opts) do
    if Application.get_env(:pika, :runtime_mode, :preview) == :serve do
      PikaWeb.Auth.call(conn, [])
    else
      call_preview(conn)
    end
  end

  defp call_preview(conn) do
    conn = fetch_query_params(conn)

    cond do
      is_binary(conn.query_params["token"]) -> exchange_token(conn, conn.query_params["token"])
      Pika.PreviewAuth.authenticated_marker?(get_session(conn, :preview_auth)) -> conn
      true -> unauthorized(conn)
    end
  end

  defp exchange_token(conn, token) do
    case Pika.PreviewAuth.authenticate(token) do
      {:ok, marker} ->
        conn
        |> put_session(:preview_auth, marker)
        |> configure_session(renew: true)
        |> Controller.redirect(to: "/")
        |> halt()

      _ ->
        unauthorized(conn)
    end
  end

  defp unauthorized(conn) do
    body = """
    <!doctype html><html><head><meta charset="utf-8"><title>Pika Preview</title></head>
    <body style="font-family:system-ui;background:#071015;color:#dfe8ec;padding:4rem">
      <h1>Pika Preview</h1><p>Use the tokenized URL printed by <code>pika preview</code>.</p>
    </body></html>
    """

    conn
    |> put_resp_content_type("text/html")
    |> send_resp(401, body)
    |> halt()
  end
end
