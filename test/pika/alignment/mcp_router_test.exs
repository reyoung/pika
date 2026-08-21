defmodule Pika.Alignment.MCP.RouterTest do
  use ExUnit.Case, async: false

  import Plug.Conn
  import Plug.Test

  alias Pika.Alignment.{Campaign, Workspace}
  alias Pika.Test.AlignmentFixtures

  @token "alignment-http-mcp-token"

  setup do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    repo = AlignmentFixtures.git_repo()
    {:ok, workspace} = Workspace.prepare(repo)
    skill_dir = Path.join(workspace.root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)

    File.write!(
      Path.join(skill_dir, "SKILL.md"),
      "---\nname: ncu-report-skill\ndescription: test\n---\n"
    )

    {:ok, pid} =
      Campaign.start_link(
        workspace: workspace,
        start_backend: false,
        resolve_references: false,
        mcp_url: "http://127.0.0.1:1/mcp",
        mcp_token: @token,
        skill: %{
          name: "ncu-report-skill",
          url: "test",
          sha: String.duplicate("e", 40),
          path: skill_dir
        }
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)
    :ok
  end

  test "authenticates initialize and exposes the Boundary tool contract" do
    assert %{"result" => %{"serverInfo" => %{"name" => "pika-alignment"}}} =
             rpc(@token, %{
               "jsonrpc" => "2.0",
               "id" => 1,
               "method" => "initialize",
               "params" => %{}
             })

    assert %{"result" => %{"tools" => tools}} =
             rpc(@token, %{"jsonrpc" => "2.0", "id" => 2, "method" => "tools/list"})

    assert Enum.map(tools, & &1["name"]) == [
             "get_context",
             "ask_questions",
             "register_artifact",
             "submit_spec",
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "complete_setup_merge",
             "reopen_baseline_definition",
             "submit_baseline",
             "submit_iteration_sample"
           ]

    refute Enum.any?(tools, &(&1["name"] == "ask_question"))

    baseline_tool = Enum.find(tools, &(&1["name"] == "submit_baseline"))
    baseline_schema = baseline_tool["inputSchema"]
    assert baseline_schema["required"] == ~w(idempotency_key manifest_artifact)
    refute Map.has_key?(baseline_schema, "oneOf")

    reopen_tool = Enum.find(tools, &(&1["name"] == "reopen_baseline_definition"))

    assert reopen_tool["inputSchema"]["required"] ==
             ~w(idempotency_key reason requested_changes)

    bundle_tool = Enum.find(tools, &(&1["name"] == "submit_implementation_bundle"))
    assert bundle_tool["inputSchema"]["required"] == ~w(idempotency_key setup_sha)

    review_tool = Enum.find(tools, &(&1["name"] == "submit_implementation_review"))
    assert get_in(review_tool, ["inputSchema", "properties", "metrics", "minItems"]) == 1
    assert get_in(review_tool, ["inputSchema", "properties", "exit_code", "const"]) == 0
    assert get_in(review_tool, ["inputSchema", "properties", "schema_version", "const"]) == 2

    questions_tool = Enum.find(tools, &(&1["name"] == "ask_questions"))
    questions_schema = get_in(questions_tool, ["inputSchema", "properties", "questions"])
    assert questions_schema["minItems"] == 1
    refute Map.has_key?(questions_schema, "maxItems")

    assert %{"result" => %{"structuredContent" => %{"required_operations" => required}}} =
             rpc(@token, %{
               "jsonrpc" => "2.0",
               "id" => 3,
               "method" => "tools/call",
               "params" => %{"name" => "get_context", "arguments" => %{}}
             })

    assert required == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]
  end

  test "rejects a missing bearer token" do
    conn =
      :post
      |> conn("/", Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"}))
      |> put_req_header("content-type", "application/json")
      |> Pika.Alignment.MCP.Router.call(Pika.Alignment.MCP.Router.init([]))

    assert conn.status == 401
  end

  test "keeps ask_questions open until the UI returns every answer" do
    task =
      Task.async(fn ->
        rpc(@token, %{
          "jsonrpc" => "2.0",
          "id" => 4,
          "method" => "tools/call",
          "params" => %{
            "name" => "ask_questions",
            "arguments" => %{
              "questions" => [
                %{
                  "id" => "fusion_scope",
                  "question" => "选择融合边界",
                  "options" => [
                    %{"label" => "核心算子"},
                    %{"label" => "包含 epilogue", "description" => "允许额外融合"}
                  ]
                },
                %{
                  "id" => "layout",
                  "question" => "选择输入布局",
                  "options" => [%{"label" => "连续"}, %{"label" => "分块"}]
                }
              ]
            }
          }
        })
      end)

    assert eventually(fn -> not is_nil(Campaign.snapshot().pending_question) end)
    question = Campaign.snapshot().pending_question
    assert :ok = Campaign.answer_question(question.id, "包含 epilogue", "option-2")
    refute Task.yield(task, 20)

    question = Campaign.snapshot().pending_question
    assert question.question == "选择输入布局"
    assert :ok = Campaign.answer_question(question.id, "连续", "option-1")

    assert %{
             "result" => %{
               "isError" => false,
               "structuredContent" => %{
                 "answers" => [
                   %{
                     "question_id" => "fusion_scope",
                     "answer" => "包含 epilogue",
                     "selected_option" => "包含 epilogue"
                   },
                   %{
                     "question_id" => "layout",
                     "answer" => "连续",
                     "selected_option" => "连续"
                   }
                 ]
               }
             }
           } = Task.await(task)
  end

  defp rpc(token, request) do
    conn =
      :post
      |> conn("/", Jason.encode!(request))
      |> put_req_header("content-type", "application/json")
      |> put_req_header("authorization", "Bearer #{token}")
      |> Pika.Alignment.MCP.Router.call(Pika.Alignment.MCP.Router.init([]))

    assert conn.status == 200
    Jason.decode!(conn.resp_body)
  end

  defp eventually(fun, attempts \\ 30)
  defp eventually(fun, 0), do: fun.()

  defp eventually(fun, attempts) do
    if fun.() do
      true
    else
      Process.sleep(10)
      eventually(fun, attempts - 1)
    end
  end
end
