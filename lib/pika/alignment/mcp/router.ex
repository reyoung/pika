defmodule Pika.Alignment.MCP.Router do
  @moduledoc false

  use Plug.Router

  plug Plug.Parsers, parsers: [:json], pass: ["application/json"], json_decoder: Jason
  plug :match
  plug :dispatch

  post _ do
    with {:ok, token} <- bearer_token(conn) do
      dispatch_rpc(conn, token, conn.body_params)
    else
      _ -> rpc_error(conn, nil, -32_001, "unauthorized", 401)
    end
  end

  match _ do
    send_resp(conn, 405, "method not allowed")
  end

  defp dispatch_rpc(conn, token, %{"method" => "initialize", "id" => id}) do
    case Pika.Alignment.Campaign.authorize(token) do
      :ok ->
        rpc_result(conn, id, %{
          "protocolVersion" => "2025-06-18",
          "capabilities" => %{"tools" => %{"listChanged" => false}},
          "serverInfo" => %{"name" => "pika-alignment", "version" => "0.1.0"}
        })

      _ ->
        rpc_error(conn, id, -32_001, "unauthorized", 401)
    end
  end

  defp dispatch_rpc(conn, token, %{"method" => "notifications/initialized"}) do
    case Pika.Alignment.Campaign.authorize(token) do
      :ok -> send_resp(conn, 202, "")
      _ -> rpc_error(conn, nil, -32_001, "unauthorized", 401)
    end
  end

  defp dispatch_rpc(conn, token, %{"method" => "ping", "id" => id}) do
    case Pika.Alignment.Campaign.authorize(token) do
      :ok -> rpc_result(conn, id, %{})
      _ -> rpc_error(conn, id, -32_001, "unauthorized", 401)
    end
  end

  defp dispatch_rpc(conn, token, %{"method" => "tools/list", "id" => id}) do
    case Pika.Alignment.Campaign.authorize(token) do
      :ok -> rpc_result(conn, id, %{"tools" => tools()})
      _ -> rpc_error(conn, id, -32_001, "unauthorized", 401)
    end
  end

  defp dispatch_rpc(conn, token, %{
         "method" => "tools/call",
         "id" => id,
         "params" => %{"name" => name} = params
       }) do
    arguments = Map.get(params, "arguments", %{})

    case Pika.Alignment.Campaign.mcp_call(token, name, arguments) do
      {:ok, result} -> tool_result(conn, id, result)
      {:error, code, message, details} -> tool_error(conn, id, code, message, details)
      {:error, reason} -> tool_error(conn, id, "internal_error", inspect(reason), %{})
    end
  end

  defp dispatch_rpc(conn, _token, %{"id" => id}),
    do: rpc_error(conn, id, -32_601, "method not found", 404)

  defp dispatch_rpc(conn, _token, _request),
    do: rpc_error(conn, nil, -32_600, "invalid request", 400)

  defp tools do
    [
      tool(
        "get_context",
        "Read the current Alignment Campaign state and required Boundary operations.",
        %{}
      ),
      tool(
        "register_artifact",
        "Register an existing file under the Alignment Workspace.",
        %{
          "idempotency_key" => string(),
          "kind" => string(),
          "relative_path" => string(),
          "sha256" => string(),
          "size" => %{"type" => "integer"},
          "mime" => string(),
          "metadata" => %{"type" => "object"}
        },
        ~w(idempotency_key kind relative_path sha256 size mime)
      ),
      tool(
        "submit_spec",
        "Submit Campaign Spec v1. This never confirms the Spec for the user.",
        %{
          "idempotency_key" => string(),
          "spec" => Pika.CampaignSpec.json_schema()
        },
        ~w(idempotency_key spec)
      ),
      tool(
        "submit_harness",
        "Register Reference, correctness tests and Benchmark Harness from the setup worktree.",
        %{
          "idempotency_key" => string(),
          "reference_path" => string(),
          "correctness_paths" => %{"type" => "array", "items" => string()},
          "benchmark_path" => string(),
          "protected_paths" => %{"type" => "array", "items" => string()}
        },
        ~w(idempotency_key reference_path correctness_paths benchmark_path protected_paths)
      ),
      tool(
        "complete_setup_merge",
        "Report the Agent-owned squash merge into the temporary pika/best branch.",
        %{
          "idempotency_key" => string(),
          "base_sha" => string(),
          "setup_sha" => string(),
          "best_sha" => string()
        },
        ~w(idempotency_key base_sha setup_sha best_sha)
      ),
      tool(
        "submit_baseline",
        "Submit raw self-paired Baseline, correctness and profiler Artifacts for Pika recomputation.",
        %{
          "idempotency_key" => string(),
          "measured_sha" => string(),
          "samples_artifact" => string(),
          "correctness_artifact" => string(),
          "profiler_artifact" => string(),
          "summary" => string()
        },
        ~w(idempotency_key measured_sha samples_artifact correctness_artifact profiler_artifact summary)
      ),
      tool(
        "submit_iteration_sample",
        "Select the initial Iteration Sample Set after Pika accepts the complete Baseline.",
        %{
          "idempotency_key" => string(),
          "case_ids" => %{
            "type" => "array",
            "items" => string(),
            "minItems" => 1,
            "maxItems" => 10
          },
          "reasons" => %{"type" => "object", "additionalProperties" => string()},
          "estimated_cost" => %{
            "type" => "object",
            "properties" => %{
              "iteration_seconds" => %{"type" => "number", "minimum" => 0},
              "full_seconds" => %{"type" => "number", "exclusiveMinimum" => 0},
              "savings_ratio" => %{"type" => "number", "minimum" => 0, "maximum" => 1}
            },
            "required" => ~w(iteration_seconds full_seconds savings_ratio),
            "additionalProperties" => false
          },
          "summary" => string()
        },
        ~w(idempotency_key case_ids reasons estimated_cost summary)
      )
    ]
  end

  defp tool(name, description, properties, required \\ []) do
    %{
      "name" => name,
      "description" => description,
      "inputSchema" => %{
        "type" => "object",
        "properties" => properties,
        "required" => required,
        "additionalProperties" => false
      }
    }
  end

  defp string, do: %{"type" => "string"}

  defp tool_result(conn, id, value) do
    value = Pika.JSONSafe.json_safe(value)

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => false
    })
  end

  defp tool_error(conn, id, code, message, details) do
    value = %{
      "code" => to_string(code),
      "message" => message,
      "details" => Pika.JSONSafe.json_safe(details)
    }

    rpc_result(conn, id, %{
      "content" => [%{"type" => "text", "text" => Jason.encode!(value)}],
      "structuredContent" => value,
      "isError" => true
    })
  end

  defp rpc_result(conn, id, result),
    do: json(conn, 200, %{"jsonrpc" => "2.0", "id" => id, "result" => result})

  defp rpc_error(conn, id, code, message, status),
    do:
      json(conn, status, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "error" => %{"code" => code, "message" => message}
      })

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
end
