defmodule Pika.Sync.MCP.Router do
  @moduledoc false

  use Plug.Router

  @tools ~w(get_sync_context register_artifact report_sync_candidate submit_sync_validation create_sync_intent complete_sync)
  @write_tools @tools -- ["get_sync_context"]

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :authenticate
  plug :match
  plug :dispatch

  post _ do
    dispatch_rpc(conn, conn.assigns.sync_token, conn.body_params)
  end

  match _ do
    send_resp(conn, 405, "method not allowed")
  end

  defp authenticate(conn, _opts) do
    with ["Bearer " <> token] <- get_req_header(conn, "authorization"),
         :ok <- Pika.SyncCoordinator.authorize(token) do
      assign(conn, :sync_token, token)
    else
      _ -> rpc_error(conn, nil, -32_001, "unauthorized", %{}, 401) |> halt()
    end
  end

  defp dispatch_rpc(conn, _token, %{"method" => "initialize", "id" => id}) do
    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{"tools" => %{"listChanged" => false}},
      "serverInfo" => %{"name" => "pika-sync", "version" => "0.1.0"}
    })
  end

  defp dispatch_rpc(conn, _token, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _token, %{"method" => "ping", "id" => id}),
    do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, _token, %{"method" => "tools/list", "id" => id}),
    do: rpc_result(conn, id, %{"tools" => Enum.map(@tools, &tool/1)})

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => name} = params
       }) do
    mcp_response(conn, id, Pika.SyncCoordinator.mcp_call(token, name, params["arguments"] || %{}))
  end

  defp dispatch_rpc(conn, _token, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", %{}, 404)

  defp dispatch_rpc(conn, _token, _request),
    do: rpc_error(conn, nil, -32_600, "invalid request", %{}, 400)

  defp tool(name) do
    required = if(name in @write_tools, do: ["idempotency_key"], else: [])

    %{
      "name" => name,
      "description" => description(name),
      "inputSchema" => %{
        "type" => "object",
        "properties" => %{"idempotency_key" => %{"type" => "string", "minLength" => 1}},
        "required" => required,
        "additionalProperties" => true
      }
    }
  end

  defp description("get_sync_context"),
    do: "Read the user-confirmed Sync Run and frozen Campaign context."

  defp description("register_artifact"), do: "Register a Sync validation Artifact."

  defp description("report_sync_candidate"),
    do: "Report the merged candidate and protected-input digest."

  defp description("submit_sync_validation"),
    do: "Submit Full Case Set correctness and paired Best metrics."

  defp description("create_sync_intent"), do: "Persist the external-state intent before push."

  defp description("complete_sync"),
    do: "Perform verified fast-forward push and advance local Best."

  defp mcp_response(conn, id, {:ok, value}), do: tool_result(conn, id, value)

  defp mcp_response(conn, id, {:error, code, message, details}),
    do: rpc_error(conn, id, -32_602, "#{code}: #{message}", details, 409)

  defp mcp_response(conn, id, {:error, reason}),
    do: rpc_error(conn, id, -32_602, inspect(reason), %{}, 409)

  defp tool_result(conn, id, value) do
    value = stringify(value)

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp rpc_result(conn, id, result),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp rpc_error(conn, id, code, message, details, status),
    do:
      json(conn, status, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "error" => %{"code" => code, "message" => message, "data" => stringify(details)}
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
  defp stringify(tuple) when is_tuple(tuple), do: tuple |> Tuple.to_list() |> stringify()
  defp stringify(value) when is_atom(value), do: Atom.to_string(value)
  defp stringify(value), do: value
end
