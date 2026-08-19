defmodule PikaWeb.RootController do
  use PikaWeb, :controller

  def index(conn, _params) do
    live_view =
      if Application.get_env(:pika, :runtime_mode, :preview) == :serve do
        campaign = Pika.Persistence.current_campaign()

        if campaign &&
             campaign.status in ~w(optimizing draining completed paused blocked stopped),
           do: PikaWeb.ControlLive,
           else: PikaWeb.AlignmentLive
      else
        PikaWeb.AlignmentLive
      end

    Phoenix.LiveView.Controller.live_render(conn, live_view, session: get_session(conn))
  end

  def alignment(conn, _params) do
    Phoenix.LiveView.Controller.live_render(conn, PikaWeb.AlignmentLive,
      session: get_session(conn)
    )
  end

  def control(conn, _params) do
    Phoenix.LiveView.Controller.live_render(conn, PikaWeb.ControlLive, session: get_session(conn))
  end
end
