defmodule Pika.MCP.ProbeRouter do
  @moduledoc "Minimal stateless Streamable HTTP MCP endpoint used by backend conformance."

  use Plug.Router

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :match
  plug :dispatch

  post "/mcp" do
    with {:ok, token} <- bearer_token(conn),
         {:ok, _context} <- Pika.MCP.ProbeState.get_context(token) do
      dispatch_rpc(conn, token, conn.body_params)
    else
      _ -> rpc_error(conn, nil, -32_001, "unauthorized", 401)
    end
  end

  get "/mcp" do
    send_resp(
      conn,
      405,
      "Streamable HTTP notifications are not retained by the conformance probe"
    )
  end

  match _ do
    send_resp(conn, 404, "not found")
  end

  defp dispatch_rpc(conn, _token, %{"method" => "initialize", "id" => id}) do
    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{"tools" => %{"listChanged" => false}},
      "serverInfo" => %{"name" => "pika-backend-probe", "version" => "0.0.1"}
    })
  end

  defp dispatch_rpc(conn, _token, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _token, %{"method" => "ping", "id" => id}),
    do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, _token, %{"method" => "tools/list", "id" => id}) do
    rpc_result(conn, id, %{"tools" => tools()})
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => "get_probe_context"}
       }) do
    case Pika.MCP.ProbeState.get_context(token) do
      {:ok, context} -> tool_result(conn, id, context)
      {:error, reason} -> rpc_error(conn, id, -32_001, to_string(reason), 401)
    end
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => "complete_probe", "arguments" => arguments}
       }) do
    case Pika.MCP.ProbeState.complete(
           token,
           arguments["idempotency_key"],
           arguments["nonce"],
           arguments["summary"]
         ) do
      {:ok, completion} -> tool_result(conn, id, completion)
      {:error, reason} -> rpc_error(conn, id, -32_602, to_string(reason), 409)
    end
  end

  defp dispatch_rpc(conn, _token, %{"id" => id, "method" => method}),
    do: rpc_error(conn, id, -32_601, "method not found: #{method}", 404)

  defp dispatch_rpc(conn, _token, _), do: rpc_error(conn, nil, -32_600, "invalid request", 400)

  defp tools do
    [
      %{
        "name" => "get_probe_context",
        "description" =>
          "Return this Pika Backend Session identity and the nonce required by complete_probe.",
        "inputSchema" => %{
          "type" => "object",
          "properties" => %{},
          "additionalProperties" => false
        }
      },
      %{
        "name" => "complete_probe",
        "description" =>
          "Mandatory backend conformance gate. Call exactly once after reading the injected skill.",
        "inputSchema" => %{
          "type" => "object",
          "properties" => %{
            "idempotency_key" => %{"type" => "string"},
            "nonce" => %{"type" => "string"},
            "summary" => %{"type" => "string"}
          },
          "required" => ["idempotency_key", "nonce", "summary"],
          "additionalProperties" => false
        }
      }
    ]
  end

  defp tool_result(conn, id, value) do
    json_value = stringify(value)

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(json_value)}],
      "structuredContent" => json_value,
      "isError" => false
    })
  end

  defp rpc_result(conn, id, result),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp rpc_error(conn, id, code, message, status) do
    json(conn, status, %{
      "jsonrpc" => "2.0",
      "id" => id,
      "error" => %{"code" => code, "message" => message}
    })
  end

  defp json(conn, status, body) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(status, Jason.encode!(body))
  end

  defp bearer_token(conn) do
    case get_req_header(conn, "authorization") do
      ["Bearer " <> token] when token != "" -> {:ok, token}
      _ -> {:error, :missing_bearer_token}
    end
  end

  defp stringify(%_{} = struct), do: struct |> Map.from_struct() |> stringify()

  defp stringify(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify(value)} end)

  defp stringify(list) when is_list(list), do: Enum.map(list, &stringify/1)
  defp stringify(value), do: value
end
