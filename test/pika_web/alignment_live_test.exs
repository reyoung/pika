defmodule PikaWeb.AlignmentLiveTest do
  use PikaWeb.ConnCase, async: false

  alias Pika.PreviewAuth, as: Auth
  alias Pika.Alignment.{Campaign, Workspace}
  alias Pika.Test.AlignmentFixtures

  setup do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    Auth.clear()
    %{token: token} = Auth.generate()
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
        mcp_token: "mcp-test",
        skill: %{
          name: "ncu-report-skill",
          url: "test",
          sha: String.duplicate("d", 40),
          path: skill_dir
        }
      )

    on_exit(fn ->
      Auth.clear()
      if Process.alive?(pid), do: GenServer.stop(pid)
    end)

    %{token: token, workspace: workspace}
  end

  test "requires the startup token and exchanges it for a session cookie", %{
    conn: conn,
    token: token
  } do
    assert conn |> get("/") |> response(401) =~ "tokenized URL"
    conn = get(conn, "/?token=#{token}")
    assert redirected_to(conn) == "/"

    conn = recycle(conn)
    assert {:ok, _view, html} = live(conn, "/")
    assert html =~ "Alignment → GPU Baseline Preview"
    assert html =~ "DraftingSpec"
    assert html =~ "未完成项不是错误"
    assert html =~ ~s(phx-hook="ConversationScroll")
    refute html =~ "at least one target Metric"
    assert length(String.split(html, "/assets/app.js")) - 1 == 1
  end

  test "sends text and attachments through the unified Composer", %{
    conn: conn,
    token: token,
    workspace: workspace
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")

    upload =
      file_input(view, "#message-form", :inputs, [
        %{name: "shapes.jsonl", content: "{\"n\":128}\n", type: "application/json"}
      ])

    assert is_binary(render_upload(upload, "shapes.jsonl"))

    view
    |> form("#message-form", message: %{body: "目标机器是 H20", intent: "conversation"})
    |> render_submit()

    assert render(view) =~ "目标机器是 H20"
    assert render(view) =~ "shapes.jsonl"
    assert File.ls!(Path.join(workspace.artifacts, "inputs")) != []
  end

  test "allows an attachment-only Composer message", %{conn: conn, token: token} do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")

    upload =
      file_input(view, "#message-form", :inputs, [
        %{name: "shape.pkl", content: "fixture", type: "application/octet-stream"}
      ])

    render_upload(upload, "shape.pkl")
    view |> form("#message-form", message: %{body: "", intent: "conversation"}) |> render_submit()
    assert render(view) =~ "shape.pkl"
  end

  test "keeps Composer text and postponed attachments when Campaign submission fails", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")

    upload =
      file_input(view, "#message-form", :inputs, [
        %{name: "retry.jsonl", content: "{\"n\":1}\n", type: "application/json"}
      ])

    render_upload(upload, "retry.jsonl")
    GenServer.stop(Process.whereis(Campaign))

    html =
      view
      |> form("#message-form", message: %{body: "retry this message", intent: "conversation"})
      |> render_submit()

    assert html =~ "retry this message"
    assert html =~ "retry.jsonl"
    assert html =~ "not_started"
  end

  test "renders five human-readable review sections without raw Shape JSON", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, html} = live(conn, "/")
    assert html =~ "优化验收单"
    assert html =~ "目标边界"
    assert html =~ "Metrics"
    assert html =~ "Benchmark Cases"
    assert html =~ "测量与采样规则"
    assert html =~ "Reference Projects"
    assert html =~ "全量 5 Pair · 异常升级 30 Pair"

    view |> element(~s(button[phx-value-target="metrics"])) |> render_click()
    html = render(view)
    assert html =~ "请重新检查并修改 Metrics"
    assert html =~ ~s(value="request_changes")
  end

  test "renders Backend execution as one collapsed gray activity row", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")

    send(
      Process.whereis(Campaign),
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:tool_started, :fake, "session", %{
         data: %{
           item: %{
             "id" => "command-1",
             "type" => "commandExecution",
             "command" => "rg -n kernel lib",
             "commandActions" => [%{"type" => "read", "path" => "lib"}]
           }
         }
       })}
    )

    assert eventually(fn -> render(view) =~ "activity-row" end)
    html = render(view)
    assert html =~ "<details"
    assert html =~ "读取文件 · 运行了命令"
    assert html =~ "rg -n kernel lib"
    refute html =~ "Agent Activity"
  end

  test "copies Agent source Markdown and shows a typing indicator for an active Turn", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")
    campaign = Process.whereis(Campaign)

    send(
      campaign,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_started, :fake, "copy-session", %{
         turn_id: "copy-turn"
       })}
    )

    assert eventually(fn -> render(view) =~ "Agent 正在输入" end)

    send(
      campaign,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:message_delta, :fake, "copy-session", %{
         turn_id: "copy-turn",
         data: %{delta: "**原始** `Markdown`"}
       })}
    )

    assert eventually(fn -> render(view) =~ ~s(phx-hook="CopyMarkdown") end)
    html = render(view)
    assert html =~ ~s(phx-hook="CopyMarkdown")
    assert html =~ ~s(data-markdown="**原始** `Markdown`")
    assert html =~ "<strong>原始</strong>"

    send(
      campaign,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_completed, :fake, "copy-session", %{
         turn_id: "copy-turn"
       })}
    )

    assert eventually(fn -> not (render(view) =~ "Agent 正在输入") end)
  end

  test "renders an MCP question batch sequentially and returns all answers", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    {:ok, view, _html} = live(conn, "/")

    task =
      Task.async(fn ->
        Campaign.mcp_call("mcp-test", "ask_questions", %{
          "questions" => [
            %{
              "id" => "correctness",
              "question" => "正确性容差使用哪一档？",
              "options" => [
                %{"label" => "严格", "description" => "rtol 1e-5"},
                %{"label" => "宽松", "description" => "rtol 1e-3"}
              ]
            },
            %{
              "id" => "fusion_scope",
              "question" => "是否包含 epilogue？",
              "options" => [
                %{"label" => "包含", "description" => "允许融合"},
                %{"label" => "不包含", "description" => "仅核心算子"}
              ]
            }
          ]
        })
      end)

    assert eventually(fn -> render(view) =~ "正确性容差使用哪一档？" end)
    html = render(view)
    assert html =~ "严格"
    assert html =~ "宽松"
    assert html =~ "输入自己的回答"
    assert html =~ "Agent 正在等待你的选择"
    assert html =~ "问题 1 / 2"
    refute html =~ ~s(id="message-form")

    view
    |> element(".question-options button", "严格")
    |> render_click()

    refute Task.yield(task, 20)
    assert eventually(fn -> render(view) =~ "是否包含 epilogue？" end)
    assert render(view) =~ "问题 2 / 2"

    view
    |> form(".question-custom", %{"answer" => "使用 rtol 5e-4"})
    |> render_submit()

    assert {:ok,
            %{
              answers: [
                %{question_id: "correctness", answer: "严格", selected_option: "严格"},
                %{
                  question_id: "fusion_scope",
                  answer: "使用 rtol 5e-4",
                  selected_option: nil
                }
              ]
            }} = Task.await(task)

    assert render(view) =~ "使用 rtol 5e-4"
    refute render(view) =~ "请选择一个答案"
  end

  test "serves the local favicon without entering the authenticated router", %{conn: conn} do
    conn = get(conn, "/favicon.svg")
    assert response(conn, 200) =~ "<svg"
  end

  test "rejects an authenticated request after the server rotates its startup token", %{
    conn: conn,
    token: token
  } do
    conn = conn |> get("/?token=#{token}") |> recycle()
    _new_registration = Auth.generate()
    assert conn |> get("/") |> response(401) =~ "tokenized URL"
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
