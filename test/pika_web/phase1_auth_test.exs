defmodule PikaWeb.Phase1AuthTest do
  use PikaWeb.ConnCase, async: false

  alias Pika.Auth

  setup do
    previous_mode = Application.get_env(:pika, :runtime_mode)
    Application.put_env(:pika, :runtime_mode, :serve)
    Auth.clear()
    %{token: token} = Auth.generate()

    on_exit(fn ->
      Auth.clear()

      if previous_mode,
        do: Application.put_env(:pika, :runtime_mode, previous_mode),
        else: Application.delete_env(:pika, :runtime_mode)
    end)

    %{token: token}
  end

  test "protects HTML and exchanges the URL token for a strict HttpOnly cookie", %{
    conn: conn,
    token: token
  } do
    assert conn |> get("/") |> response(401) =~ "pika serve"
    conn = get(conn, "/?token=#{token}")
    assert redirected_to(conn) == "/"
    [cookie] = get_resp_header(conn, "set-cookie")
    assert cookie =~ "_pika="
    assert cookie =~ "HttpOnly"
    assert cookie =~ "SameSite=Strict"
    refute get_resp_header(conn, "location") |> List.first() =~ "token="
    refute File.read!("assets/js/app.js") =~ "localStorage"
  end

  test "protects JSON, SSE, MCP, and the LiveView mount", %{conn: conn, token: token} do
    assert conn |> get("/api/status") |> response(401) =~ "unauthorized"
    assert conn |> get("/events") |> response(401) =~ "unauthorized"

    request = Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "initialize"})

    assert conn
           |> put_req_header("content-type", "application/json")
           |> post("/mcp", request)
           |> response(401) =~ "unauthorized"

    conn =
      conn
      |> put_req_header("content-type", "application/json")
      |> put_req_header("authorization", "Bearer #{token}")
      |> post("/mcp", request)

    assert %{"result" => %{"serverInfo" => %{"name" => "pika"}}} =
             Jason.decode!(response(conn, 200))

    assert {:ok, redirected} = PikaWeb.DashboardLive.mount(%{}, %{}, %Phoenix.LiveView.Socket{})
    assert redirected.redirected
  end

  test "rotation invalidates old HTTP and MCP Bearer tokens", %{conn: conn, token: old_token} do
    %{token: new_token} = Auth.generate()
    refute new_token == old_token

    assert conn |> get("/?token=#{old_token}") |> response(401) =~ "pika serve"

    assert conn
           |> put_req_header("authorization", "Bearer #{old_token}")
           |> get("/api/status")
           |> response(401) =~ "unauthorized"

    request = Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "initialize"})

    assert conn
           |> put_req_header("content-type", "application/json")
           |> put_req_header("authorization", "Bearer #{old_token}")
           |> post("/mcp", request)
           |> response(401) =~ "unauthorized"
  end
end
