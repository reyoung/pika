defmodule Pika.Agent.MCP.Router do
  @moduledoc "JSON-RPC MCP transport for v2 Actor-bound Role tokens."

  import Plug.Conn

  alias Pika.Agent.Directory

  def init(opts), do: opts

  def call(conn, opts) do
    directory = Keyword.get(opts, :directory, Directory)
    invoker = Keyword.get(opts, :invoker, &Pika.Agent.Actor.invoke/3)

    with {:ok, conn} <- parse(conn),
         {:ok, binding} <- authenticate(conn, directory) do
      dispatch(conn, binding, invoker)
    else
      {:error, :invalid_json, conn} -> rpc_error(conn, nil, -32_600, "invalid request", %{}, 400)
      {:error, :unauthorized} -> rpc_error(conn, nil, -32_001, "unauthorized", %{}, 401)
    end
  end

  defp parse(%Plug.Conn{body_params: %Plug.Conn.Unfetched{}} = conn) do
    parser =
      Plug.Parsers.init(
        parsers: [:json],
        pass: ["application/json"],
        json_decoder: Jason
      )

    {:ok, Plug.Parsers.call(conn, parser)}
  rescue
    Plug.Parsers.ParseError -> {:error, :invalid_json, conn}
  end

  defp parse(conn), do: {:ok, conn}

  defp authenticate(conn, directory) do
    with ["Bearer " <> token] when token != "" <- get_req_header(conn, "authorization"),
         {:ok, binding} <- Directory.lookup(token, directory) do
      {:ok, binding}
    else
      _other -> {:error, :unauthorized}
    end
  catch
    :exit, _reason -> {:error, :unauthorized}
  end

  defp dispatch(%{method: "POST"} = conn, binding, invoker),
    do: dispatch_rpc(conn, binding, invoker, conn.body_params)

  defp dispatch(conn, _binding, _invoker), do: send_resp(conn, 405, "method not allowed")

  defp dispatch_rpc(conn, binding, _invoker, %{"method" => "initialize", "id" => id}) do
    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{"tools" => %{"listChanged" => false}},
      "serverInfo" => %{"name" => "pika-v2-#{binding.role_id}", "version" => "0.2.0"}
    })
  end

  defp dispatch_rpc(conn, _binding, _invoker, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _binding, _invoker, %{"method" => "ping", "id" => id}),
    do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, binding, _invoker, %{"method" => "tools/list", "id" => id}),
    do: rpc_result(conn, id, %{"tools" => binding.catalog})

  defp dispatch_rpc(
         conn,
         binding,
         invoker,
         %{"method" => "tools/call", "id" => id, "params" => %{"name" => name}} = request
       ) do
    arguments = get_in(request, ["params", "arguments"]) || %{}

    case invoke(invoker, binding.actor, name, arguments) do
      {:ok, value} -> tool_result(conn, id, value)
      {:error, reason} -> operation_error(conn, id, reason)
    end
  end

  defp dispatch_rpc(conn, _binding, _invoker, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", %{}, 404)

  defp dispatch_rpc(conn, _binding, _invoker, _request),
    do: rpc_error(conn, nil, -32_600, "invalid request", %{}, 400)

  defp invoke(invoker, actor, name, arguments) do
    invoker.(actor, name, arguments)
  catch
    :exit, reason -> {:error, {:actor_unavailable, reason}}
  end

  defp tool_result(conn, id, value) do
    value = Pika.JSONSafe.json_safe(value)

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp operation_error(conn, id, reason),
    do: rpc_error(conn, id, -32_602, inspect(reason), %{reason: reason}, 409)

  defp rpc_result(conn, id, result),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp rpc_error(conn, id, code, message, details, status) do
    json(conn, status, %{
      "jsonrpc" => "2.0",
      "id" => id,
      "error" => %{
        "code" => code,
        "message" => message,
        "data" => Pika.JSONSafe.json_safe(details)
      }
    })
  end

  defp json(conn, status, value) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(status, Jason.encode!(value))
  end
end
