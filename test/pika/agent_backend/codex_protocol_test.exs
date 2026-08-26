defmodule Pika.AgentBackend.CodexProtocolTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend
  alias Pika.AgentBackend.JSONLWriter

  test "maps Codex thread/turn, native steer, interrupt, skills and close" do
    artifact_dir = temp_dir("codex-artifacts")
    skill_dir = fake_skill()

    profile = %{
      backend: :codex_app_server,
      command: System.find_executable("mix"),
      args: ["run", "--no-compile", "--no-start", fake_provider(), "--"],
      env: %{"PIKA_FAKE_PROTOCOL" => "codex"},
      approval_policy: "on_request",
      sandbox_policy: "read_only",
      artifact_dir: artifact_dir
    }

    {:ok, backend} =
      AgentBackend.start_link(Pika.AgentBackend.CodexAppServer, profile, self())

    assert {:ok, session} =
             AgentBackend.open_session(
               backend,
               File.cwd!(),
               "fake-model",
               :low,
               %{url: "http://127.0.0.1:1/mcp", token: "unit-test-secret"},
               [skill_dir],
               "Pika system instructions"
             )

    assert session.backend_session_id == "fake-thread"
    assert Path.basename(session.jsonl_path) =~ ".wire.jsonl"
    assert Path.basename(Path.dirname(session.jsonl_path)) == "transport"
    assert_receive {:pika_backend_event, %{type: :session_started}}

    os_pid = Pika.AgentBackend.CodexAppServer.process_os_pid(backend.pid)
    assert {:ok, cmdline} = File.read("/proc/#{os_pid}/cmdline")
    assert cmdline =~ "mcp_servers.pika.default_tools_approval_mode=\"approve\""
    assert cmdline =~ "mcp_servers.pika.tool_timeout_sec=610"
    refute cmdline =~ "mcp_servers.pika.default_tools_approval_mode=\"auto\""

    refute Enum.any?(
             JSONLWriter.replay(session.jsonl_path),
             &(get_in(&1, ["payload", "method"]) == "turn/start")
           )

    assert {:ok, first_turn} = AgentBackend.start_turn(backend, "complete")
    assert is_binary(first_turn)
    assert_event(:turn_started)
    assert_event(:message_started)
    assert_event(:message_delta)

    assert %{data: %{item: %{"text" => "fake complete"}}} =
             assert_event(:message_completed)

    assert_event(:turn_completed)

    assert {:ok, held_turn} = AgentBackend.start_turn(backend, "hold")
    assert_event(:turn_started)
    assert {:ok, ^held_turn} = AgentBackend.steer(backend, "complete after steer")
    assert_event(:message_started)
    assert_event(:message_delta)
    assert_event(:message_completed)
    assert_event(:turn_completed)

    assert {:ok, interrupted_turn} = AgentBackend.start_turn(backend, "hold for interrupt")
    assert_event(:turn_started)
    assert :ok = AgentBackend.interrupt(backend)

    assert %{data: %{"status" => "interrupted"}, turn_id: ^interrupted_turn} =
             assert_event(:turn_completed)

    assert %{native_steer: true, protocol: "codex-app-server-v2"} =
             AgentBackend.capabilities(backend)

    assert :ok = AgentBackend.close_session(backend)
    assert %{data: %{expected: true}} = assert_event(:process_exited)

    records = JSONLWriter.replay(session.jsonl_path)

    methods =
      for %{"direction" => "out", "payload" => %{"method" => method}} <- records, do: method

    assert "initialize" in methods
    assert "skills/extraRoots/set" in methods
    assert "skills/list" in methods
    assert "thread/start" in methods
    assert "turn/start" in methods
    assert "turn/steer" in methods
    assert "turn/interrupt" in methods

    thread_start =
      Enum.find(records, &(get_in(&1, ["payload", "method"]) == "thread/start"))

    assert get_in(thread_start, ["payload", "params", "developerInstructions"]) ==
             "Pika system instructions"

    assert get_in(thread_start, ["payload", "params", "ephemeral"]) == false
    assert get_in(thread_start, ["payload", "params", "approvalPolicy"]) == "on-request"
    assert get_in(thread_start, ["payload", "params", "approvalsReviewer"]) == "auto_review"
    assert get_in(thread_start, ["payload", "params", "sandbox"]) == "read-only"

    first_turn = Enum.find(records, &(get_in(&1, ["payload", "method"]) == "turn/start"))

    assert get_in(first_turn, ["payload", "params", "input"]) == [
             %{"type" => "text", "text" => "complete"}
           ]

    assert get_in(first_turn, ["payload", "params", "approvalPolicy"]) == "on-request"
    assert get_in(first_turn, ["payload", "params", "approvalsReviewer"]) == "auto_review"
    assert get_in(first_turn, ["payload", "params", "sandboxPolicy"]) == %{"type" => "readOnly"}
  end

  test "resumes a persisted Codex thread instead of starting a new one" do
    profile = %{
      backend: :codex_app_server,
      command: System.find_executable("mix"),
      args: ["run", "--no-compile", "--no-start", fake_provider(), "--"],
      env: %{"PIKA_FAKE_PROTOCOL" => "codex"},
      artifact_dir: temp_dir("codex-resume")
    }

    {:ok, backend} =
      AgentBackend.start_link(Pika.AgentBackend.CodexAppServer, profile, self())

    assert {:ok, session} =
             AgentBackend.open_session(
               backend,
               File.cwd!(),
               "fake-model",
               :low,
               %{
                 url: "http://127.0.0.1:1/mcp",
                 token: "resume-secret",
                 resume_session_id: "persisted-codex-thread"
               },
               [],
               "Pika resumed instructions"
             )

    assert session.backend_session_id == "persisted-codex-thread"
    assert session.resumed
    assert session.resume_error == nil

    methods =
      for %{"direction" => "out", "payload" => %{"method" => method}} <-
            JSONLWriter.replay(session.jsonl_path),
          do: method

    assert "thread/resume" in methods
    refute "thread/start" in methods
    assert :ok = AgentBackend.close_session(backend)
  end

  test "classifies terminal failures and waits through retriable error diagnostics" do
    profile = %{
      backend: :codex_app_server,
      command: System.find_executable("mix"),
      args: ["run", "--no-compile", "--no-start", fake_provider(), "--"],
      env: %{"PIKA_FAKE_PROTOCOL" => "codex"},
      artifact_dir: temp_dir("codex-failures")
    }

    {:ok, backend} =
      AgentBackend.start_link(Pika.AgentBackend.CodexAppServer, profile, self())

    assert {:ok, _session} =
             AgentBackend.open_session(
               backend,
               File.cwd!(),
               "fake-model",
               :low,
               %{url: "http://127.0.0.1:1/mcp", token: "failure-secret"},
               [],
               "Pika failure instructions"
             )

    assert_event(:session_started)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "usage failure")
    assert_event(:turn_started)

    assert %{data: %{failure: %{category: "capacity_exhausted", retry_at: retry_at}}} =
             assert_event(:backend_error)

    assert is_integer(retry_at)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "unauthorized failure")
    assert_event(:turn_started)

    assert %{data: %{failure: %{category: "authentication_failed"}}} =
             assert_event(:backend_error)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "context failure")
    assert_event(:turn_started)

    assert %{data: %{failure: %{category: "context_exhausted"}}} =
             assert_event(:backend_error)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "session budget failure")
    assert_event(:turn_started)

    assert %{data: %{failure: %{category: "session_budget_exhausted"}}} =
             assert_event(:backend_error)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "network failure")
    assert_event(:turn_started)

    assert %{data: %{failure: %{category: "transient"}}} =
             assert_event(:backend_error)

    assert {:ok, successful_turn} =
             AgentBackend.start_turn(backend, "rate notification success")

    assert_event(:turn_started)
    assert %{turn_id: ^successful_turn} = assert_event(:turn_completed)
    refute_receive {:pika_backend_event, %{type: :backend_error}}, 100

    assert {:ok, retry_turn} = AgentBackend.start_turn(backend, "retry then success")
    assert_event(:turn_started)
    assert %{turn_id: ^retry_turn} = assert_event(:turn_completed)
    refute_receive {:pika_backend_event, %{type: :backend_error}}, 100

    assert :ok = AgentBackend.close_session(backend)
  end

  defp assert_event(type) do
    receive do
      {:pika_backend_event, %{type: ^type} = event} -> event
      {:pika_backend_event, _other} -> assert_event(type)
    after
      5_000 -> flunk("timed out waiting for #{type}")
    end
  end

  defp fake_provider, do: Path.expand("../../support/fake_jsonl_provider.exs", __DIR__)

  defp fake_skill do
    dir = temp_dir("codex-skill")
    File.write!(Path.join(dir, "SKILL.md"), "---\nname: fake-skill\ndescription: test\n---\n")
    dir
  end

  defp temp_dir(prefix) do
    dir = Path.join(System.tmp_dir!(), "#{prefix}-#{System.unique_integer([:positive])}")
    File.mkdir_p!(dir)
    dir
  end
end
