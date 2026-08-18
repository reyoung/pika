defmodule Pika.AgentBackend.CursorProtocolTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend
  alias Pika.AgentBackend.JSONLWriter

  test "maps ACP prompt/update, cancel-follow-up steer, interrupt and process close fallback" do
    artifact_dir = temp_dir("cursor-artifacts")
    skill_dir = fake_skill()

    profile = %{
      backend: :cursor_acp,
      command: System.find_executable("mix"),
      args: ["run", "--no-compile", "--no-start", fake_provider(), "--"],
      env: %{"PIKA_FAKE_PROTOCOL" => "cursor"},
      artifact_dir: artifact_dir
    }

    {:ok, backend} = AgentBackend.start_link(Pika.AgentBackend.CursorACP, profile, self())

    assert {:ok, session} =
             AgentBackend.open_session(
               backend,
               File.cwd!(),
               nil,
               :low,
               %{url: "http://127.0.0.1:1/mcp", token: "cursor-unit-secret"},
               [skill_dir]
             )

    assert_receive {:pika_backend_event, %{type: :session_started}}

    assert %{close: :process_fallback, simulated_steer: :cancel_then_prompt} =
             AgentBackend.capabilities(backend)

    assert {:ok, _turn} = AgentBackend.start_turn(backend, "complete")
    assert_event(:turn_started)
    assert_event(:message_delta)
    assert_event(:turn_completed)

    assert {:ok, cancelled_turn} = AgentBackend.start_turn(backend, "hold")
    assert_event(:turn_started)
    steer = Task.async(fn -> AgentBackend.steer(backend, "complete after steer") end)

    assert %{turn_id: ^cancelled_turn, data: %{"stopReason" => "cancelled"}} =
             assert_event(:turn_completed)

    assert {:ok, follow_up_turn} = Task.await(steer, 5_000)
    assert is_binary(follow_up_turn)
    assert_event(:turn_started)
    assert %{turn_id: ^follow_up_turn} = assert_event(:turn_completed)

    assert {:ok, interrupted_turn} = AgentBackend.start_turn(backend, "hold for interrupt")
    assert_event(:turn_started)
    assert :ok = AgentBackend.interrupt(backend)

    assert %{turn_id: ^interrupted_turn, data: %{"stopReason" => "cancelled"}} =
             assert_event(:turn_completed)

    assert :ok = AgentBackend.close_session(backend)
    assert %{data: %{wire_close_fallback: nil}} = assert_event(:process_exited)

    records = JSONLWriter.replay(session.jsonl_path)

    methods =
      for %{"direction" => "out", "payload" => %{"method" => method}} <- records, do: method

    assert "initialize" in methods
    assert "session/new" in methods
    assert "session/prompt" in methods
    assert "session/cancel" in methods
    refute "session/close" in methods

    contents = File.read!(session.jsonl_path)
    assert contents =~ "[REDACTED]"
    refute contents =~ "cursor-unit-secret"
  end

  test "uses ACP session/close only when the provider advertises it" do
    profile = %{
      backend: :cursor_acp,
      command: System.find_executable("mix"),
      args: ["run", "--no-compile", "--no-start", fake_provider(), "--"],
      env: %{"PIKA_FAKE_PROTOCOL" => "cursor", "PIKA_FAKE_CURSOR_CLOSE" => "1"},
      artifact_dir: temp_dir("cursor-close")
    }

    {:ok, backend} = AgentBackend.start_link(Pika.AgentBackend.CursorACP, profile, self())

    assert {:ok, session} =
             AgentBackend.open_session(
               backend,
               File.cwd!(),
               nil,
               nil,
               %{url: "http://127.0.0.1:1/mcp", token: "secret"},
               []
             )

    assert %{close: :protocol} = AgentBackend.capabilities(backend)
    assert :ok = AgentBackend.close_session(backend)

    methods =
      for %{"direction" => "out", "payload" => %{"method" => method}} <-
            JSONLWriter.replay(session.jsonl_path),
          do: method

    assert "session/close" in methods
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
    dir = temp_dir("cursor-skill")
    File.write!(Path.join(dir, "SKILL.md"), "---\nname: fake-skill\ndescription: test\n---\n")
    dir
  end

  defp temp_dir(prefix) do
    dir = Path.join(System.tmp_dir!(), "#{prefix}-#{System.unique_integer([:positive])}")
    File.mkdir_p!(dir)
    dir
  end
end
