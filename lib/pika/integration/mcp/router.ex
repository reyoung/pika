defmodule Pika.Integration.MCP.Router do
  @moduledoc false

  use Plug.Router

  @tools ~w(get_integration_context register_artifact acquire_integration_lease complete_refresh submit_full_regression reject_attempt create_merge_intent complete_merge)

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :authenticate
  plug :match
  plug :dispatch

  post _ do
    dispatch_rpc(conn, conn.assigns.integration_token, conn.body_params)
  end

  match(_, do: send_resp(conn, 405, "method not allowed"))

  defp authenticate(conn, _opts) do
    with ["Bearer " <> token] <- get_req_header(conn, "authorization"),
         :ok <- Pika.IntegrationCoordinator.authorize(token) do
      assign(conn, :integration_token, token)
    else
      _ -> json(conn, 401, error(nil, -32_001, "unauthorized", %{})) |> halt()
    end
  end

  defp dispatch_rpc(conn, _token, %{"method" => "initialize", "id" => id}) do
    result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{"tools" => %{"listChanged" => false}},
      "serverInfo" => %{"name" => "pika-integration", "version" => "0.1.0"}
    })
  end

  defp dispatch_rpc(conn, _token, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _token, %{"method" => "ping", "id" => id}), do: result(conn, id, %{})

  defp dispatch_rpc(conn, _token, %{"method" => "tools/list", "id" => id}) do
    tools =
      Enum.map(@tools, fn name ->
        %{
          "name" => name,
          "description" => "Pika serial Integration operation: #{name}",
          "inputSchema" => %{
            "type" => "object",
            "properties" => %{
              "idempotency_key" => %{"type" => "string", "minLength" => 1}
            },
            "additionalProperties" => true
          }
        }
      end)

    result(conn, id, %{"tools" => tools})
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => name} = params
       }) do
    case Pika.IntegrationCoordinator.mcp_call(token, name, params["arguments"] || %{}) do
      {:ok, value} ->
        tool_result(conn, id, value)

      {:error, code, message, details} ->
        json(conn, 409, error(id, -32_602, "#{code}: #{message}", details))

      {:error, reason} ->
        json(conn, 409, error(id, -32_602, inspect(reason), %{}))
    end
  end

  defp dispatch_rpc(conn, _token, %{"id" => id}),
    do: json(conn, 404, error(id, -32_601, "method not found", %{}))

  defp dispatch_rpc(conn, _token, _),
    do: json(conn, 400, error(nil, -32_600, "invalid request", %{}))

  defp tool_result(conn, id, value) do
    value = stringify(value)

    result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp result(conn, id, value),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => value})

  defp error(id, code, message, details),
    do: %{
      "jsonrpc" => "2.0",
      "id" => id,
      "error" => %{"code" => code, "message" => message, "data" => stringify(details)}
    }

  defp json(conn, status, value),
    do:
      conn |> put_resp_content_type("application/json") |> send_resp(status, Jason.encode!(value))

  defp stringify(%_{} = value), do: value |> Map.from_struct() |> stringify()

  defp stringify(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify(value)} end)

  defp stringify(list) when is_list(list), do: Enum.map(list, &stringify/1)
  defp stringify(tuple) when is_tuple(tuple), do: tuple |> Tuple.to_list() |> stringify()
  defp stringify(value) when is_atom(value), do: Atom.to_string(value)
  defp stringify(value), do: value
end
