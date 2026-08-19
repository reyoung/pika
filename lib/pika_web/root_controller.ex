defmodule PikaWeb.RootController do
  use PikaWeb, :controller

  def index(conn, _params) do
    Phoenix.LiveView.Controller.live_render(conn, PikaWeb.AlignmentLive,
      session: get_session(conn)
    )
  end
end
