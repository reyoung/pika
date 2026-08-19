defmodule PikaWeb.Router do
  use PikaWeb, :router

  pipeline :browser do
    plug :accepts, ["html"]
    plug :fetch_session
    plug :fetch_live_flash
    plug :put_root_layout, html: {PikaWeb.Layouts, :root}
    plug :protect_from_forgery
    plug :put_secure_browser_headers
    plug PikaWeb.EntryAuth
  end

  pipeline :api do
    plug :accepts, ["json"]
    plug PikaWeb.APIAuth
  end

  scope "/" do
    pipe_through :browser
    get "/", PikaWeb.RootController, :index
    get "/alignment", PikaWeb.RootController, :alignment
    get "/control", PikaWeb.RootController, :control
  end

  scope "/api", PikaWeb do
    pipe_through :api
    get "/status", StatusController, :show
    get "/control", ControlController, :show
    get "/attempts", ControlController, :attempts
    get "/attempts/:id", ControlController, :attempt
    get "/metrics", ControlController, :metrics
    get "/audit/events", ControlController, :events
    get "/sync/preview", ControlController, :sync_preview
    post "/control/pause", ControlController, :pause
    post "/control/stop", ControlController, :stop
    post "/control/resume", ControlController, :resume
    post "/sync", ControlController, :request_sync
    post "/sync/:id/spec-confirmation", ControlController, :confirm_sync_spec
    post "/attempts/:id/btw", ControlController, :create_btw
  end

  scope "/", PikaWeb do
    pipe_through :api
    get "/events", StatusController, :events
  end

  forward "/mcp", PikaWeb.MCPGateway
end
