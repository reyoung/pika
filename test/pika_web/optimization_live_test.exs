defmodule PikaWeb.OptimizationLiveTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{ConversationJournal, SessionBinding}
  alias Pika.Baseline.{Lifecycle, Questions}
  alias Pika.Optimization.Bootstrap
  alias Pika.{Auth, Git, Repo}

  defmodule SymphonyStub do
    use GenServer

    def start_link(opts),
      do: GenServer.start_link(__MODULE__, Keyword.get(opts, :owner), name: Pika.Agent.Symphony)

    def init(owner), do: {:ok, owner}

    def handle_call({:kickoff, role, work_id, message}, _from, owner) do
      if owner, do: send(owner, {:kickoff, role, work_id, message})
      {:reply, :ok, owner}
    end

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
               "content" => "I am checking the cases and metrics.",
               "phase" => "commentary",
               "complete" => true
             })

    assert {:ok, _turn} =
             ConversationJournal.append_mcp_call(turn.id, %{
               "name" => "get_context",
               "status" => "completed"
             })

    assert {:ok, _turn} =
             ConversationJournal.append_output(turn.id, %{
               "role" => "assistant",
               "content" => "The measurement plan is ready for review.",
               "phase" => "final_answer",
               "complete" => true
             })

    command_ref = String.duplicate("a", 64)

    assert {:ok, _turn} =
             ConversationJournal.upsert_tool_call(turn.id, %{
               "id" => "command-1",
               "kind" => "command",
               "name" => "Read files",
               "summary" => "sed -n '1,80p' lib/pika.ex",
               "status" => "completed",
               "command_ref" => command_ref
             })

    assert {:ok, _turn} =
             ConversationJournal.append_mcp_call(turn.id, %{
               "name" => "save_context",
               "status" => "completed"
             })

    assert {:ok, _turn} =
             ConversationJournal.append_input(turn.id, %{
               "role" => "user",
               "content" => "Run every case in one process."
             })

    assert {:ok, _turn} =
             ConversationJournal.append_output(turn.id, %{
               "role" => "assistant",
               "content" => "I will reuse one torchrun process.",
               "phase" => "commentary",
               "complete" => true
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
    assert html =~ "The measurement plan is ready for review."
    assert html =~ "Update 1"
    assert html =~ "Final answer"
    assert html =~ "ops-chat-message-final"
    assert html =~ "Confirm the measurement protocol"
    assert html =~ "I am streaming the current Baseline analysis."
    assert html =~ "get_context"
    assert html =~ "2 tool calls"
    assert html =~ ~s(class="ops-chat-tools")
    assert length(:binary.matches(html, ~s(class="ops-chat-tools"))) == 2
    assert html =~ ~s(phx-hook="PersistDetails")
    assert html =~ "Read files"
    assert html =~ "View output"
    assert html =~ ~s(phx-click="open_command_console")
    assert html =~ ~s(phx-value-ref="#{command_ref}")
    assert html =~ ~s(class="ops-chat-message ops-chat-message-user")
    assert html =~ ~s(class="ops-chat-message ops-chat-message-assistant ops-chat-message-)
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
    assert html =~ ~s(phx-disable-with="Sending…")
    assert html =~ "Enter to send · ⌘/Ctrl/Shift+Enter for a new line"
    assert html =~ ~s(name="answers[target][choice]")
    assert html =~ ~s(name="answers[target][custom]")
    assert html =~ ~s(phx-change="change_question_answers")
    assert html =~ "Write your own answer"
    assert html =~ ~s(phx-value-tab="attempts")
    assert html =~ "No progress summary yet"
    refute html =~ "Performance timeline"
    refute html =~ ">Sync<"
    refute html =~ "Campaign singleton"
    refute html =~ "diagnostic-hero"

    artifact_html =
      socket
      |> Phoenix.Component.assign(:baseline, %{baseline | definition_artifact_id: 1})
      |> Phoenix.Component.assign(:review_bundle, %{
        definition: "{}",
        smoke_verify: "{}",
        smoke_benchmark: "ok",
        verification_result: "{}"
      })
      |> Map.fetch!(:assigns)
      |> PikaWeb.OptimizationLive.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert artifact_html =~
             ~s(id="baseline-definition-details-#{baseline.id}" phx-hook="PersistDetails" open)

    assert artifact_html =~
             ~s(id="baseline-smoke-verify-details-#{baseline.id}" phx-hook="PersistDetails")

    assert artifact_html =~
             ~s(id="baseline-smoke-benchmark-details-#{baseline.id}" phx-hook="PersistDetails")

    assert artifact_html =~
             ~s(id="baseline-verification-result-details-#{baseline.id}" phx-hook="PersistDetails")

    {session_one_position, _length} = :binary.match(html, "Session 1")
    {session_two_position, _length} = :binary.match(html, "Session 2")
    assert session_one_position < session_two_position

    {initial_user_position, _length} = :binary.match(html, "Inspect the benchmark contract")
    {first_agent_position, _length} = :binary.match(html, "I am checking the cases and metrics.")
    {first_tool_position, _length} = :binary.match(html, "get_context")

    {second_agent_position, _length} =
      :binary.match(html, "The measurement plan is ready for review.")

    {second_tool_position, _length} = :binary.match(html, "Read files")
    {third_tool_position, _length} = :binary.match(html, "save_context")
    {user_reply_position, _length} = :binary.match(html, "Run every case in one process.")
    {final_agent_position, _length} = :binary.match(html, "I will reuse one torchrun process.")

    assert initial_user_position < first_agent_position
    assert first_agent_position < first_tool_position
    assert first_tool_position < second_agent_position
    assert second_agent_position < second_tool_position
    assert second_tool_position < third_tool_position
    assert third_tool_position < user_reply_position
    assert user_reply_position < final_agent_position

    assert {:noreply, opened_first_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "toggle_agent_session",
               %{"session" => session.id},
               socket
             )

    assert opened_first_socket.assigns.open_session_id == session.id

    opened_first_html =
      opened_first_socket.assigns
      |> PikaWeb.OptimizationLive.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert length(:binary.matches(opened_first_html, ~s(aria-expanded="true"))) == 1

    assert opened_first_html =~
             ~s(phx-value-session="#{session.id}" aria-expanded="true")

    assert opened_first_html =~
             ~s(phx-value-session="#{active_session.id}" aria-expanded="false")

    assert {:noreply, collapsed_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "toggle_agent_session",
               %{"session" => session.id},
               opened_first_socket
             )

    assert collapsed_socket.assigns.open_session_id == nil

    assert {:noreply, refreshed_collapsed_socket} =
             PikaWeb.OptimizationLive.handle_info(:refresh, collapsed_socket)

    assert refreshed_collapsed_socket.assigns.open_session_id == nil

    refreshed_collapsed_html =
      refreshed_collapsed_socket.assigns
      |> PikaWeb.OptimizationLive.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert length(:binary.matches(refreshed_collapsed_html, ~s(aria-expanded="true"))) == 0

    assert {:noreply, console_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "open_command_console",
               %{"ref" => command_ref},
               socket
             )

    assert console_socket.assigns.command_console_ref == command_ref
    assert console_socket.assigns.command_console.ref == command_ref

    assert {:noreply, closed_console_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "close_command_console",
               %{},
               console_socket
             )

    assert closed_console_socket.assigns.command_console_ref == nil
    assert closed_console_socket.assigns.command_console == nil

    edited_answers = %{
      "target" => %{"choice" => "A", "custom" => ""},
      "metric" => %{
        "choice" => "Latency",
        "custom" => "Use P95 latency and report throughput"
      }
    }

    assert {:noreply, edited_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "change_question_answers",
               %{"answers" => edited_answers},
               socket
             )

    assert edited_socket.assigns.question_answers["metric"] == %{
             "choice" => "",
             "custom" => "Use P95 latency and report throughput"
           }

    assert {:noreply, refreshed_socket} =
             PikaWeb.OptimizationLive.handle_info(:refresh, edited_socket)

    refreshed_html =
      refreshed_socket.assigns
      |> PikaWeb.OptimizationLive.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert refreshed_html =~ "Use P95 latency and report throughput"
    assert refreshed_html =~ ~s(value="A" checked)
    refute refreshed_html =~ ~s(value="Latency" checked)

    assert {:noreply, answered_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "answer_questions",
               %{"answers" => edited_answers},
               refreshed_socket
             )

    assert answered_socket.assigns.question_answers == %{}
    assert answered_socket.assigns.question_batch_id == nil

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

    start_supervised!({SymphonyStub, owner: self()})

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

    assert_receive {:kickoff, "baseline_alignment", baseline_id,
                    "Keep the case set deterministic"}

    assert baseline_id == to_string(baseline.id)

    assert Phoenix.LiveView.Utils.get_push_events(sent_socket) == [
             ["clear-form", %{id: "baseline-message-form"}]
           ]

    now = System.system_time(:microsecond)

    Repo.query!(
      "UPDATE baseline_revisions SET status = 'verifying', updated_at = ? WHERE id = ?",
      [now, baseline.id]
    )

    assert {:noreply, _verify_message_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "send_message",
               %{"message" => %{"body" => "Finish aggregating the completed benchmark"}},
               sent_socket
             )

    assert_receive {:kickoff, "baseline_verify", ^baseline_id,
                    "Finish aggregating the completed benchmark"}

    assert {:ok, _session} =
             ConversationJournal.interrupt_session(active_session.id, "revision superseded")

    now = System.system_time(:microsecond)

    Repo.query!(
      "UPDATE baseline_revisions SET status = 'superseded', terminal_reason = 'test verification rejection', updated_at = ? WHERE id = ?",
      [now, baseline.id]
    )

    Repo.query!(
      "UPDATE optimizations SET status = 'aligning_baseline', updated_at = ? WHERE id = 'optimization'",
      [now]
    )

    next_root = Path.join([workspace, "baseline", "revisions", "000001"])
    File.mkdir_p!(next_root)
    assert {:ok, next_baseline} = Lifecycle.ensure_draft(next_root)

    assert {:ok, next_session} =
             ConversationJournal.start_session(
               "baseline_alignment",
               :baseline_revision,
               to_string(next_baseline.id),
               %{"backend" => "codex_app_server", "model" => "gpt-next"},
               "next system prompt",
               "next context"
             )

    assert {:ok, revisions_socket} =
             PikaWeb.OptimizationLive.mount(
               %{},
               %{"pika_auth" => marker},
               %Phoenix.LiveView.Socket{}
             )

    revisions_html = render_socket(revisions_socket)
    assert revisions_socket.assigns.baseline.revision == 1
    assert revisions_html =~ ~s(class="ops-baseline-tabs")
    assert revisions_html =~ ~s(role="tablist")
    assert revisions_html =~ ~s(phx-value-revision="0")
    assert revisions_html =~ ~s(phx-value-revision="1")
    assert revisions_html =~ next_session.id

    assert {:noreply, revision_zero_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "select_baseline_revision",
               %{"revision" => "0"},
               revisions_socket
             )

    revision_zero_html = render_socket(revision_zero_socket)
    assert revision_zero_socket.assigns.baseline.revision == 0
    refute revision_zero_socket.assigns.current_baseline?
    assert revision_zero_html =~ "test verification rejection"
    assert revision_zero_html =~ "I am checking the cases and metrics."
    assert revision_zero_html =~ session.id
    refute revision_zero_html =~ next_session.id
    refute revision_zero_html =~ ~s(id="baseline-message-form")

    now = System.system_time(:microsecond)
    base_sha = Git.run!(repo, ["rev-parse", "HEAD"])

    Repo.query!(
      "INSERT INTO sampling_revisions(optimization_id, baseline_revision_id, sequence, cause, created_at) VALUES ('optimization', ?, 1, 'test', ?)",
      [next_baseline.id, now]
    )

    [[sampling_revision_id]] = Repo.query!("SELECT last_insert_rowid()").rows

    for {attempt_id, status, failure_reason} <- [
          {1, "rejected", "target setup failed"},
          {2, "iterating", nil}
        ] do
      Repo.query!(
        """
        INSERT INTO attempts(
          id, optimization_id, status, work_relative_path, branch, slot_index,
          base_best_revision, base_sha, sampling_revision_id, current_iteration_round,
          failure_reason, inserted_at, updated_at
        ) VALUES (?, 'optimization', ?, ?, ?, 0, 0, ?, ?, 1, ?, ?, ?)
        """,
        [
          attempt_id,
          status,
          "attempts/#{String.pad_leading(to_string(attempt_id), 6, "0")}",
          "pika/attempt/#{String.pad_leading(to_string(attempt_id), 6, "0")}",
          base_sha,
          sampling_revision_id,
          failure_reason,
          now + attempt_id,
          now + attempt_id
        ]
      )
    end

    Repo.query!(
      "UPDATE optimizations SET status = 'optimizing', best_sha = ?, updated_at = ? WHERE id = 'optimization'",
      [base_sha, now]
    )

    assert {:ok, iteration_session} =
             ConversationJournal.start_session(
               "iteration",
               :attempt,
               "2",
               %{
                 "backend" => "cursor_acp",
                 "model" => "cursor-test",
                 "reasoning_effort" => "low"
               },
               "iteration system prompt",
               "iteration context"
             )

    assert {:ok, iteration_turn} =
             ConversationJournal.start_turn(iteration_session.id, [
               %{"role" => "user", "content" => "Optimize the sampled attention cases"}
             ])

    assert {:ok, _iteration_turn} =
             ConversationJournal.stream_output(iteration_turn.id, %{
               "role" => "assistant",
               "content" => "I am profiling the current Iteration candidate."
             })

    assert {:noreply, transitioned_socket} =
             PikaWeb.OptimizationLive.handle_info(:refresh, revisions_socket)

    assert transitioned_socket.assigns.workspace_tab == "attempts"
    assert transitioned_socket.assigns.selected_attempt_id == 2
    assert transitioned_socket.assigns.open_session_id == iteration_session.id

    assert {:ok, attempts_socket} =
             PikaWeb.OptimizationLive.mount(
               %{},
               %{"pika_auth" => marker},
               %Phoenix.LiveView.Socket{}
             )

    attempts_html = render_socket(attempts_socket)
    assert attempts_socket.assigns.workspace_tab == "attempts"
    assert attempts_socket.assigns.selected_attempt_id == 2
    assert attempts_socket.assigns.open_session_id == iteration_session.id
    assert attempts_html =~ "Iteration &amp; Integration"
    assert attempts_html =~ "Iteration Agent"
    assert attempts_html =~ "I am profiling the current Iteration candidate."
    assert attempts_html =~ "Performance timeline"
    assert attempts_html =~ "Test-case improvement over time"
    assert attempts_html =~ ~s(phx-change="filter_performance")
    refute attempts_html =~ ~s(phx-hook="PerformanceChart")
    assert attempts_html =~ "No matching measurements yet"
    assert attempts_html =~ ~s(class="ops-attempt-tabs")
    assert attempts_html =~ ~s(role="tablist" aria-label="Optimization attempts")
    assert attempts_html =~ ~s(phx-value-attempt="2" aria-selected="true")

    {attempt_one_position, _length} = :binary.match(attempts_html, ~s(phx-value-attempt="1"))
    {attempt_two_position, _length} = :binary.match(attempts_html, ~s(phx-value-attempt="2"))
    assert attempt_one_position < attempt_two_position

    assert {:noreply, filtered_performance_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "filter_performance",
               %{"case_filter" => "1", "metric_filter" => "latency_us"},
               attempts_socket
             )

    assert filtered_performance_socket.assigns.performance_case_filter == "1"
    assert filtered_performance_socket.assigns.performance_metric_filter == "latency_us"

    assert {:noreply, reset_performance_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "reset_performance_filter",
               %{},
               filtered_performance_socket
             )

    assert reset_performance_socket.assigns.performance_case_filter == ""
    assert reset_performance_socket.assigns.performance_metric_filter == "all"

    assert {:noreply, first_attempt_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "select_attempt",
               %{"attempt" => "1"},
               attempts_socket
             )

    assert first_attempt_socket.assigns.selected_attempt_id == 1
    first_attempt_html = render_socket(first_attempt_socket)
    assert first_attempt_html =~ "target setup failed"
    assert first_attempt_html =~ ~s(phx-value-attempt="1" aria-selected="true")

    refute first_attempt_html =~ "I am profiling the current Iteration candidate."

    assert {:noreply, baseline_tab_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "select_workspace_tab",
               %{"tab" => "baseline"},
               attempts_socket
             )

    assert baseline_tab_socket.assigns.workspace_tab == "baseline"
    assert render_socket(baseline_tab_socket) =~ "Alignment &amp; Review"
    refute render_socket(baseline_tab_socket) =~ "I am profiling the current Iteration candidate."

    assert {:noreply, refreshed_baseline_tab_socket} =
             PikaWeb.OptimizationLive.handle_info(:refresh, baseline_tab_socket)

    assert refreshed_baseline_tab_socket.assigns.workspace_tab == "baseline"

    GenServer.stop(bootstrap)
    Auth.clear()
  end

  defp render_socket(socket) do
    socket.assigns
    |> PikaWeb.OptimizationLive.render()
    |> Phoenix.HTML.Safe.to_iodata()
    |> IO.iodata_to_binary()
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
