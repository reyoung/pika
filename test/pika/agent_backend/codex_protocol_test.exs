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
               [skill_dir]
             )

    assert session.backend_session_id == "fake-thread"
    assert_receive {:pika_backend_event, %{type: :session_started}}

    assert {:ok, first_turn} = AgentBackend.start_turn(backend, "complete")
    assert is_binary(first_turn)
    assert_event(:turn_started)
    assert_event(:message_delta)
    assert_event(:turn_completed)

    assert {:ok, held_turn} = AgentBackend.start_turn(backend, "hold")
    assert_event(:turn_started)
    assert {:ok, ^held_turn} = AgentBackend.steer(backend, "complete after steer")
    assert_event(:message_delta)
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
