defmodule PikaWeb do
  @moduledoc false

  def static_paths, do: ~w(assets favicon.svg robots.txt)

  def router do
    quote do
      use Phoenix.Router
      import Phoenix.LiveView.Router
    end
  end

  def live_view do
    quote do
      use Phoenix.LiveView, layout: {PikaWeb.Layouts, :app}
      import PikaWeb.CoreComponents
    end
  end

  def html do
    quote do
      use Phoenix.Component
      import Phoenix.HTML
      import PikaWeb.CoreComponents
    end
  end

  def controller do
    quote do
      use Phoenix.Controller, formats: [:html, :json]
      import Plug.Conn
    end
  end

  def verified_routes do
    quote do
      use Phoenix.VerifiedRoutes,
        endpoint: PikaWeb.Endpoint,
        router: PikaWeb.Router,
        statics: PikaWeb.static_paths()
    end
  end

  defmacro __using__(which) when is_atom(which), do: apply(__MODULE__, which, [])
end
