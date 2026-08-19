defmodule PikaWeb.Endpoint do
  @moduledoc false

  use Phoenix.Endpoint, otp_app: :pika

  @session_options [
    store: :cookie,
    key: "_pika_stage0",
    signing_salt: "pika-stage0-cookie",
    same_site: "Strict",
    http_only: true
  ]

  socket "/live", Phoenix.LiveView.Socket,
    websocket: [connect_info: [session: @session_options]],
    longpoll: false

  plug Plug.Static,
    at: "/",
    from: :pika,
    gzip: false,
    only: PikaWeb.static_paths()

  plug Plug.RequestId
  plug Plug.Telemetry, event_prefix: [:phoenix, :endpoint]

  plug Plug.Parsers,
    parsers: [:urlencoded, :multipart, :json],
    pass: ["*/*"],
    json_decoder: Phoenix.json_library()

  plug Plug.MethodOverride
  plug Plug.Head
  plug Plug.Session, @session_options
  plug PikaWeb.Router
end
