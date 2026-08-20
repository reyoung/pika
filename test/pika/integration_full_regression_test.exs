defmodule Pika.IntegrationFullRegressionTest do
  use ExUnit.Case, async: false

  alias Pika.{
    AttemptCoordinator,
    AttemptStore,
    Git,
    IntegrationCoordinator,
    IntegrationStore,
    Repo
  }

  alias Pika.Test.{AttemptAgentBackend, IntegrationAgentBackend, OptimizationFixtures}

  setup do
    on_exit(fn -> OptimizationFixtures.stop_repo() end)
    :ok
  end

  test "FIFO Integration refreshes stale bases, rejects regressions before Git, and advances Best safely" do
    context = ready_attempts(3) |> OptimizationFixtures.add_guard_case()
    attempts = AttemptStore.attempts(context.campaign.id, limit: 10) |> Enum.sort_by(& &1.ordinal)
    assert Enum.all?(attempts, &(&1.status == "ready_for_integration"))

    coordinator =
      start_integration(
        context,
        integration_profile(%{test_pid: self(), barrier: true, regress_ordinals: [3]})
      )

    [first] = receive_integrations(1)
    assert first.attempt_id == Enum.at(attempts, 0).id
    assert_integration_mcp_gateway(first.token)
    refute_receive {:integration_started, _, _, _}, 100
    send(first.task_pid, :release)

    [second] = receive_integrations(1)
    assert second.attempt_id == Enum.at(attempts, 1).id
    {:ok, accepted_first} = AttemptStore.attempt(first.attempt_id)
    assert accepted_first.status == "accepted"
    send(second.task_pid, :release)

    [third] = receive_integrations(1)
    assert third.attempt_id == Enum.at(attempts, 2).id
    {:ok, accepted_second} = AttemptStore.attempt(second.attempt_id)
    assert accepted_second.status == "accepted"
    assert accepted_second.base_sha == accepted_first.accepted_sha
    send(third.task_pid, :release)

    eventually(fn ->
      statuses =
        AttemptStore.attempts(context.campaign.id, limit: 10)
        |> Map.new(&{&1.ordinal, &1.status})

      statuses == %{1 => "accepted", 2 => "accepted", 3 => "rejected"}
    end)

    final = AttemptStore.attempts(context.campaign.id, limit: 10) |> Enum.sort_by(& &1.ordinal)
    [accepted_one, accepted_two, rejected] = final
    best_sha = Pika.Persistence.current_campaign().best_sha
    assert best_sha == accepted_two.accepted_sha
    assert best_sha == Git.run!(context.workspace.repo, ["rev-parse", "refs/heads/pika/best"])
    refute best_sha == rejected.candidate_sha

    assert {:ok, %{status: "passed"}} = IntegrationStore.receipt_for_attempt(accepted_one.id)
    assert {:ok, %{status: "passed"}} = IntegrationStore.receipt_for_attempt(accepted_two.id)
    assert {:ok, rejected_receipt} = IntegrationStore.receipt_for_attempt(rejected.id)
    assert rejected_receipt.status == "rejected"
    assert rejected_receipt.regressed_case_ids == ["guard_case"]

    assert AttemptStore.metrics_for_attempt(accepted_one.id)
           |> Enum.map(&{&1.case_id, &1.source})
           |> Enum.sort() ==
             [{"guard_case", "integration_screen"}, {"target_case", "integration_screen"}]

    assert AttemptStore.metrics_for_attempt(rejected.id)
           |> Enum.map(&{&1.case_id, &1.source})
           |> Enum.sort() ==
             [{"guard_case", "integration_full"}, {"target_case", "integration_screen"}]

    guard_metric = Enum.find(rejected_receipt.metrics, &(&1["case_id"] == "guard_case"))
    assert guard_metric["source"] == "integration_full"
    assert guard_metric["pair_count"] == 7
    assert guard_metric["valid_pair_count"] == 7

    [[sampling_sequence]] =
      Repo.query!(
        "SELECT MAX(sequence) FROM sampling_revisions WHERE campaign_id = ?",
        [context.campaign.id]
      ).rows

    assert sampling_sequence == 2

    [[feedback_evidence]] =
      Repo.query!(
        "SELECT src.evidence_json FROM sampling_revision_cases src JOIN benchmark_cases bc ON bc.id = src.benchmark_case_id JOIN sampling_revisions sr ON sr.id = src.sampling_revision_id WHERE sr.campaign_id = ? AND sr.sequence = 2 AND bc.name = 'guard_case'",
        [context.campaign.id]
      ).rows

    assert Jason.decode!(feedback_evidence)["representative_reason"] =~ "shape family"

    [[sampled_count]] =
      Repo.query!(
        "SELECT COUNT(*) FROM sampling_revision_cases WHERE sampling_revision_id = (SELECT id FROM sampling_revisions WHERE campaign_id = ? ORDER BY sequence DESC LIMIT 1)",
        [context.campaign.id]
      ).rows

    assert sampled_count == 2
    assert {:error, :integration_lease_missing} = IntegrationStore.lease(context.campaign.id)

    [[best_revision_count]] =
      Repo.query!("SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ?", [
        context.campaign.id
      ]).rows

    assert best_revision_count == 3

    [[best_notifications]] =
      Repo.query!(
        "SELECT COUNT(*) FROM agent_messages WHERE campaign_id = ? AND scope = 'best_advanced'",
        [context.campaign.id]
      ).rows

    [[sampling_notifications]] =
      Repo.query!(
        "SELECT COUNT(*) FROM agent_messages WHERE campaign_id = ? AND scope = 'sampling_advanced'",
        [context.campaign.id]
      ).rows

    assert best_notifications >= 2
    assert sampling_notifications >= 1

    eventually(fn ->
      Enum.all?(final, fn attempt ->
        not File.exists?(Path.join(context.workspace.root, attempt.worktree_relative_path)) and
          match?(
            {:error, _},
            Git.run(context.workspace.repo, [
              "show-ref",
              "--verify",
              "refs/heads/#{attempt.branch_name}"
            ])
          )
      end)
    end)

    assert IntegrationCoordinator.snapshot(coordinator).last_error == nil
  end

  test "a crash after squash recovers the durable Lease, Receipt, and Intent without a second merge" do
    context = ready_attempts(1)
    {:ok, crash_counter} = Agent.start_link(fn -> 0 end)
    {:ok, measurement_counter} = Agent.start_link(fn -> %{} end)

    coordinator =
      start_integration(
        context,
        integration_profile(%{
          test_pid: self(),
          crash_stage: :after_squash,
          crash_counter: crash_counter,
          measurement_counter: measurement_counter
        })
      )

    starts = receive_integrations(2)
    assert starts |> Enum.map(& &1.attempt_id) |> Enum.uniq() == [hd(starts).attempt_id]
    assert starts |> Enum.map(& &1.token) |> Enum.uniq() |> length() == 2

    eventually(fn ->
      match?({:ok, %{status: "accepted"}}, AttemptStore.attempt(hd(starts).attempt_id))
    end)

    [[best_revisions, merge_intents, receipts]] =
      Repo.query!(
        "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ?), (SELECT COUNT(*) FROM operation_intents WHERE campaign_id = ? AND kind = 'merge'), (SELECT COUNT(*) FROM full_regression_receipts WHERE campaign_id = ?)",
        [context.campaign.id, context.campaign.id, context.campaign.id]
      ).rows

    assert best_revisions == 2
    assert merge_intents == 1
    assert receipts == 1
    assert Agent.get(measurement_counter, & &1) == %{screening: 1}
    assert IntegrationCoordinator.snapshot(coordinator).recovery_count == 1
    assert {:error, :integration_lease_missing} = IntegrationStore.lease(context.campaign.id)
  end

  test "MCP authentication is available while the Integration Session is opening" do
    context = ready_attempts(1)

    coordinator =
      start_integration(
        context,
        integration_profile(%{
          test_pid: self(),
          barrier: true,
          authorize_during_open: true
        })
      )

    [start] = receive_integrations(1)
    assert :ok = IntegrationCoordinator.authorize(start.token, coordinator)
    assert_integration_mcp_gateway(start.token)
    send(start.task_pid, :release)

    eventually(fn ->
      match?({:ok, %{status: "accepted"}}, AttemptStore.attempt(start.attempt_id))
    end)
  end

  for crash_stage <- [
        :after_lease,
        :during_screening,
        :after_receipt,
        :after_intent,
        :before_sqlite_commit
      ] do
    test "recovers safely from #{crash_stage} without duplicate durable Integration records" do
      context = ready_attempts(1)
      {:ok, crash_counter} = Agent.start_link(fn -> 0 end)
      {:ok, measurement_counter} = Agent.start_link(fn -> %{} end)

      coordinator =
        start_integration(
          context,
          integration_profile(%{
            test_pid: self(),
            crash_stage: unquote(crash_stage),
            crash_counter: crash_counter,
            measurement_counter: measurement_counter
          })
        )

      starts = receive_integrations(2)
      attempt_id = hd(starts).attempt_id
      assert Enum.uniq(Enum.map(starts, & &1.attempt_id)) == [attempt_id]

      eventually(fn ->
        match?({:ok, %{status: "accepted"}}, AttemptStore.attempt(attempt_id))
      end)

      [[best_revisions, merge_intents, receipts]] =
        Repo.query!(
          "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ?), (SELECT COUNT(*) FROM operation_intents WHERE campaign_id = ? AND kind = 'merge'), (SELECT COUNT(*) FROM full_regression_receipts WHERE campaign_id = ?)",
          [context.campaign.id, context.campaign.id, context.campaign.id]
        ).rows

      assert {best_revisions, merge_intents, receipts} == {2, 1, 1}
      assert Agent.get(measurement_counter, & &1) == %{screening: 1}
      assert IntegrationCoordinator.snapshot(coordinator).recovery_count == 1
      assert {:error, :integration_lease_missing} = IntegrationStore.lease(context.campaign.id)
    end
  end

  test "a crash immediately after the SQLite merge transaction never duplicates acceptance" do
    context = ready_attempts(1)
    {:ok, crash_counter} = Agent.start_link(fn -> 0 end)

    coordinator =
      start_integration(
        context,
        integration_profile(%{
          test_pid: self(),
          crash_stage: :after_sqlite_commit,
          crash_counter: crash_counter
        })
      )

    [start] = receive_integrations(1)
    attempt_id = start.attempt_id

    eventually(fn ->
      match?({:ok, %{status: "accepted"}}, AttemptStore.attempt(attempt_id))
    end)

    refute_receive {:integration_started, ^attempt_id, _, _}, 150

    [[best_revisions, verified_intents, receipts, best_events]] =
      Repo.query!(
        "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ?), (SELECT COUNT(*) FROM operation_intents WHERE campaign_id = ? AND kind = 'merge' AND state = 'verified'), (SELECT COUNT(*) FROM full_regression_receipts WHERE campaign_id = ?), (SELECT COUNT(*) FROM domain_events WHERE aggregate_id = ? AND event_type = 'best_advanced')",
        [
          context.campaign.id,
          context.campaign.id,
          context.campaign.id,
          context.campaign.id
        ]
      ).rows

    assert {best_revisions, verified_intents, receipts, best_events} == {2, 1, 1, 1}
    assert {:error, :integration_lease_missing} = IntegrationStore.lease(context.campaign.id)
    assert IntegrationCoordinator.snapshot(coordinator).session == nil
  end

  test "an unexplained Best worktree mutation blocks the Campaign before starting Integration" do
    context = ready_attempts(1)
    unexpected_path = Path.join(context.workspace.repo, "unexpected.txt")
    File.write!(unexpected_path, "external mutation\n")
    Git.run!(context.workspace.repo, ["add", "unexpected.txt"])
    Git.run!(context.workspace.repo, ["commit", "-m", "Unexpected external mutation"])
    unexpected_sha = Git.run!(context.workspace.repo, ["rev-parse", "HEAD"])
    refute unexpected_sha == context.best_sha

    coordinator = start_integration(context, integration_profile(%{test_pid: self()}))

    eventually(fn -> Pika.Persistence.current_campaign().status == "blocked" end)
    refute_receive {:integration_started, _, _, _}, 150

    snapshot = IntegrationCoordinator.snapshot(coordinator)
    assert snapshot.session == nil
    assert snapshot.last_error =~ "unexplained_best_state"

    [[blocked_events]] =
      Repo.query!(
        "SELECT COUNT(*) FROM domain_events WHERE aggregate_id = ? AND event_type = 'integration_blocked'",
        [context.campaign.id]
      ).rows

    assert blocked_events == 1
  end

  test "recovers during the user-sized escalation and rejects before mutating Best" do
    context = ready_attempts(1) |> OptimizationFixtures.add_guard_case()
    {:ok, crash_counter} = Agent.start_link(fn -> 0 end)
    {:ok, measurement_counter} = Agent.start_link(fn -> %{} end)

    _coordinator =
      start_integration(
        context,
        integration_profile(%{
          test_pid: self(),
          crash_stage: :during_escalation,
          crash_counter: crash_counter,
          measurement_counter: measurement_counter,
          regress_ordinals: [1]
        })
      )

    starts = receive_integrations(2)
    attempt_id = hd(starts).attempt_id

    eventually(fn ->
      match?({:ok, %{status: "rejected"}}, AttemptStore.attempt(attempt_id))
    end)

    [[best_revisions, receipts, sampling_revisions]] =
      Repo.query!(
        "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ?), (SELECT COUNT(*) FROM full_regression_receipts WHERE campaign_id = ?), (SELECT COUNT(*) FROM sampling_revisions WHERE campaign_id = ?)",
        [context.campaign.id, context.campaign.id, context.campaign.id]
      ).rows

    assert {best_revisions, receipts, sampling_revisions} == {1, 1, 2}
    assert Agent.get(measurement_counter, & &1) == %{escalation: 1, screening: 1}
    assert Pika.Persistence.current_campaign().best_sha == context.best_sha
    assert {:error, :integration_lease_missing} = IntegrationStore.lease(context.campaign.id)
  end

  defp ready_attempts(count) do
    context = OptimizationFixtures.setup_campaign(max_attempts: count)

    {:ok, coordinator} =
      AttemptCoordinator.start_link(
        workspace: context.workspace,
        campaign: context.campaign,
        profiles: attempt_profiles(count),
        backend_modules: %{codex_app_server: AttemptAgentBackend},
        mcp_url: "http://127.0.0.1:18080/mcp"
      )

    Process.unlink(coordinator)

    eventually(fn ->
      attempts = AttemptStore.attempts(context.campaign.id, limit: count + 1)
      length(attempts) == count and Enum.all?(attempts, &(&1.status == "ready_for_integration"))
    end)

    GenServer.stop(coordinator)
    context
  end

  defp start_integration(context, profile) do
    {:ok, coordinator} =
      IntegrationCoordinator.start_link(
        workspace: context.workspace,
        campaign: context.campaign,
        profile: profile,
        backend_modules: %{codex_app_server: IntegrationAgentBackend},
        mcp_url: "http://127.0.0.1:18080/mcp"
      )

    Process.unlink(coordinator)

    on_exit(fn ->
      if Process.alive?(coordinator) do
        try do
          GenServer.stop(coordinator)
        catch
          :exit, _ -> :ok
        end
      end
    end)

    coordinator
  end

  defp attempt_profiles(count) do
    for index <- 1..count do
      %{
        "name" => "slot-#{index}",
        "backend" => "codex_app_server",
        "command" => ["codex", "app-server", "--listen", "stdio://"],
        "reasoning_effort" => "high",
        "env" => %{},
        "protocol_config" => %{}
      }
    end
  end

  defp integration_profile(env) do
    %{
      "name" => "integration",
      "backend" => "codex_app_server",
      "command" => ["codex", "app-server", "--listen", "stdio://"],
      "reasoning_effort" => "high",
      "env" => env,
      "protocol_config" => %{}
    }
  end

  defp receive_integrations(count) do
    for _ <- 1..count do
      assert_receive {:integration_started, attempt_id, task_pid, token}, 5_000
      %{attempt_id: attempt_id, task_pid: task_pid, token: token}
    end
  end

  defp assert_integration_mcp_gateway(token) do
    conn =
      Plug.Test.conn(
        :post,
        "/mcp",
        Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "initialize"})
      )
      |> Plug.Conn.put_req_header("content-type", "application/json")
      |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
      |> PikaWeb.MCPGateway.call([])

    assert %{"result" => %{"serverInfo" => %{"name" => "pika-integration"}}} =
             Jason.decode!(conn.resp_body)

    context =
      Plug.Test.conn(
        :post,
        "/mcp",
        Jason.encode!(%{
          "jsonrpc" => "2.0",
          "id" => 2,
          "method" => "tools/call",
          "params" => %{"name" => "get_integration_context", "arguments" => %{}}
        })
      )
      |> Plug.Conn.put_req_header("content-type", "application/json")
      |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
      |> PikaWeb.MCPGateway.call([])

    assert context.status == 200

    assert %{"result" => %{"structuredContent" => %{"best_metrics" => best_metrics}}} =
             Jason.decode!(context.resp_body)

    assert map_size(best_metrics) >= 1
  end

  defp eventually(fun, attempts \\ 160)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.() do
      :ok
    else
      Process.sleep(25)
      eventually(fun, attempts - 1)
    end
  end

  defp eventually(fun, 0), do: flunk("condition did not become true: #{inspect(fun.())}")
end
