defmodule PikaWeb.Router do
  use PikaWeb, :router

  pipeline :browser do
    plug :accepts, ["html"]
    plug :fetch_session
    plug :fetch_live_flash
    plug :put_root_layout, html: {PikaWeb.Layouts, :root}
    plug :protect_from_forgery
    plug :put_secure_browser_headers
    plug PikaWeb.Stage0Auth
  end

  scope "/" do
    pipe_through :browser
    live "/", PikaWeb.AlignmentLive, :index
  end

  forward "/mcp", Pika.Stage0.MCP.Router
end
