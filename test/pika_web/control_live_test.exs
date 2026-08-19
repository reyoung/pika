defmodule PikaWeb.ControlLiveTest do
  use PikaWeb.ConnCase, async: false

  alias Pika.Auth
  alias Pika.Test.OptimizationFixtures

  setup do
    previous_mode = Application.get_env(:pika, :runtime_mode)
    Application.put_env(:pika, :runtime_mode, :serve)
    context = OptimizationFixtures.setup_campaign(max_attempts: 3)
    Auth.clear()
    %{token: token, marker: marker} = Auth.generate()

    on_exit(fn ->
      Auth.clear()
      OptimizationFixtures.stop_repo()

      if previous_mode,
        do: Application.put_env(:pika, :runtime_mode, previous_mode),
        else: Application.delete_env(:pika, :runtime_mode)
    end)

    %{context: context, token: token, marker: marker}
  end

  test "Control LiveView exposes Attempts, Metrics, Sync, audit, and explicit Stop confirmation",
       %{
         conn: conn,
         marker: marker,
         context: context
       } do
    {:ok, attempt} = Pika.AttemptStore.create_attempt(context.campaign.id, 0)

    assert {:ok, _event} =
             Pika.AttemptStore.record_metrics(
               attempt.id,
               [
                 %{
                   case_id: "target_case",
                   metric_id: "latency_us",
                   value: 9.8,
                   baseline_value: 10.0,
                   improvement_ratio: 0.02,
                   mad: 0.001,
                   noise_tolerance: 0.005,
                   pair_count: 30,
                   valid_pair_count: 30
                 }
               ],
               context.best_sha
             )

    session = %Pika.AgentBackend.Session{
      id: Ecto.UUID.generate(),
      backend: :codex_app_server,
      backend_protocol: "codex-app-server",
      backend_session_id: "provider-session",
      cwd: context.workspace.repo,
      model: "gpt-test",
      reasoning_effort: :high,
      jsonl_path: "/dev/null"
    }

    identity = %{
      attempt_id: attempt.id,
      role: :iteration,
      slot_index: 0,
      token_hash: String.duplicate("a", 64)
    }

    assert :ok =
             Pika.AttemptStore.insert_session(
               context.campaign.id,
               identity,
               session,
               %{},
               []
             )

    log_path = "artifacts/logs/#{attempt.id}/#{session.id}.jsonl"

    assert {:ok, artifact} =
             Pika.ArtifactStore.append_jsonl(
               context.workspace,
               log_path,
               %{
                 at: DateTime.utc_now() |> DateTime.to_iso8601(),
                 type: "message_delta",
                 data: %{delta: "Inspecting the kernel"}
               },
               %{
                 campaign_id: context.campaign.id,
                 owner_type: "attempt",
                 owner_id: attempt.id,
                 kind: "agent_jsonl"
               }
             )

    assert :ok = Pika.AttemptStore.attach_session_log(session.id, artifact.id)

    conn = init_test_session(conn, %{pika_auth: marker})

    {:ok, view, html} =
      live_isolated(conn, PikaWeb.ControlLive, session: %{"pika_auth" => marker})

    assert html =~ "Attempts"
    assert html =~ "Stop Now"
    assert html =~ "Agent events"
    assert html =~ "Inspecting the kernel"
    assert html =~ log_path
    metrics_html = view |> element("button[phx-value-tab='metrics']") |> render_click()
    assert metrics_html =~ "Metrics Timeline"
    assert metrics_html =~ "Spec Revision"
    assert metrics_html =~ "Selected point"
    assert metrics_html =~ "latency_us"

    assert has_element?(view, "#metrics-chart[data-points]")

    assert view
           |> form(".metric-filters", %{
             "metric_filter" => "latency_us",
             "case_filter" => "target_case",
             "spec_filter" => "1"
           })
           |> render_change() =~ "1 points"

    assert view |> element("button[phx-value-tab='sync']") |> render_click() =~
             "明确预览，明确确认"

    assert view |> element("button[phx-value-tab='audit']") |> render_click() =~
             "用户动作与状态变更审计"

    view |> element("button", "Stop Now") |> render_click()
    assert has_element?(view, "button[phx-click='stop_now']", "确认")

    {:ok, _reconnected_view, reconnected_html} =
      live_isolated(conn, PikaWeb.ControlLive, session: %{"pika_auth" => marker})

    assert reconnected_html =~ "Inspecting the kernel"
    assert reconnected_html =~ log_path
  end

  test "JSON API is bearer protected, idempotent, and excludes MCP credentials", %{
    conn: conn,
    token: token
  } do
    assert conn |> get("/api/control") |> response(401) =~ "unauthorized"

    conn =
      conn
      |> put_req_header("authorization", "Bearer #{token}")
      |> get("/api/control")

    body = json_response(conn, 200)
    assert body["control"]["campaign"]["status"] == "optimizing"
    refute inspect(body) =~ "mcp_token"
    refute inspect(body) =~ "protocol_config"

    request =
      build_conn()
      |> put_req_header("authorization", "Bearer #{token}")
      |> put_req_header("content-type", "application/json")
      |> put_req_header("idempotency-key", "api-pause")
      |> post("/api/control/pause", Jason.encode!(%{}))

    assert json_response(request, 200)["status"] == "paused"

    replay =
      build_conn()
      |> put_req_header("authorization", "Bearer #{token}")
      |> put_req_header("content-type", "application/json")
      |> put_req_header("idempotency-key", "api-pause")
      |> post("/api/control/pause", Jason.encode!(%{}))

    assert json_response(replay, 200)["status"] == "paused"
  end
end
