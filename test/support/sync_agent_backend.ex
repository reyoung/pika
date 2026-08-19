defmodule Pika.Test.SyncAgentBackend do
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Event, Session}
  alias Pika.{Git, SyncCoordinator}

  def start_link(profile, sink) do
    Agent.start_link(fn ->
      %{
        profile: profile,
        sink: sink,
        session: nil,
        cwd: nil,
        mcp: nil,
        turn: nil,
        task_pid: nil
      }
    end)
  end

  def open_session(server, cwd, model, effort, mcp, _skill_roots, instructions) do
    session = %Session{
      id: Ecto.UUID.generate(),
      backend: :fake,
      backend_protocol: "sync-fake-v1",
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
        do: send(pid, {:sync_interrupted, state.mcp && state.mcp.sync_run_id})

      stop_task(state.task_pid)
    end

    :ok
  end

  def close_session(server) do
    if Process.alive?(server) do
      stop_task(Agent.get(server, & &1.task_pid))
      Agent.stop(server)
    end

    :ok
  end

  def capabilities(_server), do: %{protocol: "sync-fake-v1", native_steer: true}

  defp run(server, state) do
    if pid = env(state)[:test_pid],
      do: send(pid, {:sync_started, state.mcp.sync_run_id, state.mcp.token})

    if barrier = env(state)[:barrier] do
      send(barrier, {:sync_waiting, self()})
      receive do: (:release -> :ok)
    end

    context = get_context(state)
    run = context.sync_run

    context =
      if "report_sync_candidate" in context.required_operations do
        merge_remote(state, run)
        candidate_sha = Git.run!(state.cwd, ["rev-parse", "HEAD"])
        digest = harness_digest(state.cwd, context.campaign)

        {:ok, _} =
          mcp(state, "report_sync_candidate", %{
            "idempotency_key" => "candidate-#{run.id}",
            "candidate_sha" => candidate_sha,
            "harness_digest" => digest
          })

        get_context(state)
      else
        context
      end

    if context.sync_run.status == "awaiting_spec_confirmation" do
      complete_turn(server)
    else
      if "submit_sync_validation" in context.required_operations do
        validate(state, context)
      end

      context = get_context(state)

      if "create_sync_intent" in context.required_operations do
        {:ok, _} =
          mcp(state, "create_sync_intent", %{
            "idempotency_key" => "intent-#{context.sync_run.id}"
          })
      end

      context = get_context(state)

      if "complete_sync" in context.required_operations do
        _ =
          mcp(state, "complete_sync", %{
            "idempotency_key" => "complete-#{context.sync_run.id}"
          })
      end

      complete_turn(server)
    end
  end

  defp merge_remote(state, run) do
    remote_ref = "refs/pika/sync/#{run.id}/remote"

    case Git.run(state.cwd, ["merge", "--no-edit", remote_ref]) do
      {:ok, _} ->
        :ok

      {:error, _} ->
        files = Git.run!(state.cwd, ["diff", "--name-only", "--diff-filter=U"])

        files
        |> String.split("\n", trim: true)
        |> Enum.each(fn relative ->
          File.write!(Path.join(state.cwd, relative), "resolved by sync agent\n")
        end)

        Git.run!(state.cwd, ["add", "."])
        Git.run!(state.cwd, ["commit", "-m", "Resolve Sync conflicts"])
    end
  end

  defp validate(state, context) do
    run = context.sync_run
    root = workspace_root(state.cwd)
    samples_relative = "artifacts/logs/sync/#{run.id}/pairs.jsonl"
    correctness_relative = "artifacts/logs/sync/#{run.id}/correctness.json"
    samples_path = Path.join(root, samples_relative)
    correctness_path = Path.join(root, correctness_relative)
    File.mkdir_p!(Path.dirname(samples_path))

    records =
      for benchmark_case <- context.campaign.cases,
          metric <- context.campaign.metrics,
          index <- 0..29 do
        baseline = 10.0 + index / 10_000

        %{
          "schema_version" => 1,
          "base_sha" => run.base_sha,
          "candidate_sha" => run.candidate_sha,
          "case_id" => benchmark_case["id"],
          "metric_id" => metric["id"],
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "bc", else: "cb"),
          "baseline" => baseline,
          "candidate" => baseline * 0.99,
          "valid" => true
        }
      end

    File.write!(samples_path, Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n")

    File.write!(
      correctness_path,
      Jason.encode!(%{
        "candidate_sha" => run.candidate_sha,
        "cases" => Enum.map(context.campaign.cases, &%{"case_id" => &1["id"], "passed" => true})
      })
    )

    register(state, samples_relative, "sync_measurement")
    register(state, correctness_relative, "sync_correctness")

    {:ok, _} =
      mcp(state, "submit_sync_validation", %{
        "idempotency_key" => "validation-#{run.id}",
        "base_sha" => run.base_sha,
        "candidate_sha" => run.candidate_sha,
        "samples_artifact" => samples_relative,
        "correctness_artifact" => correctness_relative
      })
  end

  defp register(state, relative_path, kind) do
    root = workspace_root(state.cwd)
    {:ok, artifact} = Pika.Alignment.ArtifactStore.register(root, relative_path)

    {:ok, _} =
      mcp(state, "register_artifact", %{
        "idempotency_key" => "artifact-#{relative_path}",
        "kind" => kind,
        "relative_path" => relative_path,
        "sha256" => artifact.sha256,
        "size" => artifact.size,
        "mime" => artifact.mime,
        "metadata" => %{}
      })
  end

  defp harness_digest(root, campaign) do
    spec = campaign.spec
    reference = get_in(spec, ["computation", "reference_path"])
    benchmark = get_in(spec, ["benchmark", "harness_path"])
    correctness = campaign.protected_paths -- [reference, benchmark]

    {:ok, harness} =
      Pika.Harness.validate(root, %{
        "reference_path" => reference,
        "benchmark_path" => benchmark,
        "correctness_paths" => correctness,
        "protected_paths" => campaign.protected_paths
      })

    harness.digest
  end

  defp mcp(state, tool, args),
    do: SyncCoordinator.mcp_call(state.mcp.token, tool, args, state.mcp.coordinator)

  defp get_context(state) do
    case mcp(state, "get_sync_context", %{}) do
      {:ok, context} -> context
      {:error, "unauthorized", _message, _details} -> exit(:normal)
      other -> raise "unexpected Sync context response: #{inspect(other)}"
    end
  end

  defp emit(server, type) do
    state = Agent.get(server, & &1)

    send(
      state.sink,
      {:pika_backend_event,
       %Event{
         backend: :fake,
         session_id: state.session.id,
         backend_session_id: state.session.backend_session_id,
         turn_id: state.turn,
         type: type,
         data: %{},
         at: DateTime.utc_now()
       }}
    )
  end

  defp complete_turn(server) do
    if Process.alive?(server), do: emit(server, :turn_completed)
  end

  defp env(state), do: state.profile[:env] || state.profile["env"] || %{}

  defp store_task(server, task_pid) do
    Agent.update(server, &%{&1 | task_pid: task_pid})
  catch
    :exit, _reason -> :ok
  end

  defp stop_task(pid) when is_pid(pid) do
    if Process.alive?(pid), do: Process.exit(pid, :kill)
  end

  defp stop_task(_pid), do: :ok
  defp workspace_root(cwd), do: cwd |> Path.dirname() |> Path.dirname()
end
