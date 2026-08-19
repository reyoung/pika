defmodule Pika.MCP.Router do
  @moduledoc false

  use Plug.Router

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :authenticate
  plug :match
  plug :dispatch

  post _ do
    dispatch_rpc(conn, conn.body_params)
  end

  match _ do
    send_resp(conn, 405, "method not allowed")
  end

  defp authenticate(conn, _opts) do
    authorization = get_req_header(conn, "authorization") |> List.first()

    if Pika.Auth.bearer_authenticated?(authorization) do
      conn
    else
      rpc_error(conn, nil, -32_001, "unauthorized", 401) |> halt()
    end
  end

  defp dispatch_rpc(conn, %{"method" => "initialize", "id" => id}) do
    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{"tools" => %{"listChanged" => false}},
      "serverInfo" => %{"name" => "pika", "version" => "0.0.1"}
    })
  end

  defp dispatch_rpc(conn, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, %{"method" => "ping", "id" => id}), do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, %{"method" => "tools/list", "id" => id}) do
    rpc_result(conn, id, %{
      "tools" => [
        %{
          "name" => "get_server_status",
          "description" => "Read the Phase 1 Pika Workspace and Campaign recovery status.",
          "inputSchema" => %{
            "type" => "object",
            "properties" => %{},
            "additionalProperties" => false
          }
        }
      ]
    })
  end

  defp dispatch_rpc(conn, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => "get_server_status"}
       }) do
    value = stringify(Pika.Runtime.snapshot())

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp dispatch_rpc(conn, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", 404)

  defp dispatch_rpc(conn, _request), do: rpc_error(conn, nil, -32_600, "invalid request", 400)

  defp rpc_result(conn, id, result),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp rpc_error(conn, id, code, message, status),
    do:
      json(conn, status, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "error" => %{"code" => code, "message" => message}
      })

  defp json(conn, status, value) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(status, Jason.encode!(value))
  end

  defp stringify(%_{} = struct), do: struct |> Map.from_struct() |> stringify()

  defp stringify(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify(value)} end)

  defp stringify(list) when is_list(list), do: Enum.map(list, &stringify/1)
  defp stringify(value) when is_atom(value), do: Atom.to_string(value)
  defp stringify(value), do: value
end
