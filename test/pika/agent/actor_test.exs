Code.require_file(Path.expand("../../support/fake_agent_backend.exs", __DIR__))
Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.ActorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, Actor, Directory, Work}
  alias Pika.Baseline.{Lifecycle, Questions}
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

    %{
      baseline: baseline,
      directory: directory,
      draft_id: draft.id,
      opts: opts,
      work: work,
      workspace: workspace
    }
  end

  test "opens a frozen Session, waits for user kickoff, and completes only after terminal MCP", %{
    draft_id: draft_id,
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

    insert_answered_question_batch(draft_id, session_id)

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

  test "persists distinct message items and trusts completed text", %{
    opts: opts,
    work: work
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, session_id}, 2_000

    assert :ok = Actor.kickoff(actor, "emit distinct message items")
    assert eventually(fn -> Actor.status(actor).phase == :awaiting_user end)

    [turn] = ConversationJournal.work_turns(work.role_id, work.kind, work.id)
    assert turn.session_id == session_id

    assert turn.output_messages == [
             %{
               "id" => "message-1",
               "role" => "assistant",
               "phase" => "commentary",
               "complete" => true,
               "content" => "Complete first update."
             },
             %{
               "id" => "message-2",
               "role" => "assistant",
               "phase" => "final_answer",
               "complete" => true,
               "content" => "Complete final answer."
             }
           ]

    assert [tool] = turn.mcp_calls
    assert tool["id"] == "command-1"
    assert tool["kind"] == "command"
    assert tool["name"] == "Read files"
    assert tool["status"] == "completed"
    assert tool["command_ref"] =~ ~r/^[0-9a-f]{64}$/

    assert turn.timeline_items == [
             %{sequence: 1, kind: "input", item_index: 0},
             %{sequence: 2, kind: "tool", item_index: 0},
             %{sequence: 3, kind: "output", item_index: 0},
             %{sequence: 4, kind: "output", item_index: 1}
           ]
  end

  test "steers an active turn and keeps replacement-turn completion isolated", %{
    opts: opts,
    work: work
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, session_id}, 2_000

    assert :ok = Actor.kickoff(actor, "hold turn open")
    assert Actor.status(actor).phase == :running

    assert :ok = Actor.kickoff(actor, "replace active turn")
    Process.sleep(25)
    assert Actor.status(actor).phase == :running

    [turn] = ConversationJournal.work_turns(work.role_id, work.kind, work.id)
    assert turn.session_id == session_id
    assert turn.partial

    assert turn.input_messages == [
             %{"role" => "user", "content" => "hold turn open"},
             %{"role" => "user", "content" => "replace active turn"}
           ]
  end

  test "records a user message before an active-turn steer can fail", %{
    opts: opts,
    work: work
  } do
    actor = start_supervised!({Actor, opts})
    assert_receive {:agent_actor_started, ^work, _session_id}, 2_000

    assert :ok = Actor.kickoff(actor, "hold turn open")
    assert Actor.status(actor).phase == :running

    assert {:error, {:turn_steer_failed, %Pika.AgentBackend.Error{code: :steer_failed}}} =
             Actor.kickoff(actor, "fail active turn")

    [turn] = ConversationJournal.work_turns(work.role_id, work.kind, work.id)

    assert turn.input_messages == [
             %{"role" => "user", "content" => "hold turn open"},
             %{"role" => "user", "content" => "fail active turn"}
           ]
  end

  test "continues Baseline Alignment automatically after question answers arrive", %{
    opts: opts,
    work: work
  } do
    questions = start_supervised!({Questions, name: nil})

    question_handler = fn binding, batch -> Questions.ask(binding, batch, questions) end
    actor = start_supervised!({Actor, Keyword.put(opts, :question_handler, question_handler)})
    assert_receive {:agent_actor_started, ^work, _session_id}, 2_000

    assert :ok = Actor.kickoff(actor, "collect requirements")
    assert eventually(fn -> Actor.status(actor).phase == :awaiting_user end)

    batch = [
      %{
        "id" => "metric",
        "question" => "主指标是什么？",
        "options" => [
          %{"label" => "Latency", "description" => "最小化延迟"},
          %{"label" => "Throughput", "description" => "最大化吞吐"}
        ]
      }
    ]

    invocation =
      Task.async(fn -> Actor.invoke(actor, "ask_questions", %{"questions" => batch}) end)

    assert eventually(fn -> Questions.pending(questions) != nil end)
    pending = Questions.pending(questions)
    answers = [%{"id" => "metric", "answer" => "Latency"}]

    assert {:ok, _completed} = Questions.answer(pending.id, answers, questions)
    assert Task.await(invocation) == {:ok, %{answers: answers}}

    assert eventually(fn ->
             turns = ConversationJournal.work_turns(work.role_id, work.kind, work.id)

             length(turns) == 2 and
               turns
               |> List.last()
               |> Map.fetch!(:input_messages)
               |> List.first()
               |> Map.fetch!("content")
               |> String.contains?("立即使用这些答案继续")
           end)

    assert Actor.status(actor).phase == :awaiting_user
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

  defp insert_answered_question_batch(revision_id, session_id) do
    now = System.system_time(:microsecond)

    Repo.query!(
      """
      INSERT INTO baseline_question_batches(
        id, optimization_id, baseline_revision_id, session_id, status,
        questions_json, answers_json, created_at, answered_at
      ) VALUES (?, 'optimization', ?, ?, 'answered', ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        revision_id,
        session_id,
        Jason.encode!([
          %{
            "id" => "measurement",
            "question" => "确认测量协议？",
            "options" => [
              %{"label" => "确认", "description" => "使用当前协议"},
              %{"label" => "修改", "description" => "提供自定义协议"}
            ]
          }
        ]),
        Jason.encode!([%{"id" => "measurement", "answer" => "确认"}]),
        now,
        now
      ]
    )
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
