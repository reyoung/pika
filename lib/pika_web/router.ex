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

  pipeline :api do
    plug :accepts, ["json"]
    plug PikaWeb.APIAuth
  end

  scope "/" do
    pipe_through :browser
    get "/", PikaWeb.RootController, :index
  end

  scope "/api", PikaWeb do
    pipe_through :api
    get "/status", StatusController, :show
  end

  scope "/", PikaWeb do
    pipe_through :api
    get "/events", StatusController, :events
  end

  forward "/mcp", PikaWeb.MCPGateway
end
