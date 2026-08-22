defmodule Pika.Test.IntegrationAgentBackend do
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Event, Session}
  alias Pika.{Git, IntegrationCoordinator}
  alias Pika.Test.OptimizationFixtures

  def start_link(profile, sink) do
    Agent.start_link(fn ->
      %{
        sink: sink,
        profile: profile,
        session: nil,
        cwd: nil,
        mcp: nil,
        turn: nil,
        task_pid: nil
      }
    end)
  end

  def open_session(server, cwd, model, effort, mcp, _skill_roots, instructions) do
    if Agent.get(server, &env(&1)[:authorize_during_open]) do
      assert_mcp_handshake(mcp)
    end

    session = %Session{
      id: Ecto.UUID.generate(),
      backend: :fake,
      backend_protocol: "integration-fake-v1",
      backend_session_id: Ecto.UUID.generate(),
      cwd: cwd,
      model: model,
      reasoning_effort: effort,
      jsonl_path: "/dev/null"
    }

    Agent.update(
      server,
      &Map.merge(&1, %{session: session, cwd: cwd, mcp: mcp, instructions: instructions})
    )

    emit(server, :session_started)
    {:ok, session}
  end

  def start_turn(server, _input) do
    turn_id = Ecto.UUID.generate()

    state =
      Agent.get_and_update(server, fn state ->
        {%{state | turn: turn_id}, %{state | turn: turn_id}}
      end)

    emit(server, :turn_started)
    {:ok, task_pid} = Task.start(fn -> run(server, state) end)
    store_task(server, task_pid)
    {:ok, turn_id}
  end

  def steer(server, _input), do: {:ok, Agent.get(server, & &1.turn)}

  def interrupt(server) do
    if Process.alive?(server) do
      state = Agent.get(server, & &1)

      if pid = env(state)[:test_pid],
        do: send(pid, {:integration_interrupted, state.mcp && state.mcp.attempt_id})

      stop_task(state.task_pid)
    end

    :ok
  end

  def capabilities(_server), do: %{protocol: "integration-fake-v1", native_steer: true}

  def close_session(server) do
    if Process.alive?(server) do
      stop_task(Agent.get(server, & &1.task_pid))
      Agent.stop(server)
    end

    :ok
  end

  defp run(server, state) do
    notify(state)

    if env(state)[:unresponsive] do
      complete_turn(server)
    else
      run_integration(server, state)
    end
  end

  defp run_integration(server, state) do
    maybe_wait(state)
    {:ok, context} = mcp(state, "get_integration_context", %{})

    {:ok, lease} =
      mcp(state, "acquire_integration_lease", %{
        "idempotency_key" => "lease-#{state.mcp.attempt_id}",
        "expected_best_sha" => context.best_sha
      })

    maybe_wait_after_lease(state)

    if lease.stale_base, do: refresh(state, lease)

    maybe_crash(server, state, :after_lease)
    {:ok, context} = mcp(state, "get_integration_context", %{})
    receipt = context.receipt || full_regression(server, state, context, lease)
    if is_nil(context.receipt), do: maybe_crash(server, state, :after_receipt)

    if receipt.status == "rejected" and receipt.id == state.mcp.attempt_id do
      :ok
    else
      finish_integration(server, state, context, lease, receipt)
    end

    complete_turn(server)
  end

  defp finish_integration(server, state, context, lease, receipt) do
    if receipt.status == "rejected" do
      {:ok, _} =
        mcp(state, "reject_attempt", %{
          "idempotency_key" => "reject-#{state.mcp.attempt_id}",
          "lease_id" => lease.id,
          "receipt_id" => receipt.id,
          "representative_case_ids" => receipt.regressed_case_ids,
          "representative_case_reasons" =>
            Map.new(receipt.regressed_case_ids, fn case_id ->
              {case_id,
               "Representative by shape family, confirmed regression magnitude, and production weight"}
            end)
        })
    else
      intent =
        context.intent ||
          then_create_intent(state, lease.id, receipt.id)

      if is_nil(context.intent), do: maybe_crash(server, state, :after_intent)

      current_best_head = Git.run!(context.best_worktree, ["rev-parse", "HEAD"])

      new_sha =
        if current_best_head == context.best_sha,
          do: squash_merge(context, state.mcp.attempt_id),
          else: current_best_head

      if current_best_head == context.best_sha, do: maybe_crash(server, state, :after_squash)

      maybe_crash(server, state, :before_sqlite_commit)

      {:ok, _} =
        mcp(state, "complete_merge", %{
          "idempotency_key" => "complete-merge-#{state.mcp.attempt_id}",
          "lease_id" => lease.id,
          "receipt_id" => receipt.id,
          "intent_id" => intent.id,
          "new_sha" => new_sha
        })

      maybe_crash(server, state, :after_sqlite_commit)
    end
  end

  defp then_create_intent(state, lease_id, receipt_id) do
    {:ok, intent} =
      mcp(state, "create_merge_intent", %{
        "idempotency_key" => "merge-intent-#{state.mcp.attempt_id}",
        "lease_id" => lease_id,
        "receipt_id" => receipt_id
      })

    intent
  end

  defp refresh(state, lease) do
    {:ok, context} = mcp(state, "get_integration_context", %{})
    Git.run!(state.cwd, ["rebase", context.best_sha])
    candidate_sha = Git.run!(state.cwd, ["rev-parse", "HEAD"])
    root = workspace_root(state.cwd)

    {samples, correctness} =
      OptimizationFixtures.write_iteration_artifacts(
        root,
        state.mcp.attempt_id,
        context.best_sha,
        candidate_sha
      )

    register(state, samples.relative_path, "iteration_measurement")
    register(state, correctness.relative_path, "iteration_measurement")

    patch =
      Git.run!(state.cwd, [
        "diff",
        "--binary",
        "--full-index",
        "#{context.best_sha}...#{candidate_sha}",
        "--",
        "."
      ])

    patch_path = Path.join(root, "artifacts/patches/#{state.mcp.attempt_id}/candidate.patch")
    File.write!(patch_path, patch <> if(patch == "", do: "", else: "\n"))
    register(state, "artifacts/patches/#{state.mcp.attempt_id}/candidate.patch", "patch")

    {:ok, _} =
      mcp(state, "complete_refresh", %{
        "idempotency_key" => "refresh-#{state.mcp.attempt_id}",
        "lease_id" => lease.id,
        "sampling_revision_id" => context.attempt.sampling_revision_id,
        "new_base_sha" => context.best_sha,
        "candidate_sha" => candidate_sha,
        "harness_digest" => context.spec_revision.protected_digest,
        "samples_artifact" => samples.relative_path,
        "correctness_artifact" => correctness.relative_path
      })
  end

  defp full_regression(server, state, context, lease) do
    attempt_id = state.mcp.attempt_id
    root = workspace_root(state.cwd)
    regression? = context.attempt.ordinal in List.wrap(env(state)[:regress_ordinals])
    screening_relative = "artifacts/logs/#{attempt_id}/integration/screening.jsonl"
    full_relative = "artifacts/logs/#{attempt_id}/integration/full.jsonl"
    correctness_relative = "artifacts/logs/#{attempt_id}/integration/correctness.json"
    screening_path = Path.join(root, screening_relative)
    full_path = Path.join(root, full_relative)
    correctness_path = Path.join(root, correctness_relative)
    File.mkdir_p!(Path.dirname(screening_path))

    screening_keys =
      for benchmark_case <- context.cases,
          metric <- context.metrics,
          do: {benchmark_case["id"], metric["id"]}

    unless reusable_jsonl?(screening_path, context, screening_keys, 5) do
      screening =
        for benchmark_case <- context.cases,
            metric <- context.metrics,
            index <- 0..4 do
          improvement =
            if regression? and benchmark_case["id"] == "guard_case",
              do: -0.02,
              else: improvement_for(context.attempt.ordinal, state)

          pair(context, benchmark_case["id"], metric["id"], index, improvement)
        end

      track_measurement(state, :screening)
      File.write!(screening_path, encode_jsonl(screening))
    end

    register(state, screening_relative, "full_regression_screening")
    maybe_crash(server, state, :during_screening)

    pair_count = context.spec["benchmark"]["pair_count"]
    full_keys = screening_keys

    if not reusable_jsonl?(full_path, context, full_keys, pair_count) do
      full =
        for benchmark_case <- context.cases,
            metric <- context.metrics,
            index <- 0..(pair_count - 1) do
          improvement =
            if regression? and benchmark_case["id"] == "guard_case",
              do: -0.02,
              else: improvement_for(context.attempt.ordinal, state)

          pair(context, benchmark_case["id"], metric["id"], index, improvement)
        end

      track_measurement(state, :escalation)
      File.write!(full_path, encode_jsonl(full))
    end

    register(state, full_relative, "full_regression_escalation")
    maybe_crash(server, state, :during_escalation)

    File.write!(
      correctness_path,
      Jason.encode!(%{
        "schema_version" => 2,
        "target_snapshot_id" => context.target_snapshot.id,
        "candidate_sha" => context.attempt.candidate_sha,
        "cases" =>
          Enum.map(context.cases, fn benchmark_case ->
            %{
              "case_id" => benchmark_case["id"],
              "target_passed" => true,
              "candidate_passed" => env(state)[:correctness_failure] != true
            }
          end)
      })
    )

    register(state, correctness_relative, "full_regression_correctness")

    {:ok, receipt} =
      mcp(state, "submit_full_regression", %{
        "idempotency_key" => "full-regression-#{attempt_id}",
        "lease_id" => lease.id,
        "base_sha" => context.best_sha,
        "candidate_sha" => context.attempt.candidate_sha,
        "harness_digest" => context.spec_revision.protected_digest,
        "screening_artifact" => screening_relative,
        "correctness_artifact" => correctness_relative,
        "full_artifact" => full_relative
      })

    receipt
  end

  defp squash_merge(context, attempt_id) do
    Git.run!(context.best_worktree, ["apply", "--index", context.patch_path])

    message =
      "Accept Pika Attempt #{attempt_id}\n\n" <>
        "Pika-Attempt: #{attempt_id}\n" <>
        "Pika-Spec-Revision: #{context.attempt.spec_revision_id}\n" <>
        "Pika-Sampling-Revision: #{context.attempt.sampling_revision_id}"

    Git.run!(context.best_worktree, ["commit", "-m", message])
    Git.run!(context.best_worktree, ["rev-parse", "HEAD"])
  end

  defp improvement_for(ordinal, state) do
    env(state)
    |> Map.get(:improvements_by_ordinal, %{})
    |> Map.get(ordinal, 0.02)
  end

  defp reusable_jsonl?(path, _context, expected_keys, pair_count) do
    with {:ok, body} <- File.read(path),
         records <-
           body
           |> String.split("\n", trim: true)
           |> Enum.map(&Jason.decode/1),
         true <- Enum.all?(records, &match?({:ok, _}, &1)),
         decoded <- Enum.map(records, &elem(&1, 1)),
         grouped <- Enum.group_by(decoded, &{&1["case_id"], &1["metric_id"]}),
         true <- Enum.sort(Map.keys(grouped)) == Enum.sort(expected_keys),
         true <-
           Enum.all?(grouped, fn {_key, values} ->
             ordered = Enum.sort_by(values, & &1["pair_index"])

             Enum.map(ordered, & &1["pair_index"]) == Enum.to_list(0..(pair_count - 1)) and
               Enum.with_index(ordered)
               |> Enum.all?(fn {record, index} ->
                 record["order"] == if(rem(index, 2) == 0, do: "tc", else: "ct")
               end)
           end) do
      true
    else
      _ -> false
    end
  end

  defp track_measurement(state, kind) do
    if counter = env(state)[:measurement_counter] do
      Agent.update(counter, &Map.update(&1, kind, 1, fn count -> count + 1 end))
    end
  end

  defp store_task(server, task_pid) do
    Agent.update(server, &%{&1 | task_pid: task_pid})
  catch
    :exit, _reason -> :ok
  end

  defp stop_task(pid) when is_pid(pid) do
    if Process.alive?(pid), do: Process.exit(pid, :kill)
  end

  defp stop_task(_pid), do: :ok

  defp register(state, relative_path, kind) do
    root = workspace_root(state.cwd)
    {:ok, artifact} = Pika.Alignment.ArtifactStore.register(root, relative_path)

    {:ok, _} =
      mcp(state, "register_artifact", %{
        "idempotency_key" => "integration-artifact-#{relative_path}",
        "kind" => kind,
        "relative_path" => relative_path,
        "sha256" => artifact.sha256,
        "size" => artifact.size,
        "mime" => artifact.mime,
        "metadata" => %{}
      })
  end

  defp pair(_context, case_id, metric_id, index, improvement) do
    target = 10.0 + index / 10_000

    %{
      "case_id" => case_id,
      "metric_id" => metric_id,
      "pair_index" => index,
      "order" => if(rem(index, 2) == 0, do: "tc", else: "ct"),
      "target" => target,
      "candidate" => target * (1.0 - improvement),
      "valid" => true,
      "error" => nil
    }
  end

  defp maybe_crash(server, state, stage) do
    if env(state)[:crash_stage] == stage do
      counter = env(state)[:crash_counter]

      if counter && Agent.get_and_update(counter, &{&1 + 1, &1 + 1}) == 1 do
        Process.exit(server, :injected_integration_crash)
        Process.exit(self(), :normal)
      end
    end
  end

  defp notify(state) do
    if pid = env(state)[:test_pid],
      do: send(pid, {:integration_started, state.mcp.attempt_id, self(), state.mcp.token})
  end

  defp maybe_wait(state) do
    if env(state)[:barrier] do
      receive do
        :release -> :ok
      after
        5_000 -> :ok
      end
    end
  end

  defp maybe_wait_after_lease(state) do
    if env(state)[:barrier_after_lease] do
      if pid = env(state)[:test_pid], do: send(pid, {:integration_lease_waiting, self()})

      receive do
        :release -> :ok
      after
        5_000 -> :ok
      end
    end
  end

  defp complete_turn(server) do
    if Process.alive?(server), do: emit(server, :turn_completed, %{status: "completed"})
  end

  defp mcp(state, tool, args),
    do: IntegrationCoordinator.mcp_call(state.mcp.token, tool, args, state.mcp.coordinator)

  defp assert_mcp_handshake(mcp) do
    Enum.each(["initialize", "tools/list"], fn method ->
      request = %{"jsonrpc" => "2.0", "id" => method, "method" => method}

      response =
        Plug.Test.conn(:post, "/mcp", Jason.encode!(request))
        |> Plug.Conn.put_req_header("content-type", "application/json")
        |> Plug.Conn.put_req_header("authorization", "Bearer #{mcp.token}")
        |> PikaWeb.MCPGateway.call([])

      decoded = Jason.decode!(response.resp_body)

      if response.status != 200 or Map.has_key?(decoded, "error") do
        raise "Integration MCP handshake failed during Session open: #{inspect(decoded)}"
      end
    end)
  end

  defp workspace_root(cwd), do: cwd |> Path.dirname() |> Path.dirname()
  defp encode_jsonl(records), do: Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n"
  defp env(state), do: state.profile.env || %{}

  defp emit(server, type, data \\ %{}) do
    state = Agent.get(server, & &1)

    event =
      Event.new(type, :fake, state.session.id, %{
        backend_session_id: state.session.backend_session_id,
        turn_id: state.turn,
        data: data
      })

    send(state.sink, {:pika_backend_event, event})
  catch
    :exit, _ -> :ok
  end
end
