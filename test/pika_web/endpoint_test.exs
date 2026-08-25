defmodule PikaWeb.EndpointTest do
  use ExUnit.Case, async: true

  test "checks websocket origins against the current request" do
    assert PikaWeb.Endpoint.config(:check_origin) == :conn
  end
end
