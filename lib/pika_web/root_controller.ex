defmodule PikaWeb.RootController do
  use PikaWeb, :controller

  def index(conn, _params), do: render_optimization(conn, get_session(conn))
  def alignment(conn, _params), do: render_optimization(conn, get_session(conn))

  def control(conn, params) do
    render_optimization(conn, Map.put(get_session(conn), "control_tab", params["tab"]))
  end

  defp render_optimization(conn, session) do
    Phoenix.LiveView.Controller.live_render(conn, PikaWeb.OptimizationLive, session: session)
  end
end
