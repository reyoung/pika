defmodule PikaWeb.RootController do
  use PikaWeb, :controller

  def index(conn, _params) do
    module =
      if Application.get_env(:pika, :runtime_mode, :stage0) == :serve,
        do: PikaWeb.DashboardLive,
        else: PikaWeb.AlignmentLive

    Phoenix.LiveView.Controller.live_render(conn, module, session: get_session(conn))
  end
end
