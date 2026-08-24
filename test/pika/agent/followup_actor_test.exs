Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Test.FollowupBackend do
  @behaviour Pika.AgentBackend

  alias Pika.Agent.Actor
  alias Pika.AgentBackend.{Event, Id, Session}

  def start_link(_profile, sink),
    do:
      Agent.start_link(fn ->
        %{sink: sink, session: nil, mcp: nil, instructions: nil, turn: nil}
      end)

  def open_session(server, cwd, model, effort, mcp, _skills, instructions) do
    session = %Session{
      id: Id.new("session"),
      backend: :followup_fake,
      backend_protocol: "v2-followup-fake",
      backend_session_id: Id.new("provider"),
      cwd: cwd,
      model: model,
      reasoning_effort: effort,
      jsonl_path: "/dev/null"
    }

    Agent.update(server, &%{&1 | session: session, mcp: mcp, instructions: instructions})
    {:ok, session}
  end

  def start_turn(server, _input) do
    turn_id = Id.new("turn")
    Agent.update(server, &%{&1 | turn: turn_id})
    state = Agent.get(server, & &1)
    emit(state, :turn_started)

    Task.start(fn ->
      if String.contains?(state.instructions, "Follow-up Agent") do
        {:ok, _value} =
          Actor.invoke(state.mcp.coordinator, "submit_followup_message", %{
            "message" => "请继续核对全量结果并提交 Baseline Verification Result。",
            "idempotency_key" => "generated-followup"
          })
      end

      emit(state, :message_delta, %{delta: "turn complete"})
      emit(state, :turn_completed, %{status: "completed"})
    end)

    {:ok, turn_id}
  end

  def steer(server, input), do: start_turn(server, input)
  def interrupt(_server), do: :ok
  def close_session(server), do: if(Process.alive?(server), do: Agent.stop(server), else: :ok)
  def capabilities(_server), do: %{}

  defp emit(state, type, data \\ %{}) do
    event =
      Event.new(type, :followup_fake, state.session.id, %{turn_id: state.turn, data: data})

    send(state.sink, {:pika_backend_event, event})
  end
end

defmodule Pika.Agent.FollowupActorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, Actor, Directory, Work}
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Followup.Lifecycle, as: FollowupLifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.{V2BaselineFixtures, FollowupBackend}
  alias Pika.Repo

  test "a dedicated Follow-up Actor delivers a generated User Turn back to the target Actor" do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-followup-actor-#{System.unique_integer([:positive])}")

    on_exit(fn -> File.rm_rf!(root) end)
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(workspace, "pika.yaml")
    File.write!(config_path, yaml(baseline.repo, workspace))
    assert {:ok, config} = Config.load(config_path)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()

    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, baseline.development_sha)

    assert {:ok, _draft} = BaselineLifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             BaselineLifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, verifying} =
             BaselineLifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    directory = start_supervised!({Directory, name: nil})
    backend_modules = %{codex_app_server: FollowupBackend}

    target_work = %Work{
      role_id: "baseline_verify",
      kind: :baseline_revision,
      id: to_string(verifying.id)
    }

    target =
      start_supervised!(
        {Actor,
         work: target_work,
         workspace: workspace,
         directory: directory,
         backend_modules: backend_modules,
         mcp_url: "http://127.0.0.1:4000/mcp",
         notify: self()}
      )

    assert_receive {:agent_actor_started, ^target_work, _session_id}, 2_000
    assert eventually(fn -> Actor.status(target).phase == :awaiting_followup end)

    [projection] = FollowupLifecycle.project_work()

    Process.exit(target, :kill)

    assert eventually(fn ->
             Directory.lookup_work(
               target_work.role_id,
               target_work.kind,
               target_work.id,
               directory
             ) == {:error, :not_found}
           end)

    assert {:ok, replacement} =
             Actor.start_link(
               work: target_work,
               workspace: workspace,
               directory: directory,
               backend_modules: backend_modules,
               mcp_url: "http://127.0.0.1:4000/mcp",
               notify: self()
             )

    assert_receive {:agent_actor_started, ^target_work, _replacement_session_id}, 2_000
    assert eventually(fn -> Actor.status(replacement).phase == :awaiting_followup end)

    followup_work = %Work{
      role_id: projection.role_id,
      kind: projection.work_kind,
      id: projection.work_id,
      payload: %{request: projection.request}
    }

    assert {:ok, _generator} =
             Actor.start_link(
               work: followup_work,
               workspace: workspace,
               directory: directory,
               backend_modules: backend_modules,
               mcp_url: "http://127.0.0.1:4000/mcp",
               notify: self()
             )

    assert_receive {:agent_actor_started, ^followup_work, _generator_session_id}, 2_000

    assert eventually(fn ->
             turns =
               ConversationJournal.work_turns(
                 "baseline_verify",
                 "baseline_revision",
                 to_string(verifying.id)
               )

             length(turns) >= 2
           end)

    turns =
      ConversationJournal.work_turns(
        "baseline_verify",
        "baseline_revision",
        to_string(verifying.id)
      )

    assert Enum.at(turns, 1).input_messages == [
             %{
               "role" => "user",
               "content" => "请继续核对全量结果并提交 Baseline Verification Result。"
             }
           ]

    assert eventually(fn ->
             FollowupLifecycle.fetch(projection.request.id) |> elem(1) |> Map.get(:status) ==
               "target_incomplete"
           end)

    assert eventually(fn -> length(FollowupLifecycle.project_work()) == 1 end)
    if Process.alive?(replacement), do: Actor.stop(replacement)
  end

  defp eventually(check, attempts \\ 200)
  defp eventually(_check, 0), do: false

  defp eventually(check, attempts) do
    if check.() do
      true
    else
      Process.sleep(10)
      eventually(check, attempts - 1)
    end
  catch
    :exit, _reason -> false
  end

  defp yaml(repo, workspace) do
    """
    version: 2
    repo: #{repo}
    workspace: #{workspace}
    agents:
      baseline_alignment:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      baseline_verify:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
        max_followups: 3
      baseline_verify_followup:
        backend: codex
        approval_policy: never
        sandbox: read-only
        generator_max_attempts: 2
      iteration:
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
