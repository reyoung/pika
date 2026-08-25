defmodule PikaWeb.OptimizationLiveTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, SessionBinding}
  alias Pika.Baseline.{Lifecycle, Questions}
  alias Pika.Optimization.Bootstrap
  alias Pika.{Auth, Git, Repo}

  defmodule SymphonyStub do
    use GenServer

    def start_link(_opts), do: GenServer.start_link(__MODULE__, :ok, name: Pika.Agent.Symphony)
    def init(:ok), do: {:ok, nil}

    def handle_call({:kickoff, _role, _work_id, _message}, _from, state),
      do: {:reply, :ok, state}

    def handle_call({:restart_session, _session_id}, _from, state),
      do: {:reply, :ok, state}
  end

  test "renders the singleton v2 Optimization without removed workflow controls" do
    root = Path.join(System.tmp_dir!(), "pika-v2-live-#{System.unique_integer([:positive])}")
    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    File.mkdir_p!(workspace)
    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "kernel.py"), "def run(): return 1\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "initial"])
    config_path = Path.join(workspace, "pika.yaml")
    File.write!(config_path, yaml(repo, workspace))
    on_exit(fn -> File.rm_rf!(root) end)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert {:ok, bootstrap} = Bootstrap.start_link(name: nil, config_path: config_path)
    baseline = Lifecycle.latest_revision()

    assert {:ok, session} =
             ConversationJournal.start_session(
               "baseline_alignment",
               :baseline_revision,
               to_string(baseline.id),
               %{
                 "backend" => "codex_app_server",
                 "model" => "gpt-test",
                 "reasoning_effort" => "high",
                 "approval_policy" => "never",
                 "sandbox_policy" => "workspace-write"
               },
               "system prompt",
               "context"
             )

    assert {:ok, turn} =
             ConversationJournal.start_turn(session.id, [
               %{"role" => "user", "content" => "Inspect the benchmark contract"}
             ])

    assert {:ok, _turn} =
             ConversationJournal.append_output(turn.id, %{
               "role" => "assistant",
               "content" => "I am checking the cases and metrics."
             })

    assert {:ok, _turn} =
             ConversationJournal.append_mcp_call(turn.id, %{
               "name" => "get_context",
               "status" => "completed"
             })

    assert {:ok, _turn} = ConversationJournal.finish_turn(turn.id, "completed")
    assert {:ok, _session} = ConversationJournal.interrupt_session(session.id, "test restart")

    assert {:ok, active_session} =
             ConversationJournal.start_session(
               "baseline_alignment",
               :baseline_revision,
               to_string(baseline.id),
               %{
                 "backend" => "codex_app_server",
                 "model" => "gpt-test",
                 "reasoning_effort" => "high",
                 "approval_policy" => "never",
                 "sandbox_policy" => "workspace-write"
               },
               "system prompt two",
               "context two"
             )

    assert {:ok, active_turn} =
             ConversationJournal.start_turn(active_session.id, [
               %{"role" => "user", "content" => "Confirm the measurement protocol"}
             ])

    assert {:ok, _streaming_turn} =
             ConversationJournal.stream_output(active_turn.id, %{
               "role" => "assistant",
               "content" => "I am streaming the current Baseline analysis."
             })

    start_supervised!({Questions, name: Questions})

    questions = [
      %{
        "id" => "target",
        "question" => "Which target should be optimized?",
        "options" => [
          %{"label" => "A", "description" => "Use target A"},
          %{"label" => "B", "description" => "Use target B"}
        ]
      },
      %{
        "id" => "metric",
        "question" => "Which metric is primary?",
        "options" => [
          %{"label" => "Latency", "description" => "Minimize latency"},
          %{"label" => "Throughput", "description" => "Maximize throughput"}
        ]
      }
    ]

    binding = %SessionBinding{
      actor: self(),
      role_id: "baseline_alignment",
      work_kind: :baseline_revision,
      work_id: to_string(baseline.id),
      session_id: active_session.id,
      context_file: Path.join(workspace, "context.json"),
      work_root: workspace
    }

    question_task = Task.async(fn -> Questions.ask(binding, questions) end)
    assert eventually(fn -> Questions.pending() != nil end)
    %{marker: marker} = Auth.generate()

    assert {:ok, socket} =
             PikaWeb.OptimizationLive.mount(
               %{},
               %{"pika_auth" => marker},
               %Phoenix.LiveView.Socket{}
             )

    html =
      socket.assigns
      |> PikaWeb.OptimizationLive.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert html =~ "Optimization Console"
    assert html =~ "Optimization overview"
    assert html =~ ~s(class="ops-shell")
    assert html =~ "Baseline"
    assert html =~ "Review"
    assert html =~ "Current Best"
    assert html =~ "Iteration Guidance"
    assert html =~ "Session details &amp; conversation"
    assert html =~ "Codex app server"
    assert html =~ "gpt-test"
    assert html =~ "I am checking the cases and metrics."
    assert html =~ "Confirm the measurement protocol"
    assert html =~ "I am streaming the current Baseline analysis."
    assert html =~ "get_context"
    assert html =~ ~s(class="ops-chat-message ops-chat-message-user")
    assert html =~ ~s(class="ops-chat-message ops-chat-message-assistant")
    assert html =~ ~s(phx-hook="ConversationScroll")
    assert html =~ ~s(phx-click="toggle_agent_session")
    assert length(:binary.matches(html, ~s(aria-expanded="true"))) == 1
    assert socket.assigns.open_session_id == active_session.id
    assert html =~ "Interrupt &amp; restart"
    assert html =~ ~s(phx-click="restart_agent_session")
    assert html =~ ~s(phx-value-session="#{active_session.id}")
    assert html =~ ~s(phx-disable-with="Restarting…")
    assert html =~ ~s(data-submit-on-enter="true")
    assert html =~ ~s(id="baseline-message-form")
    assert html =~ "Enter to send · ⌘/Ctrl/Shift+Enter for a new line"
    assert html =~ ~s(name="answers[target][choice]")
    assert html =~ ~s(name="answers[target][custom]")
    assert html =~ "Write your own answer"
    assert html =~ "No attempts yet"
    assert html =~ "No progress summary yet"
    refute html =~ ">Sync<"
    refute html =~ "Campaign singleton"
    refute html =~ "diagnostic-hero"

    {session_one_position, _length} = :binary.match(html, "Session 1")
    {session_two_position, _length} = :binary.match(html, "Session 2")
    assert session_one_position < session_two_position

    assert {:noreply, opened_first_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "toggle_agent_session",
               %{"session" => session.id},
               socket
             )

    assert opened_first_socket.assigns.open_session_id == session.id

    assert {:noreply, collapsed_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "toggle_agent_session",
               %{"session" => session.id},
               opened_first_socket
             )

    assert collapsed_socket.assigns.open_session_id == nil

    assert {:noreply, answered_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "answer_questions",
               %{
                 "answers" => %{
                   "target" => %{"choice" => "A", "custom" => ""},
                   "metric" => %{
                     "choice" => "Latency",
                     "custom" => "Use P95 latency and report throughput"
                   }
                 }
               },
               socket
             )

    assert Task.await(question_task) ==
             {:ok,
              [
                %{"id" => "target", "answer" => "A"},
                %{
                  "id" => "metric",
                  "answer" => "Use P95 latency and report throughput",
                  "custom" => true
                }
              ]}

    start_supervised!(SymphonyStub)

    assert {:noreply, restarted_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "restart_agent_session",
               %{"session" => active_session.id},
               answered_socket
             )

    assert {:noreply, sent_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "send_message",
               %{"message" => %{"body" => "Keep the case set deterministic"}},
               restarted_socket
             )

    assert sent_socket.assigns.message_form.params == %{"body" => ""}

    assert Phoenix.LiveView.Utils.get_push_events(sent_socket) == [
             ["clear-form", %{id: "baseline-message-form"}]
           ]

    GenServer.stop(bootstrap)
    Auth.clear()
  end

  defp eventually(check, attempts \\ 100)
  defp eventually(_check, 0), do: false

  defp eventually(check, attempts) do
    if check.() do
      true
    else
      Process.sleep(5)
      eventually(check, attempts - 1)
    end
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
