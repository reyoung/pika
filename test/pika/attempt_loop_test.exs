defmodule Pika.AttemptLoopTest do
  use ExUnit.Case, async: false

  alias Pika.{AttemptCoordinator, AttemptStore, Git, Repo}
  alias Pika.Test.{AttemptAgentBackend, OptimizationFixtures}

  setup do
    on_exit(fn -> OptimizationFixtures.stop_repo() end)
    :ok
  end

  test "three fixed slots complete independent Attempts without advancing Best" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 3)
    coordinator = start_coordinator(context, profiles(3, %{test_pid: self(), barrier: true}))

    starts = receive_starts(3)
    assert starts |> Enum.map(& &1.attempt_id) |> Enum.uniq() |> length() == 3

    snapshot = AttemptCoordinator.snapshot(coordinator)
    assert Enum.sort(Enum.map(snapshot.attempts, & &1.status)) == ~w(running running running)
    assert Enum.sort(Enum.map(snapshot.attempts, & &1.slot_index)) == [0, 1, 2]
    assert Enum.all?(snapshot.attempts, &(&1.base_sha == context.best_sha))

    assert Git.run!(context.workspace.repo, ["rev-parse", "refs/heads/pika/best"]) ==
             context.best_sha

    assert Enum.all?(starts, fn start ->
             String.contains?(start.instructions, "Attempt") and
               String.contains?(start.instructions, context.best_sha)
           end)

    assert_attempt_mcp_gateway(hd(starts).token)
    assert_mailbox_idempotency(coordinator, starts, context.campaign.id)
    Enum.each(starts, &send(&1.task_pid, :release))

    eventually(fn ->
      attempts = AttemptStore.attempts(context.campaign.id, limit: 10)
      length(attempts) == 3 and Enum.all?(attempts, &(&1.status == "ready_for_integration"))
    end)

    attempts = AttemptStore.attempts(context.campaign.id, limit: 10)

    Enum.each(attempts, fn attempt ->
      assert File.regular?(Path.join(context.workspace.root, "candidate-#{attempt.id}.txt")) ==
               false

      assert File.regular?(
               Path.join(
                 context.workspace.root,
                 attempt.worktree_relative_path <> "/candidate-#{attempt.id}.txt"
               )
             )

      assert attempt.patch_artifact_id
      assert attempt.metrics_artifact_id
      assert attempt.correctness_artifact_id
      assert attempt.summary =~ "two percent improvement"

      assert [%{pair_count: 7, valid_pair_count: 7, source: "iteration"}] =
               AttemptStore.metrics_for_attempt(attempt.id)

      assert [_ | _] =
               Path.wildcard(
                 Path.join(context.workspace.root, "artifacts/logs/#{attempt.id}/*.jsonl")
               )
    end)

    assert Git.run!(context.workspace.repo, ["rev-parse", "refs/heads/pika/best"]) ==
             context.best_sha

    assert Pika.Persistence.current_campaign().best_sha == context.best_sha
  end

  test "formal measurement follows the Campaign-specific pair protocol end to end" do
    context =
      OptimizationFixtures.setup_campaign(
        max_attempts: 1,
        pair_count: 8,
        min_valid_pairs: 6
      )

    coordinator =
      start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}, "xhigh"))

    [start] = receive_starts(1)
    assert start.instructions =~ "exactly 8 alternating"
    assert start.instructions =~ ~r/at least\s+6 valid Pairs/
    send(start.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(start.attempt_id)
      )
    end)

    assert [%{pair_count: 8, valid_pair_count: 8, source: "iteration"}] =
             AttemptStore.metrics_for_attempt(start.attempt_id)

    assert AttemptCoordinator.snapshot(coordinator).last_error == nil
  end

  test "does not dispatch while completed candidates await Integration by default" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 3)

    assert {:ok, first} = AttemptStore.create_attempt(context.campaign.id, 0)

    Repo.query!(
      "UPDATE attempts SET status = 'ready_for_integration' WHERE id = ?",
      [first.id]
    )

    coordinator =
      start_coordinator(
        context,
        profiles(1, %{test_pid: self(), barrier: true}),
        auto_dispatch: false,
        max_unverified_attempts: 0
      )

    assert :ok = AttemptCoordinator.dispatch(coordinator)
    refute_receive {:attempt_started, _, _, _, _}, 200
    assert [attempt] = AttemptStore.attempts(context.campaign.id, limit: 10)
    assert attempt.id == first.id
    assert attempt.status == "ready_for_integration"
  end

  test "MCP authentication is available while the Backend Session is opening" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    coordinator =
      start_coordinator(
        context,
        profiles(1, %{test_pid: self(), barrier: true, authorize_during_open: true})
      )

    [start] = receive_starts(1)
    assert :ok = AttemptCoordinator.authorize(start.token, coordinator)
    assert_attempt_mcp_gateway(start.token)
    send(start.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(start.attempt_id)
      )
    end)
  end

  test "active Attempts emit bounded work snapshots while waiting for a summary" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    log =
      ExUnit.CaptureLog.capture_log([level: :info], fn ->
        coordinator =
          start_coordinator(
            context,
            profiles(1, %{
              test_pid: self(),
              barrier: true,
              progress_event_output: "PIKA_MCP_TOKEN=periodic-log-secret"
            }),
            progress_log_interval_ms: 0,
            progress_log_level: :warning
          )

        [start] = receive_starts(1)

        eventually(fn ->
          coordinator
          |> :sys.get_state()
          |> Map.fetch!(:sessions)
          |> Map.values()
          |> Enum.any?(&(&1.last_event_summary == "PIKA_MCP_TOKEN=[REDACTED]"))
        end)

        send(coordinator, :log_work_snapshot)
        Process.sleep(50)
        send(start.task_pid, :release)

        eventually(fn ->
          match?(
            {:ok, %{status: "ready_for_integration"}},
            AttemptStore.attempt(start.attempt_id)
          )
        end)
      end)

    assert log =~ "Pika attempt work snapshot"
    assert log =~ "awaiting_agent_summary"
    assert log =~ "required_operations"
    assert log =~ "PIKA_MCP_TOKEN=[REDACTED]"
    refute log =~ "periodic-log-secret"
  end

  test "periodic Git inspection does not block Coordinator calls" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    owner = self()

    probe = fn _workspace, attempt ->
      send(owner, {:progress_probe_started, self()})

      receive do
        :release_probe -> %{attempt_id: attempt.id, git: %{dirty: false}}
      end
    end

    coordinator =
      start_coordinator(
        context,
        profiles(1, %{test_pid: self(), barrier: true}),
        progress_log_interval_ms: 0,
        progress_log_level: :debug,
        progress_probe: probe
      )

    [start] = receive_starts(1)
    send(coordinator, :log_work_snapshot)
    assert_receive {:progress_probe_started, probe_pid}, 1_000
    send(coordinator, :log_work_snapshot)
    refute_receive {:progress_probe_started, _other_pid}, 50

    assert %{campaign: %{campaign_id: campaign_id}} = AttemptCoordinator.snapshot(coordinator)
    assert campaign_id == context.campaign.id

    send(probe_pid, :release_probe)
    send(start.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(start.attempt_id)
      )
    end)
  end

  test "UI Spec overview reuses the snapshot and dispatch checks stay lightweight" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    {{:ok, overview}, overview_queries} =
      capture_repo_queries(fn -> AttemptStore.campaign_spec_overview(context.campaign.id) end)

    assert overview.cases == context.spec["benchmark_cases"]
    assert overview.metrics == context.spec["metrics"]

    refute Enum.any?(overview_queries, &direct_definition_scan?(&1, "benchmark_cases"))
    refute Enum.any?(overview_queries, &direct_definition_scan?(&1, "metric_definitions"))

    {{:ok, %{status: "optimizing", dispatch_gate: nil}}, dispatch_queries} =
      capture_repo_queries(fn -> AttemptStore.dispatch_state(context.campaign.id) end)

    assert length(dispatch_queries) == 1
    refute Enum.any?(dispatch_queries, &String.contains?(&1, "benchmark_cases"))
  end

  test "multiple completed Backend turns with missing MCP work follow up in the same Session" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    coordinator =
      start_coordinator(
        context,
        profiles(1, %{test_pid: self(), mode: :multi_followup, barrier: true})
      )

    [first] = receive_starts(1)
    send(first.task_pid, :release)
    [second] = receive_starts(1)
    assert first.attempt_id == second.attempt_id
    assert first.token == second.token
    assert {:ok, %{status: "awaiting_report"}} = AttemptStore.attempt(first.attempt_id)

    send(second.task_pid, :release)
    [third] = receive_starts(1)
    assert third.attempt_id == first.attempt_id
    assert third.token == first.token
    assert {:ok, %{status: "awaiting_report"}} = AttemptStore.attempt(first.attempt_id)

    send(third.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(first.attempt_id)
      )
    end)

    [[attempts_created]] =
      Repo.query!("SELECT attempts_created FROM campaigns WHERE id = ?", [context.campaign.id]).rows

    [[session_count, max_turns]] =
      Repo.query!(
        "SELECT COUNT(*), MAX(last_turn_sequence) FROM agent_sessions WHERE attempt_id = ?",
        [first.attempt_id]
      ).rows

    assert attempts_created == 1
    assert session_count == 1
    assert max_turns >= 3
    assert AttemptCoordinator.snapshot(coordinator).last_error == nil
  end

  test "repeated Backend crashes open new Sessions for the same budgeted Attempt" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    {:ok, crash_counter} = Agent.start_link(fn -> 0 end)

    coordinator =
      start_coordinator(
        context,
        profiles(1, %{test_pid: self(), mode: :crash_twice, crash_counter: crash_counter})
      )

    starts = receive_starts(3)
    assert starts |> Enum.map(& &1.attempt_id) |> Enum.uniq() == [hd(starts).attempt_id]
    assert starts |> Enum.map(& &1.token) |> Enum.uniq() |> length() == 3
    assert Enum.all?(tl(starts), &String.contains?(&1.instructions, "Recovery JSONL tail"))
    assert Enum.all?(tl(starts), &String.contains?(&1.instructions, "record_metrics"))

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(hd(starts).attempt_id)
      )
    end)

    [[attempts_created]] =
      Repo.query!("SELECT attempts_created FROM campaigns WHERE id = ?", [context.campaign.id]).rows

    [[session_count]] =
      Repo.query!("SELECT COUNT(*) FROM agent_sessions WHERE attempt_id = ?", [
        hd(starts).attempt_id
      ]).rows

    assert attempts_created == 1
    assert session_count == 3
    assert AttemptCoordinator.snapshot(coordinator).recovery_count[hd(starts).attempt_id] == 2
  end

  test "recovery context excludes provider wire records and keeps concise Agent events" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    {:ok, crash_counter} = Agent.start_link(fn -> 0 end)

    _coordinator =
      start_coordinator(
        context,
        profiles(1, %{
          test_pid: self(),
          mode: :crash_twice,
          crash_counter: crash_counter,
          barrier: true
        })
      )

    [first] = receive_starts(1)
    log_dir = Path.join([context.workspace.root, "artifacts", "logs", first.attempt_id])
    legacy_log = Path.join(log_dir, "legacy-mixed.jsonl")
    File.mkdir_p!(log_dir)

    raw_marker = "raw recursive prompt must not be recovered"
    kept_marker = "concise Agent progress survives recovery"

    File.write!(
      legacy_log,
      Jason.encode!(%{
        "at" => DateTime.utc_now() |> DateTime.to_iso8601(),
        "direction" => "out",
        "payload" => %{
          "method" => "thread/start",
          "params" => %{
            "developerInstructions" => raw_marker <> String.duplicate("x", 300_000)
          }
        }
      }) <>
        "\n" <>
        Jason.encode!(%{
          "at" => DateTime.utc_now() |> DateTime.to_iso8601(),
          "type" => "message_delta",
          "data" => %{"delta" => kept_marker}
        }) <>
        "\n"
    )

    send(first.task_pid, :release)
    [second] = receive_starts(1)
    assert second.instructions =~ kept_marker
    refute second.instructions =~ raw_marker
    assert byte_size(second.instructions) < 200_000

    send(second.task_pid, :release)
    [third] = receive_starts(1)
    refute third.instructions =~ raw_marker
    send(third.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(third.attempt_id)
      )
    end)
  end

  test "Coordinator restart recovers a plan-disabled Attempt as Iteration work" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    first_coordinator =
      start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))

    [first] = receive_starts(1)

    GenServer.stop(first_coordinator)

    second_coordinator =
      start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))

    [recovered] = receive_starts(1)
    assert recovered.attempt_id == first.attempt_id
    refute recovered.token == first.token

    assert {:ok, recovered_context} =
             AttemptCoordinator.mcp_call(
               recovered.token,
               "get_context",
               %{},
               second_coordinator
             )

    assert recovered_context.identity.role == :iteration
    assert recovered_context.attempt.status == "running"

    [[attempts_created]] =
      Repo.query!("SELECT attempts_created FROM campaigns WHERE id = ?", [context.campaign.id]).rows

    assert attempts_created == 1
    send(recovered.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(first.attempt_id)
      )
    end)
  end

  test "dispatch gate prevents new Attempts until it is cleared" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    Repo.query!("UPDATE campaigns SET dispatch_gate = 'paused' WHERE id = ?", [
      context.campaign.id
    ])

    coordinator = start_coordinator(context, profiles(1, %{test_pid: self()}))
    assert AttemptCoordinator.snapshot(coordinator).attempts == []

    Repo.query!("UPDATE campaigns SET dispatch_gate = NULL WHERE id = ?", [context.campaign.id])
    assert :ok = AttemptCoordinator.dispatch(coordinator)
    [start] = receive_starts(1)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(start.attempt_id)
      )
    end)
  end

  test "Attempt and Campaign Guidance keep their scopes" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 2)
    coordinator = start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))
    [first] = receive_starts(1)

    assert {:ok, current} =
             AttemptCoordinator.create_btw(
               first.attempt_id,
               "inspect register pressure",
               "current",
               "current-1",
               coordinator
             )

    assert current.kind == "attempt"

    assert {:ok, ^current} =
             AttemptCoordinator.create_btw(
               first.attempt_id,
               "inspect register pressure",
               "current",
               "current-1",
               coordinator
             )

    assert {:ok, future} =
             AttemptCoordinator.create_btw(
               first.attempt_id,
               "prefer a tiled alternative",
               "future",
               "future-1",
               coordinator
             )

    assert future.kind == "campaign"

    assert {:ok, side} =
             AttemptCoordinator.create_btw(
               first.attempt_id,
               "explain the last benchmark",
               "chat",
               "chat-1",
               coordinator
             )

    assert side.kind == "side"

    assert {:ok, first_context} =
             AttemptCoordinator.mcp_call(first.token, "get_context", %{}, coordinator)

    assert Enum.map(first_context.guidance, & &1.body) == ["inspect register pressure"]
    send(first.task_pid, :release)

    [second] = receive_starts(1)
    assert second.attempt_id != first.attempt_id

    assert {:ok, second_context} =
             AttemptCoordinator.mcp_call(second.token, "get_context", %{}, coordinator)

    assert Enum.map(second_context.guidance, & &1.body) == ["prefer a tiled alternative"]
    send(second.task_pid, :release)

    eventually(fn ->
      AttemptStore.attempts(context.campaign.id, limit: 10)
      |> Enum.all?(&(&1.status == "ready_for_integration"))
    end)
  end

  test "optional Plan Session writes one plan before Iteration without spending another Attempt" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1, plan_enabled: true)
    coordinator = start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))

    [plan_start] = receive_starts(1)
    send(plan_start.task_pid, :release)
    [iteration_start] = receive_starts(1)
    assert iteration_start.attempt_id == plan_start.attempt_id

    resources =
      mcp_rpc(iteration_start.token, 30, "resources/list", %{})
      |> get_in(["result", "resources"])

    assert [%{"uri" => uri, "mimeType" => "text/markdown"}] = resources

    assert [%{"text" => plan_body}] =
             mcp_rpc(iteration_start.token, 31, "resources/read", %{"uri" => uri})
             |> get_in(["result", "contents"])

    assert plan_body =~ "Improve the candidate"
    send(iteration_start.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration", plan_artifact_id: id}} when not is_nil(id),
        AttemptStore.attempt(plan_start.attempt_id)
      )
    end)

    assert File.read!(
             Path.join(
               context.workspace.root,
               "artifacts/plans/#{plan_start.attempt_id}/plan.md"
             )
           ) =~ "Improve the candidate"

    [[attempts_created, sessions]] =
      Repo.query!(
        "SELECT c.attempts_created, COUNT(s.id) FROM campaigns c JOIN agent_sessions s ON s.campaign_id = c.id WHERE c.id = ? GROUP BY c.id",
        [context.campaign.id]
      ).rows

    assert attempts_created == 1
    assert sessions == 2
    assert AttemptCoordinator.snapshot(coordinator).last_error == nil
  end

  test "Prompt history is limited to ten terminal Attempts and history queries exclude active work" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 13)

    for index <- 1..12 do
      {:ok, attempt} = AttemptStore.create_attempt(context.campaign.id, 0)

      assert {:ok, _event} =
               AttemptStore.submit_summary(attempt.id, %{
                 description: "history-description-#{index}",
                 summary: "history-token-#{String.pad_leading(to_string(index), 2, "0")}",
                 modification_scope: [],
                 risks: [],
                 profiler_summary: nil,
                 recommended_outcome: "skip"
               })

      assert {:ok, _attempt} = AttemptStore.mark_cancelled(attempt.id, "history fixture")
    end

    coordinator = start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))
    [start] = receive_starts(1)

    assert start.instructions =~ "history-token-03"
    assert start.instructions =~ "history-token-12"
    refute start.instructions =~ "history-token-01"
    refute start.instructions =~ "history-token-02"

    assert {:ok, history} =
             AttemptCoordinator.mcp_call(
               start.token,
               "query_attempt_history",
               %{"limit" => 20, "outcome" => "cancelled"},
               coordinator
             )

    assert length(history) == 12
    assert Enum.all?(history, &(&1.status == "cancelled"))
    refute Enum.any?(history, &(&1.id == start.attempt_id))
    send(start.task_pid, :release)
  end

  test "shared Reference symlinks and Skill roots never enter the candidate Patch" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    source = Pika.Test.AlignmentFixtures.git_repo()
    sha = Git.run!(source, ["rev-parse", "HEAD"])

    reference = [
      %{
        id: "local-ref",
        url: source,
        description: "local reference",
        selected: true,
        branch: Git.run!(source, ["branch", "--show-current"]),
        sha: sha
      }
    ]

    Repo.query!("UPDATE spec_revisions SET reference_snapshot_json = ? WHERE id = ?", [
      Jason.encode!(reference),
      context.spec_id
    ])

    coordinator = start_coordinator(context, profiles(1, %{test_pid: self(), barrier: true}))
    [start] = receive_starts(1)
    {:ok, running} = AttemptStore.attempt(start.attempt_id)
    worktree = Path.join(context.workspace.root, running.worktree_relative_path)
    checkout = Path.join(context.workspace.root, "refs/local-ref")
    link = Path.join(worktree, "ref/local-ref")
    assert Git.run!(checkout, ["rev-parse", "HEAD"]) == sha
    assert File.lstat!(link).type == :symlink
    assert File.read_link!(link) == checkout
    assert Git.run!(link, ["rev-parse", "HEAD"]) == sha
    assert Git.clean?(worktree)
    refute File.exists?(Path.join(worktree, ".gitmodules"))
    send(start.task_pid, :release)

    eventually(fn ->
      match?(
        {:ok, %{status: "ready_for_integration"}},
        AttemptStore.attempt(start.attempt_id)
      )
    end)

    {:ok, completed} = AttemptStore.attempt(start.attempt_id)
    tree = Git.run!(worktree, ["ls-tree", "-r", "--name-only", completed.candidate_sha])
    refute tree =~ "ref/local-ref"

    patch =
      File.read!(
        Path.join(context.workspace.root, "artifacts/patches/#{start.attempt_id}/candidate.patch")
      )

    refute patch =~ ".gitmodules"
    refute patch =~ "ref/local-ref"
    refute patch =~ ".pika/skills"
    assert File.dir?(checkout)
    assert AttemptCoordinator.snapshot(coordinator).last_error == nil
  end

  defp start_coordinator(context, profiles, opts \\ []) do
    coordinator_opts =
      [
        workspace: context.workspace,
        campaign: context.campaign,
        profiles: profiles,
        backend_modules: %{codex_app_server: AttemptAgentBackend},
        mcp_url: "http://127.0.0.1:18080/mcp"
      ]
      |> Keyword.merge(opts)
      |> Keyword.put_new(:max_unverified_attempts, 100)

    {:ok, coordinator} =
      AttemptCoordinator.start_link(coordinator_opts)

    Process.unlink(coordinator)

    on_exit(fn ->
      if Process.alive?(coordinator) do
        try do
          GenServer.stop(coordinator)
        catch
          :exit, _reason -> :ok
        end
      end
    end)

    coordinator
  end

  defp profiles(count, env, reasoning_effort \\ "high") do
    for index <- 1..count do
      %{
        "name" => "slot-#{index}",
        "backend" => "codex_app_server",
        "command" => ["codex", "app-server", "--listen", "stdio://"],
        "reasoning_effort" => reasoning_effort,
        "env" => env,
        "protocol_config" => %{}
      }
    end
  end

  defp receive_starts(count) do
    for _ <- 1..count do
      assert_receive {:attempt_started, attempt_id, task_pid, token, instructions}, 5_000

      %{
        attempt_id: attempt_id,
        task_pid: task_pid,
        token: token,
        instructions: instructions
      }
    end
  end

  defp assert_mailbox_idempotency(coordinator, starts, campaign_id) do
    [sender, receiver | _] = starts
    {:ok, agents} = AttemptCoordinator.mcp_call(sender.token, "list_agents", %{}, coordinator)
    target = Enum.find(agents, &(&1.attempt_id == receiver.attempt_id))

    args = %{
      "idempotency_key" => "message-once",
      "target_session_id" => target.id,
      "body" => "share occupancy findings",
      "priority" => "high"
    }

    assert {:ok, sent} =
             AttemptCoordinator.mcp_call(sender.token, "send_agent_message", args, coordinator)

    assert {:ok, ^sent} =
             AttemptCoordinator.mcp_call(sender.token, "send_agent_message", args, coordinator)

    assert {:ok, [message]} =
             AttemptCoordinator.mcp_call(
               receiver.token,
               "read_agent_messages",
               %{"after_sequence" => 0},
               coordinator
             )

    assert message.body == "share occupancy findings"

    assert {:ok, [redelivered]} =
             AttemptCoordinator.mcp_call(
               receiver.token,
               "read_agent_messages",
               %{"after_sequence" => 0},
               coordinator
             )

    assert redelivered.id == message.id

    assert {:ok, %{count: 1}} =
             AttemptCoordinator.mcp_call(
               receiver.token,
               "ack_agent_messages",
               %{
                 "idempotency_key" => "ack-once",
                 "through_sequence" => message.sequence
               },
               coordinator
             )

    assert {:ok, []} =
             AttemptCoordinator.mcp_call(
               receiver.token,
               "read_agent_messages",
               %{"after_sequence" => 0},
               coordinator
             )

    [[count]] =
      Repo.query!("SELECT COUNT(*) FROM agent_messages WHERE campaign_id = ?", [campaign_id]).rows

    assert count == 1
  end

  defp assert_attempt_mcp_gateway(token) do
    initialize =
      Plug.Test.conn(
        :post,
        "/mcp",
        Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "initialize"})
      )
      |> Plug.Conn.put_req_header("content-type", "application/json")
      |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
      |> PikaWeb.MCPGateway.call([])

    assert %{"result" => %{"serverInfo" => %{"name" => "pika-optimization"}}} =
             Jason.decode!(initialize.resp_body)

    tools =
      Plug.Test.conn(
        :post,
        "/mcp",
        Jason.encode!(%{"jsonrpc" => "2.0", "id" => 2, "method" => "tools/list"})
      )
      |> Plug.Conn.put_req_header("content-type", "application/json")
      |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
      |> PikaWeb.MCPGateway.call([])
      |> Map.fetch!(:resp_body)
      |> Jason.decode!()

    names = Enum.map(tools["result"]["tools"], & &1["name"])
    assert "record_metrics" in names
    refute "submit_plan" in names

    context =
      Plug.Test.conn(
        :post,
        "/mcp",
        Jason.encode!(%{
          "jsonrpc" => "2.0",
          "id" => 3,
          "method" => "tools/call",
          "params" => %{"name" => "get_context", "arguments" => %{}}
        })
      )
      |> Plug.Conn.put_req_header("content-type", "application/json")
      |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
      |> PikaWeb.MCPGateway.call([])

    assert context.status == 200

    assert %{
             "result" => %{
               "structuredContent" => %{
                 "campaign" => %{"best_metrics" => best_metrics},
                 "identity" => %{"role" => "iteration"}
               }
             }
           } = Jason.decode!(context.resp_body)

    assert map_size(best_metrics) == 1
  end

  defp capture_repo_queries(fun) do
    ref = make_ref()
    handler = {__MODULE__, ref}
    owner = self()

    :ok =
      :telemetry.attach(
        handler,
        [:pika, :repo, :query],
        fn _event, _measurements, metadata, {pid, tag} ->
          send(pid, {tag, to_string(metadata.query)})
        end,
        {owner, ref}
      )

    try do
      result = fun.()
      {result, receive_queries(ref, [])}
    after
      :telemetry.detach(handler)
    end
  end

  defp receive_queries(ref, queries) do
    receive do
      {^ref, query} -> receive_queries(ref, [query | queries])
    after
      0 -> Enum.reverse(queries)
    end
  end

  defp direct_definition_scan?(query, table) do
    normalized = String.replace(query, ~r/\s+/, " ")
    String.contains?(normalized, "FROM #{table} WHERE")
  end

  defp mcp_rpc(token, id, method, params) do
    Plug.Test.conn(
      :post,
      "/mcp",
      Jason.encode!(%{"jsonrpc" => "2.0", "id" => id, "method" => method, "params" => params})
    )
    |> Plug.Conn.put_req_header("content-type", "application/json")
    |> Plug.Conn.put_req_header("authorization", "Bearer #{token}")
    |> PikaWeb.MCPGateway.call([])
    |> Map.fetch!(:resp_body)
    |> Jason.decode!()
  end

  defp eventually(fun, attempts \\ 100)

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
