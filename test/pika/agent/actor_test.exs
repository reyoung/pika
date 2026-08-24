Code.require_file(Path.expand("../../support/fake_agent_backend.exs", __DIR__))
Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.ActorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, Actor, Directory, Work}
  alias Pika.Baseline.Lifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.{FakeAgentBackend, V2BaselineFixtures}
  alias Pika.Repo

  setup do
    root = Path.join(System.tmp_dir!(), "pika-v2-actor-#{System.unique_integer([:positive])}")
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

    assert {:ok, draft} = Lifecycle.ensure_draft(baseline.root)
    directory = start_supervised!({Directory, name: nil})

    work = %Work{
      role_id: "baseline_alignment",
      kind: :baseline_revision,
      id: to_string(draft.id)
    }

    opts = [
      work: work,
      workspace: workspace,
      directory: directory,
      backend_modules: %{codex_app_server: FakeAgentBackend},
      mcp_url: "http://127.0.0.1:4000/mcp",
      notify: self()
    ]

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline: baseline, directory: directory, opts: opts, work: work, workspace: workspace}
  end

  test "opens a frozen Session, waits for user kickoff, and completes only after terminal MCP", %{
    opts: opts,
    work: work
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, session_id}, 2_000

    assert %{phase: :awaiting_user_kickoff, session_id: ^session_id} = Actor.status(actor)
    assert :ok = Actor.kickoff(actor, "请开始梳理 Baseline")
    assert eventually(fn -> Actor.status(actor).phase == :awaiting_user end)

    assert {:ok, context} = Actor.invoke(actor, "get_context", %{})
    assert context.context_file =~ "/agent-sessions/#{session_id}/context/context.json"

    monitor = Process.monitor(actor)

    assert {:ok, %{"status" => "awaiting_review"}} =
             Actor.invoke(actor, "submit_baseline_definition", %{
               "definition_path" => "baseline-definition.json",
               "idempotency_key" => "definition-1"
             })

    assert_receive {:agent_actor_completed, ^work}, 2_000
    assert_receive {:DOWN, ^monitor, :process, ^actor, :normal}, 2_000
    assert ConversationJournal.session(session_id).status == "completed"
  end

  test "a killed Actor is replaced by a new Backend Session with recovery-01", %{
    directory: directory,
    opts: opts,
    work: work,
    workspace: workspace
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, first_session_id}, 2_000
    assert :ok = Actor.kickoff(actor, "第一轮对话")
    assert eventually(fn -> Actor.status(actor).phase == :awaiting_user end)

    Process.exit(actor, :kill)

    assert eventually(fn ->
             Directory.lookup_work(work.role_id, work.kind, work.id, directory) ==
               {:error, :not_found}
           end)

    replacement = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, second_session_id}, 2_000
    refute second_session_id == first_session_id

    assert eventually(fn -> Actor.status(replacement).phase == :awaiting_user end)
    assert ConversationJournal.session(first_session_id).status == "interrupted"
    assert ConversationJournal.session(second_session_id).session_sequence == 2
    assert ConversationJournal.session(second_session_id).recovery_sequence == 1

    recovery_root =
      Path.join([workspace, "agent-sessions", second_session_id, "recovery-01"])

    assert File.regular?(Path.join(recovery_root, "messages.jsonl"))
    assert File.regular?(Path.join(recovery_root, "recovery.json"))
    assert File.read!(Path.join(recovery_root, "messages.jsonl")) =~ "第一轮对话"

    prompt_session = ConversationJournal.session(second_session_id)
    assert prompt_session.provider_session_id != nil
  end

  test "recovery does not invent the first Alignment User Turn", %{
    directory: directory,
    opts: opts,
    work: work
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, _first_session_id}, 2_000
    assert Actor.status(actor).phase == :awaiting_user_kickoff
    Process.exit(actor, :kill)

    assert eventually(fn ->
             Directory.lookup_work(work.role_id, work.kind, work.id, directory) ==
               {:error, :not_found}
           end)

    assert {:ok, replacement} = Actor.start_link(opts)
    assert_receive {:agent_actor_started, ^work, _second_session_id}, 2_000
    assert Actor.status(replacement).phase == :awaiting_user_kickoff
    Actor.stop(replacement)
  end

  defp eventually(check, attempts \\ 100)
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
