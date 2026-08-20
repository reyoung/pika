defmodule Pika.Optimization.MCP.Router do
  @moduledoc false

  use Plug.Router

  alias Pika.AttemptTokenRegistry, as: TokenRegistry

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :authenticate
  plug :match
  plug :dispatch

  post _ do
    dispatch_rpc(conn, conn.assigns.attempt_token, conn.body_params)
  end

  match _ do
    send_resp(conn, 405, "method not allowed")
  end

  defp authenticate(conn, _opts) do
    with ["Bearer " <> token] <- get_req_header(conn, "authorization"),
         :ok <- Pika.AttemptCoordinator.authorize(token) do
      assign(conn, :attempt_token, token)
    else
      _ -> rpc_error(conn, nil, -32_001, "unauthorized", %{}, 401) |> halt()
    end
  end

  defp dispatch_rpc(conn, _token, %{"method" => "initialize", "id" => id}) do
    rpc_result(conn, id, %{
      "protocolVersion" => "2025-06-18",
      "capabilities" => %{
        "tools" => %{"listChanged" => false},
        "resources" => %{"listChanged" => false, "subscribe" => false}
      },
      "serverInfo" => %{"name" => "pika-optimization", "version" => "0.1.0"}
    })
  end

  defp dispatch_rpc(conn, _token, %{"method" => "notifications/initialized"}),
    do: send_resp(conn, 202, "")

  defp dispatch_rpc(conn, _token, %{"method" => "ping", "id" => id}),
    do: rpc_result(conn, id, %{})

  defp dispatch_rpc(conn, token, %{"method" => "tools/list", "id" => id}) do
    case TokenRegistry.lookup(token) do
      {:ok, %{role: role}} -> rpc_result(conn, id, %{"tools" => tools(role)})
      error -> mcp_response(conn, id, error)
    end
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => name} = params
       }) do
    mcp_response(
      conn,
      id,
      Pika.AttemptCoordinator.mcp_call(token, name, params["arguments"] || %{})
    )
  end

  defp dispatch_rpc(conn, token, %{"method" => "resources/list", "id" => id}) do
    with {:ok, %{attempt_id: attempt_id}} <- TokenRegistry.lookup(token),
         {:ok, attempt} <- Pika.AttemptStore.attempt(attempt_id) do
      resources =
        if attempt.plan_artifact_id do
          [
            %{
              "uri" => plan_uri(attempt.id),
              "name" => "Attempt optimization plan",
              "mimeType" => "text/markdown"
            }
          ]
        else
          []
        end

      rpc_result(conn, id, %{"resources" => resources})
    else
      error ->
        mcp_response(conn, id, error)
    end
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "resources/read",
         "id" => id,
         "params" => %{"uri" => uri}
       }) do
    with {:ok, plan} <- Pika.AttemptCoordinator.read_plan(token),
         true <- uri == plan_uri(plan.attempt_id) do
      rpc_result(conn, id, %{
        "contents" => [
          %{"uri" => uri, "mimeType" => "text/markdown", "text" => plan.text}
        ]
      })
    else
      _ -> rpc_error(conn, id, -32_002, "resource not found", %{}, 404)
    end
  end

  defp dispatch_rpc(conn, _token, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", %{}, 404)

  defp dispatch_rpc(conn, _token, _request),
    do: rpc_error(conn, nil, -32_600, "invalid request", %{}, 400)

  defp tools(role) do
    shared = [
      tool("get_context", "Read the immutable Campaign and own Attempt context."),
      tool(
        "query_attempt_history",
        "Query older terminal Attempts without inflating the Prompt."
      ),
      tool("get_attempt", "Read this Attempt or another terminal Attempt."),
      tool("list_agents", "List Agent Sessions in this Campaign."),
      tool("read_agent_messages", "Read unacknowledged Mailbox messages."),
      write_tool("ack_agent_messages", "Acknowledge Mailbox messages."),
      write_tool("send_agent_message", "Persist a direct message to another Agent Session."),
      write_tool("register_artifact", "Register an existing Workspace Artifact.")
    ]

    role_tools =
      case role do
        :plan ->
          [write_tool("submit_plan", "Atomically publish the Attempt plan Markdown.")]

        "plan" ->
          [write_tool("submit_plan", "Atomically publish the Attempt plan Markdown.")]

        _ ->
          [
            write_tool("record_metrics", "Submit formal alternating-pair measurements."),
            write_tool("submit_attempt_summary", "Submit the structured Attempt summary."),
            write_tool("complete_attempt", "Run the completion gate for this Attempt.")
          ]
      end

    shared ++ role_tools
  end

  defp tool(name, description) do
    %{
      "name" => name,
      "description" => description,
      "inputSchema" => %{
        "type" => "object",
        "properties" => %{},
        "additionalProperties" => true
      }
    }
  end

  defp write_tool(name, description) do
    schema = tool(name, description)

    put_in(schema, ["inputSchema"], %{
      "type" => "object",
      "properties" => %{"idempotency_key" => %{"type" => "string", "minLength" => 1}},
      "required" => ["idempotency_key"],
      "additionalProperties" => true
    })
  end

  defp plan_uri(attempt_id), do: "pika://attempt/#{attempt_id}/plan"

  defp mcp_response(conn, id, {:ok, value}), do: tool_result(conn, id, value)

  defp mcp_response(conn, id, {:error, code, message, details}),
    do: rpc_error(conn, id, -32_602, "#{code}: #{message}", details, 409)

  defp mcp_response(conn, id, {:error, reason}),
    do: rpc_error(conn, id, -32_602, inspect(reason), %{}, 409)

  defp tool_result(conn, id, value) do
    value = Pika.JSONSafe.json_safe(value)

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
        "error" => %{
          "code" => code,
          "message" => message,
          "data" => Pika.JSONSafe.json_safe(details)
        }
      })

  defp json(conn, status, value) do
    conn
    |> put_resp_content_type("application/json")
    |> send_resp(status, Jason.encode!(value))
  end
end
