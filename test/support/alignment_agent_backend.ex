defmodule Pika.Test.AlignmentAgentBackend do
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Event, Id, Session}
  alias Pika.Git
  alias Pika.Alignment.{ArtifactStore, Campaign}
  alias Pika.Test.AlignmentFixtures

  def start_link(_profile, sink),
    do:
      Agent.start_link(fn ->
        %{sink: sink, session: nil, cwd: nil, mcp: nil, instructions: nil, turn: nil}
      end)

  def open_session(server, cwd, model, effort, mcp, _skill_roots, instructions) do
    session = %Session{
      id: Id.new("session"),
      backend: :alignment_fake,
      backend_protocol: "alignment-fake-v1",
      backend_session_id: Id.new("provider"),
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
    turn_id = Id.new("turn")
    Agent.update(server, &%{&1 | turn: turn_id})
    emit(server, :turn_started)
    state = Agent.get(server, & &1)

    Task.start(fn ->
      run(input, state)

      if Process.alive?(server) do
        emit(server, :message_delta, %{delta: "fake workflow completed"})
        emit(server, :turn_completed, %{status: "completed"})
      end
    end)

    {:ok, turn_id}
  end

  def steer(server, input), do: start_turn(server, input)
  def interrupt(_server), do: :ok

  def close_session(server) do
    if Process.alive?(server) do
      emit(server, :process_exited, %{status: "closed", expected: true})
      Agent.stop(server)
    end

    :ok
  end

  def capabilities(_server), do: %{protocol: "alignment-fake-v1", native_steer: true}

  defp run(input, state) do
    cond do
      String.contains?(state.instructions, "Baseline Boundary Agent") -> baseline(state)
      String.contains?(input, "确认 Campaign Spec v1") -> setup_merge(state)
      String.contains?(state.instructions, "You are the Boundary Agent") -> alignment(state)
      true -> :ok
    end
  end

  defp alignment(state) do
    attrs = AlignmentFixtures.create_harness(state.cwd)
    token = state.mcp.token

    {:ok, _} =
      Campaign.mcp_call(token, "submit_spec", %{
        "idempotency_key" => "fake-spec",
        "spec" => AlignmentFixtures.spec()
      })

    {:ok, _} =
      Campaign.mcp_call(
        token,
        "submit_harness",
        Map.put(attrs, "idempotency_key", "fake-harness")
      )
  end

  defp setup_merge(state) do
    snapshot = Campaign.snapshot()
    workspace = snapshot.workspace.root
    repo = Path.join(workspace, "repo")
    setup = Path.join(workspace, "setup/1")
    Git.run!(setup, ["add", "."])
    Git.run!(setup, ["commit", "-m", "fake setup"])
    setup_sha = Git.run!(setup, ["rev-parse", "HEAD"])
    Git.run!(repo, ["merge", "--squash", setup_sha])
    Git.run!(repo, ["commit", "-m", "Fake setup"])
    best_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    {:ok, _} =
      Campaign.mcp_call(state.mcp.token, "complete_setup_merge", %{
        "idempotency_key" => "fake-merge",
        "base_sha" => snapshot.workspace.source_sha,
        "setup_sha" => setup_sha,
        "best_sha" => best_sha
      })
  end

  defp baseline(state) do
    snapshot = Campaign.snapshot()

    workspace = %{
      root: snapshot.workspace.root,
      artifacts: Path.join(snapshot.workspace.root, "artifacts")
    }

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, snapshot.best_sha, snapshot.skill.sha)

    Enum.each(
      [samples, correctness, profiler] ++ AlignmentFixtures.baseline_dependency_paths(),
      fn relative ->
        {:ok, artifact} = ArtifactStore.register(workspace.root, relative)

        {:ok, _} =
          Campaign.mcp_call(state.mcp.token, "register_artifact", %{
            "idempotency_key" => "fake-artifact-#{relative}",
            "kind" => "baseline",
            "relative_path" => relative,
            "sha256" => artifact.sha256,
            "size" => artifact.size,
            "mime" => artifact.mime,
            "metadata" => %{}
          })
      end
    )

    {:ok, _} =
      Campaign.mcp_call(state.mcp.token, "submit_baseline", %{
        "idempotency_key" => "fake-baseline",
        "measured_sha" => snapshot.best_sha,
        "samples_artifact" => samples,
        "correctness_artifact" => correctness,
        "profiler_artifact" => profiler,
        "summary" => "fake end-to-end baseline"
      })

    {:ok, _} =
      Campaign.mcp_call(state.mcp.token, "submit_iteration_sample", %{
        "idempotency_key" => "fake-sampling",
        "case_ids" => ["target_case"],
        "reasons" => %{"target_case" => "representative target"},
        "estimated_cost" => %{
          "iteration_seconds" => 1.0,
          "full_seconds" => 1.0,
          "savings_ratio" => 0.0
        },
        "summary" => "fixture initial sample"
      })
  end

  defp emit(server, type, data \\ %{}) do
    state = Agent.get(server, & &1)

    event =
      Event.new(type, :alignment_fake, state.session.id, %{
        backend_session_id: state.session.backend_session_id,
        turn_id: state.turn,
        data: data
      })

    send(state.sink, {:pika_backend_event, event})
    :ok
  catch
    :exit, _reason -> :ok
  end
end
