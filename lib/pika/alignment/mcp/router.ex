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
        "ask_questions",
        "Ask the user one or more blocking clarification questions in one batch. The UI presents them sequentially and returns all answers after the batch is complete.",
        %{
          "questions" => %{
            "type" => "array",
            "minItems" => 1,
            "items" => %{
              "type" => "object",
              "properties" => %{
                "id" => string(),
                "question" => string(),
                "options" => %{
                  "type" => "array",
                  "minItems" => 2,
                  "maxItems" => 4,
                  "items" => %{
                    "type" => "object",
                    "properties" => %{
                      "label" => string(),
                      "description" => string()
                    },
                    "required" => ["label"],
                    "additionalProperties" => false
                  }
                }
              },
              "required" => ~w(id question options),
              "additionalProperties" => false
            }
          }
        },
        ["questions"]
      ),
      tool(
        "register_artifact",
        "Register an existing file under the Alignment Workspace artifacts/ directory. relative_path must be Workspace-relative and start with artifacts/.",
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
        "Submit the current Campaign Spec draft revision. This never confirms the Spec for the user.",
        %{
          "idempotency_key" => string(),
          "spec" => Pika.CampaignSpec.json_schema()
        },
        ~w(idempotency_key spec)
      ),
      tool(
        "submit_harness",
        "Register the immutable Correctness Oracle (when repository-backed), correctness tests, and Benchmark Harness from the setup worktree. The mutable Development entrypoint must not be protected.",
        %{
          "idempotency_key" => string(),
          "oracle_path" => %{"type" => ["string", "null"]},
          "correctness_paths" => %{"type" => "array", "items" => string()},
          "benchmark_path" => string(),
          "protected_paths" => %{"type" => "array", "items" => string()}
        },
        ~w(idempotency_key oracle_path correctness_paths benchmark_path protected_paths)
      ),
      tool(
        "submit_implementation_bundle",
        "Freeze the Optimization Target and bind the mutable Development implementation to a clean setup commit. External Target repositories are prepared asynchronously in workspace/refs; the frozen Target is stored under workspace/targets.",
        %{
          "idempotency_key" => string(),
          "setup_sha" => string()
        },
        ~w(idempotency_key setup_sha)
      ),
      tool(
        "submit_implementation_review",
        "Submit one reviewable smoke run showing that the frozen Optimization Target and Development implementation both pass the Correctness Oracle on the same Benchmark Case, plus paired performance Metrics. This is review evidence, not the full Baseline.",
        %{
          "idempotency_key" => string(),
          "schema_version" => %{"type" => "integer", "const" => 2},
          "spec_revision" => %{"type" => "integer", "minimum" => 1},
          "target_snapshot_id" => string(),
          "development_sha" => string(),
          "harness_digest" => string(),
          "case_id" => string(),
          "command" => string(),
          "environment" => string(),
          "exit_code" => %{"type" => "integer", "const" => 0},
          "correctness" => %{
            "type" => "object",
            "properties" => %{
              "target_passed" => %{"type" => "boolean", "const" => true},
              "development_passed" => %{"type" => "boolean", "const" => true}
            },
            "required" => ~w(target_passed development_passed),
            "additionalProperties" => false
          },
          "metrics" => %{
            "type" => "array",
            "minItems" => 1,
            "items" => %{
              "type" => "object",
              "properties" => %{
                "metric_id" => string(),
                "target_value" => %{"type" => "number", "exclusiveMinimum" => 0},
                "development_value" => %{"type" => "number", "exclusiveMinimum" => 0},
                "unit" => string(),
                "sample_count" => %{"type" => "integer", "minimum" => 1}
              },
              "required" => ~w(metric_id target_value development_value unit sample_count),
              "additionalProperties" => false
            }
          },
          "output_artifact" => string(),
          "summary" => string()
        },
        ~w(idempotency_key schema_version spec_revision target_snapshot_id development_sha harness_digest case_id command environment exit_code correctness metrics output_artifact summary)
      ),
      tool(
        "complete_setup_merge",
        "Report the Agent-owned squash merge into the temporary pika/best branch.",
        %{
          "idempotency_key" => string(),
          "base_sha" => string(),
          "setup_sha" => string(),
          "best_sha" => string(),
          "target_snapshot_id" => string()
        },
        ~w(idempotency_key base_sha setup_sha best_sha target_snapshot_id)
      ),
      tool(
        "reopen_baseline_definition",
        "Return an active Baseline workflow to a new editable Campaign Spec revision when the confirmed Reference, Harness or measurement definition cannot produce a valid Baseline. This stops the current Baseline work and hands the reason and requested changes to the Alignment Agent; it is not for transient execution failures.",
        %{
          "idempotency_key" => string(),
          "reason" => string(),
          "requested_changes" => string()
        },
        ~w(idempotency_key reason requested_changes)
      ),
      tool(
        "submit_baseline",
        "Submit one local schema-v2 Baseline manifest Artifact. The large samples remain in the Workspace and are streamed from disk; do not send sample records through MCP.",
        %{
          "idempotency_key" => string(),
          "manifest_artifact" => string()
        },
        ~w(idempotency_key manifest_artifact)
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

  defp tool(name, description, properties, required \\ [], schema_extensions \\ %{}) do
    %{
      "name" => name,
      "description" => description,
      "inputSchema" =>
        Map.merge(
          %{
            "type" => "object",
            "properties" => properties,
            "required" => required,
            "additionalProperties" => false
          },
          schema_extensions
        )
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
