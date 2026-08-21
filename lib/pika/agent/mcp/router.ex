defmodule Pika.Agent.MCP.Router do
  @moduledoc "Single MCP transport adapter for every Actor-bound Agent Role."

  import Plug.Conn

  alias Pika.Agent.{Actor, Directory}
  alias Pika.Agent.Role.Tool

  def init(opts), do: opts

  def call(conn, opts) do
    directory = Keyword.get(opts, :directory, Directory)

    with {:ok, conn} <- parse(conn),
         {:ok, token, binding} <- authenticate(conn, directory) do
      dispatch(conn, token, binding)
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
      {:ok, token, binding}
    else
      _ -> {:error, :unauthorized}
    end
  catch
    :exit, _reason -> {:error, :unauthorized}
  end

  defp dispatch(%{method: "POST"} = conn, token, binding) do
    dispatch_rpc(conn, token, binding, conn.body_params)
  end

  defp dispatch(conn, _token, _binding), do: send_resp(conn, 405, "method not allowed")

  defp dispatch_rpc(conn, _token, binding, %{"method" => "initialize", "id" => id}) do
    capabilities =
      if binding.role_id in ["plan", "iteration"] do
        %{
          "tools" => %{"listChanged" => false},
          "resources" => %{"listChanged" => false, "subscribe" => false}
        }
      else
        %{"tools" => %{"listChanged" => false}}
      end

    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => capabilities,
      "serverInfo" => %{"name" => server_name(binding.role_id), "version" => "0.1.0"}
    })
  end

  defp dispatch_rpc(conn, _token, _binding, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _token, _binding, %{"method" => "ping", "id" => id}),
    do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, _token, %{catalog: tools}, %{"method" => "tools/list", "id" => id})
       when is_list(tools),
       do: rpc_result(conn, id, %{"tools" => Enum.map(tools, &tool/1)})

  defp dispatch_rpc(conn, _token, binding, %{"method" => "tools/list", "id" => id}) do
    case actor_call(fn -> Actor.catalog(binding.actor) end) do
      {:ok, tools} -> rpc_result(conn, id, %{"tools" => Enum.map(tools, &tool/1)})
      {:error, reason} -> operation_error(conn, id, reason)
    end
  end

  defp dispatch_rpc(conn, _token, binding, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => name} = params
       }) do
    case actor_call(fn -> Actor.invoke(binding.actor, name, params["arguments"] || %{}) end) do
      {:ok, outcome} -> tool_result(conn, id, outcome.value)
      {:error, reason} -> operation_error(conn, id, reason)
    end
  end

  defp dispatch_rpc(conn, _token, binding, %{"method" => "resources/list", "id" => id}) do
    resources =
      if binding.role_id in ["plan", "iteration"] do
        case Pika.AttemptStore.attempt(binding.work.id) do
          {:ok, %{plan_artifact_id: artifact_id}} when not is_nil(artifact_id) ->
            [
              %{
                "uri" => plan_uri(binding.work.id),
                "name" => "Attempt optimization plan",
                "mimeType" => "text/markdown"
              }
            ]

          _ ->
            []
        end
      else
        []
      end

    rpc_result(conn, id, %{"resources" => resources})
  end

  defp dispatch_rpc(conn, token, binding, %{
         "method" => "resources/read",
         "id" => id,
         "params" => %{"uri" => uri}
       }) do
    with true <- binding.role_id in ["plan", "iteration"],
         {:ok, plan} <- Pika.AttemptCoordinator.read_plan(token),
         true <- uri == plan_uri(plan.attempt_id) do
      rpc_result(conn, id, %{
        "contents" => [%{"uri" => uri, "mimeType" => "text/markdown", "text" => plan.text}]
      })
    else
      _ -> rpc_error(conn, id, -32_002, "resource not found", %{}, 404)
    end
  end

  defp dispatch_rpc(conn, _token, _binding, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", %{}, 404)

  defp dispatch_rpc(conn, _token, _binding, _request),
    do: rpc_error(conn, nil, -32_600, "invalid request", %{}, 400)

  defp actor_call(operation) do
    operation.()
  catch
    :exit, reason -> {:error, {:actor_unavailable, reason}}
  end

  defp tool(%Tool{} = tool) do
    %{
      "name" => tool.name,
      "description" => tool.description,
      "inputSchema" => tool.input_schema
    }
  end

  defp tool_result(conn, id, value) do
    value = Pika.JSONSafe.json_safe(value)

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp operation_error(conn, id, reason) do
    details =
      case reason do
        %Pika.Agent.Role.Error{} = error -> Map.from_struct(error)
        _ -> %{reason: reason}
      end

    rpc_error(conn, id, -32_602, error_message(reason), details, 409)
  end

  defp error_message(%Pika.Agent.Role.Error{} = error), do: "#{error.code}: #{error.message}"
  defp error_message(reason), do: inspect(reason)

  defp server_name("integration"), do: "pika-integration"
  defp server_name("sync"), do: "pika-sync"

  defp server_name(role_id) when role_id in ["alignment", "setup_merge", "baseline"],
    do: "pika-alignment"

  defp server_name(role_id) when role_id in ["plan", "iteration"], do: "pika-optimization"
  defp server_name(role_id), do: "pika-agent-#{role_id}"

  defp plan_uri(attempt_id), do: "pika://attempt/#{attempt_id}/plan"

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
