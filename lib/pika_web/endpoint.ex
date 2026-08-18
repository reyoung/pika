defmodule PikaWeb.Endpoint do
  @moduledoc "Future Phoenix HTTP boundary; Phase 0 starts the probe router directly with Bandit."

  use Phoenix.Endpoint, otp_app: :pika

  plug Plug.RequestId
  plug Pika.MCP.ProbeRouter
end
