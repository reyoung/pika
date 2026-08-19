defmodule PikaWeb.AlignmentLiveTest do
  use PikaWeb.ConnCase, async: false

  alias Pika.Stage0.{Auth, Campaign, Workspace}
  alias Pika.Test.Stage0Fixtures

  setup do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    Auth.clear()
    %{token: token} = Auth.generate()
    repo = Stage0Fixtures.git_repo()
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
