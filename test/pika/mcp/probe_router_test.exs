defmodule Pika.MCP.ProbeRouterTest do
  use ExUnit.Case, async: false

  import Plug.Conn
  import Plug.Test

  alias Pika.MCP.{ProbeRouter, ProbeState}

  setup do
    ProbeState.reset()
    registration = ProbeState.register_session(%{backend: :router_test})
    %{registration: registration}
  end

  test "implements initialize, tools/list and both probe tools", %{registration: registration} do
    assert %{"result" => %{"serverInfo" => %{"name" => "pika-backend-probe"}}} =
             rpc(registration.token, %{
               "jsonrpc" => "2.0",
               "id" => 1,
               "method" => "initialize",
               "params" => %{"protocolVersion" => "2025-06-18"}
             })

    assert %{"result" => %{"tools" => tools}} =
             rpc(registration.token, %{"jsonrpc" => "2.0", "id" => 2, "method" => "tools/list"})

    assert Enum.map(tools, & &1["name"]) == ["get_probe_context", "complete_probe"]

    assert %{"result" => %{"structuredContent" => %{"nonce" => nonce}}} =
             rpc(registration.token, %{
               "jsonrpc" => "2.0",
               "id" => 3,
               "method" => "tools/call",
               "params" => %{"name" => "get_probe_context", "arguments" => %{}}
             })

    request = %{
      "jsonrpc" => "2.0",
      "id" => 4,
      "method" => "tools/call",
      "params" => %{
        "name" => "complete_probe",
        "arguments" => %{"idempotency_key" => "router", "nonce" => nonce, "summary" => "done"}
      }
    }

    assert %{"result" => %{"structuredContent" => first}} = rpc(registration.token, request)
    assert %{"result" => %{"structuredContent" => ^first}} = rpc(registration.token, request)
  end

  test "rejects missing bearer tokens" do
    conn =
      conn(
        :post,
        "/mcp",
        Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"})
      )

    conn = ProbeRouter.call(conn, ProbeRouter.init([]))
    assert conn.status == 401
  end

  defp rpc(token, message) do
    conn =
      :post
      |> conn("/mcp", Jason.encode!(message))
      |> put_req_header("content-type", "application/json")
      |> put_req_header("accept", "application/json, text/event-stream")
      |> put_req_header("authorization", "Bearer #{token}")
      |> ProbeRouter.call(ProbeRouter.init([]))

    assert conn.status == 200
    Jason.decode!(conn.resp_body)
  end
end
