defmodule Pika.Test.AttemptAgentBackend do
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Event, Session}
  alias Pika.AttemptCoordinator, as: Coordinator
  alias Pika.Git
  alias Pika.Test.OptimizationFixtures

  def start_link(profile, sink) do
    Agent.start_link(fn ->
      %{
        sink: sink,
        profile: profile,
        session: nil,
        cwd: nil,
        mcp: nil,
        instructions: nil,
        turn: nil,
        turn_count: 0,
        task_pid: nil
      }
    end)
  end

  def open_session(server, cwd, model, effort, mcp, _skill_roots, instructions) do
    session = %Session{
      id: Ecto.UUID.generate(),
      backend: :fake,
      backend_protocol: "attempt-fake-v1",
      backend_session_id: Ecto.UUID.generate(),
      cwd: cwd,
      model: model,
      reasoning_effort: effort,
      jsonl_path: "/dev/null"
    }

    Agent.update(
      server,
      &%{&1 | session: session, cwd: cwd, mcp: mcp, instructions: instructions}
    )

    emit(server, :session_started)
    {:ok, session}
  end

  def start_turn(server, input) do
    turn_id = Ecto.UUID.generate()

    state =
      Agent.get_and_update(server, fn state ->
        next = %{state | turn: turn_id, turn_count: state.turn_count + 1}
        {next, next}
      end)

    emit(server, :turn_started)

    {:ok, task_pid} =
      Task.start(fn ->
        run(server, state, input)
      end)

    store_task(server, task_pid)

    {:ok, turn_id}
  end

  def steer(server, input) do
    state = Agent.get(server, & &1)
    emit(server, :message_delta, %{delta: "guided: #{input}"})
    {:ok, state.turn}
  end

  def interrupt(server) do
    if Process.alive?(server) do
      state = Agent.get(server, & &1)

      if pid = env(state)[:test_pid],
        do: send(pid, {:attempt_interrupted, state.mcp && state.mcp.attempt_id})

      stop_task(state.task_pid)
      emit(server, :turn_completed, %{status: "interrupted"})
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

  def capabilities(_server), do: %{protocol: "attempt-fake-v1", native_steer: true}

  defp run(server, state, _input) do
    notify_start(state)
    maybe_wait(state)

    case mode(state) do
      :crash_twice ->
        crash_or_complete(server, state)

      :followup ->
        if state.turn_count == 1,
          do: iteration(server, state, complete?: false),
          else: complete_only(server, state)

      :multi_followup ->
        if state.turn_count <= 2,
          do: complete_turn(server),
          else: iteration(server, state, complete?: true)

      _ ->
        if state.mcp.role == :plan,
          do: plan(server, state),
          else: iteration(server, state, complete?: true)
    end
  end

  defp crash_or_complete(server, state) do
    counter = env(state)[:crash_counter]
    count = Agent.get_and_update(counter, &{&1 + 1, &1 + 1})

    if count <= 2 do
      Process.exit(server, :injected_crash)
    else
      iteration(server, state, complete?: true)
    end
  end

  defp plan(server, state) do
    {:ok, _} =
      mcp(state, "submit_plan", %{
        "idempotency_key" => "plan-#{state.mcp.attempt_id}",
        "markdown" => "# Plan\n\nImprove the candidate by two percent.",
        "summary" => "two percent fixture plan"
      })

    complete_turn(server)
  end

  defp iteration(server, state, opts) do
    attempt_id = state.mcp.attempt_id
    base_sha = Git.run!(state.cwd, ["rev-parse", "HEAD"])
    File.write!(Path.join(state.cwd, "candidate-#{attempt_id}.txt"), "optimized\n")
    Git.run!(state.cwd, ["add", "."])
    Git.run!(state.cwd, ["commit", "-m", "Optimize #{attempt_id}"])
    candidate_sha = Git.run!(state.cwd, ["rev-parse", "HEAD"])
    root = state.cwd |> Path.dirname() |> Path.dirname()
    {:ok, context} = mcp(state, "get_context", %{})
    benchmark = context.campaign.spec["benchmark"]

    {samples, correctness} =
      OptimizationFixtures.write_iteration_artifacts(root, attempt_id, base_sha, candidate_sha,
        pair_count: benchmark["pair_count"]
      )

    Enum.each([samples, correctness], &register(state, &1))

    metrics_args = %{
      "idempotency_key" => "metrics-#{attempt_id}",
      "sampling_revision_id" => sampling_revision(state),
      "base_sha" => base_sha,
      "candidate_sha" => candidate_sha,
      "harness_digest" => harness_digest(state),
      "samples_artifact" => samples.relative_path,
      "correctness_artifact" => correctness.relative_path
    }

    {:ok, metrics_response} = mcp(state, "record_metrics", metrics_args)
    {:ok, ^metrics_response} = mcp(state, "record_metrics", metrics_args)

    {:ok, _} =
      mcp(state, "submit_attempt_summary", %{
        "idempotency_key" => "summary-#{attempt_id}",
        "description" => "fixture candidate #{attempt_id}",
        "summary" => "independent candidate reached a two percent improvement",
        "modification_scope" => ["candidate-#{attempt_id}.txt"],
        "risks" => [],
        "profiler_summary" => "fixture",
        "recommended_outcome" => "integrate"
      })

    if Keyword.fetch!(opts, :complete?) do
      complete(state, base_sha, candidate_sha)
    end

    complete_turn(server)
  end

  defp complete_only(server, state) do
    {:ok, context} = mcp(state, "get_context", %{})
    complete(state, context.attempt.base_sha, context.attempt.candidate_sha)
    complete_turn(server)
  end

  defp complete(state, base_sha, candidate_sha) do
    {:ok, _} =
      mcp(state, "complete_attempt", %{
        "idempotency_key" => "complete-#{state.mcp.attempt_id}",
        "sampling_revision_id" => sampling_revision(state),
        "base_sha" => base_sha,
        "candidate_sha" => candidate_sha,
        "worktree_status" => "clean",
        "latest_commit" => candidate_sha
      })
  end

  defp register(state, artifact) do
    args = %{
      "idempotency_key" => "artifact-#{artifact.relative_path}",
      "kind" => "iteration_measurement",
      "relative_path" => artifact.relative_path,
      "sha256" => artifact.sha256,
      "size" => artifact.size,
      "mime" => artifact.mime,
      "metadata" => %{}
    }

    {:ok, response} = mcp(state, "register_artifact", args)
    {:ok, ^response} = mcp(state, "register_artifact", args)
  end

  defp sampling_revision(state) do
    {:ok, context} = mcp(state, "get_context", %{})
    context.attempt.sampling_revision_id
  end

  defp harness_digest(state) do
    {:ok, context} = mcp(state, "get_context", %{})
    context.campaign.spec_revision.protected_digest
  end

  defp mcp(state, tool, args),
    do: Coordinator.mcp_call(state.mcp.token, tool, args, state.mcp.coordinator)

  defp complete_turn(server) do
    if Process.alive?(server) do
      emit(server, :message_delta, %{delta: "fixture complete"})
      emit(server, :turn_completed, %{status: "completed"})
    end
  end

  defp stop_task(pid) when is_pid(pid) do
    if Process.alive?(pid) and pid != self(), do: Process.exit(pid, :kill)
  end

  defp stop_task(_pid), do: :ok

  defp store_task(server, task_pid) do
    Agent.update(server, &%{&1 | task_pid: task_pid})
  catch
    :exit, _reason -> :ok
  end

  defp notify_start(state) do
    if pid = env(state)[:test_pid] do
      send(
        pid,
        {:attempt_started, state.mcp.attempt_id, self(), state.mcp.token, state.instructions}
      )
    end
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

  defp mode(state), do: env(state)[:mode] || :complete

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
    :exit, _reason -> :ok
  end
end
