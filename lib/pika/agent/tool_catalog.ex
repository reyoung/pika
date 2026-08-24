defmodule Pika.Agent.ToolCatalog do
  @moduledoc "JSON MCP tool definitions for the eight v2 Role contracts."

  alias Pika.Optimization.RoleRegistry

  @empty %{"type" => "object", "properties" => %{}, "additionalProperties" => false}
  @idempotency %{
    "idempotency_key" => %{"type" => "string", "minLength" => 1, "maxLength" => 256}
  }

  @spec for_role(atom() | String.t()) :: {:ok, [map()]} | {:error, term()}
  def for_role(role_id) do
    role_id = to_string(role_id)

    with {:ok, definition} <- RoleRegistry.fetch(role_id) do
      tools = Enum.map(definition.tools, &tool(role_id, &1.name, &1.kind))
      {:ok, tools}
    end
  end

  @spec validate() :: :ok | {:error, term()}
  def validate do
    Enum.reduce_while(RoleRegistry.all(), :ok, fn {role_id, definition}, :ok ->
      with {:ok, tools} <- for_role(role_id) do
        expected = Enum.map(definition.tools, & &1.name)
        actual = Enum.map(tools, & &1["name"])

        if expected == actual,
          do: {:cont, :ok},
          else: {:halt, {:error, {:tool_catalog_mismatch, role_id, expected, actual}}}
      else
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
  end

  defp tool(_role, "get_context", :query),
    do: definition("get_context", "返回当前不可变 Context Bundle 的入口与身份。", @empty)

  defp tool(_role, "query_attempt_history", :query) do
    definition(
      "query_attempt_history",
      "查询最近的终态 Attempt 摘要；不返回大型日志。",
      object(%{"limit" => %{"type" => "integer", "minimum" => 0, "maximum" => 100}})
    )
  end

  defp tool("baseline_alignment", "ask_questions", :query) do
    question = %{
      "type" => "object",
      "properties" => %{
        "id" => %{"type" => "string", "minLength" => 1},
        "question" => %{"type" => "string", "minLength" => 1},
        "options" => %{
          "type" => "array",
          "minItems" => 2,
          "maxItems" => 4,
          "items" => %{
            "type" => "object",
            "properties" => %{
              "label" => %{"type" => "string", "minLength" => 1},
              "description" => %{"type" => "string", "minLength" => 1}
            },
            "required" => ["label", "description"],
            "additionalProperties" => false
          }
        }
      },
      "required" => ["id", "question", "options"],
      "additionalProperties" => false
    }

    definition(
      "ask_questions",
      "向用户一次提交一批互相独立的 Baseline 对齐问题，并等待整批答案。",
      object(%{"questions" => %{"type" => "array", "minItems" => 1, "items" => question}}, [
        "questions"
      ])
    )
  end

  defp tool("baseline_alignment", "submit_baseline_definition", :command),
    do:
      command(
        "submit_baseline_definition",
        "提交当前 Baseline Revision 根目录中的 Definition 索引文件。",
        %{"definition_path" => path()}
      )

  defp tool("baseline_verify", "finish_baseline_verification", :command),
    do:
      command(
        "finish_baseline_verification",
        "提交 accepted 或 definition_rejected 的 Baseline Verification Result。",
        %{"result_path" => path()}
      )

  defp tool("iteration", "finish_iteration", :command),
    do:
      command(
        "finish_iteration",
        "提交 ready_for_integration 或 rejected 的 Iteration Result。",
        %{"result_path" => path()}
      )

  defp tool("integration", "prepare_best_update", :command),
    do:
      command(
        "prepare_best_update",
        "验证 Full Case evidence 并在任何 Best mutation 前签发 Git Intent。",
        %{"validation_path" => path()}
      )

  defp tool("integration", "finish_integration", :command),
    do:
      command(
        "finish_integration",
        "提交 Integration Accept/Reject 终态；Accept 会核验实际 Git。",
        %{"result_path" => path()}
      )

  defp tool(role, "submit_followup_message", :command)
       when role in ~w(baseline_verify_followup iteration_followup integration_followup),
       do:
         command(
           "submit_followup_message",
           "提交一条将发送给目标 Agent 的具体 Follow-up User Turn。",
           %{"message" => %{"type" => "string", "minLength" => 1, "maxLength" => 16_384}}
         )

  defp tool("progress_summary", "submit_progress_summary", :command),
    do:
      command(
        "submit_progress_summary",
        "提交当前请求目录中的 summary.md。",
        %{"summary_path" => path()}
      )

  defp definition(name, description, schema) do
    %{"name" => name, "description" => description, "inputSchema" => schema}
  end

  defp command(name, description, properties) do
    properties = Map.merge(@idempotency, properties)
    definition(name, description, object(properties, Map.keys(properties)))
  end

  defp object(properties, required \\ []) do
    %{
      "type" => "object",
      "properties" => properties,
      "required" => required,
      "additionalProperties" => false
    }
  end

  defp path, do: %{"type" => "string", "minLength" => 1}
end
