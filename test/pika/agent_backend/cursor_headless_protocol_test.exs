defmodule Pika.AgentBackend.CursorHeadlessProtocolTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend

  test "streams repaired text and tool calls through one resumed Cursor chat" do
    workspace = temp_dir("cursor-headless-workspace")
    artifact_dir = temp_dir("cursor-headless-artifacts")
    argv_log = Path.join(artifact_dir, "argv.jsonl")
    skill_dir = fake_skill()
    profile = profile(artifact_dir, argv_log)

    {:ok, backend} = AgentBackend.start_link(Pika.AgentBackend.CursorHeadless, profile, self())

    assert {:ok, session} =
             AgentBackend.open_session(
               backend,
               workspace,
               "gpt-test[context=1m,effort=medium]",
               :high,
               %{url: "http://127.0.0.1:1/mcp", token: "headless-unit-secret"},
               [skill_dir],
               "Pika headless system instructions"
             )

    assert_event(:session_started)

    assert %{
             process_lifecycle: :per_turn,
             reasoning_stream: false,
             automatic_retry: false
           } = AgentBackend.capabilities(backend)

    assert {:ok, first_turn} = AgentBackend.start_turn(backend, "repair tools")
    assert %{turn_id: ^first_turn} = assert_event(:turn_started)
    assert %{data: %{delta: "Hello"}} = assert_event(:message_delta)

    assert %{data: %{"item" => %{"id" => "fake-command", "type" => "commandExecution"}}} =
             assert_event(:tool_started)

    assert_event(:tool_completed)
    assert %{data: %{"output" => "hello\n"}} = assert_event(:command_output)

    assert %{data: %{delta: " world", delivery: "terminal_repair"}} =
             assert_event(:message_delta)

    assert %{turn_id: ^first_turn, data: %{"status" => "completed"}} =
             assert_event(:turn_completed)

    assert Pika.AgentBackend.CursorHeadless.process_os_pid(backend.pid) == nil

    assert {:ok, second_turn} = AgentBackend.start_turn(backend, "second")
    assert %{turn_id: ^second_turn} = assert_event(:turn_started)
    assert_event(:turn_completed)

    invocations = read_invocations(argv_log)
    turn_invocations = Enum.filter(invocations, &(&1 |> Enum.member?("-p")))
    assert length(turn_invocations) == 2

    Enum.each(turn_invocations, fn args ->
      assert option(args, "--resume") == session.backend_session_id
      assert option(args, "--model") == "gpt-test[context=1m,effort=high]"
      assert "--approve-mcps" in args
      assert "--stream-partial-output" in args
    end)

    plugin_dir = option(hd(turn_invocations), "--plugin-dir")

    assert File.read!(Path.join([plugin_dir, "rules", "pika-system.mdc"])) =~
             "Pika headless system instructions"

    assert File.read!(Path.join(plugin_dir, "mcp.json")) =~ "headless-unit-secret"
    assert [_skill] = Path.wildcard(Path.join([plugin_dir, "skills", "*", "SKILL.md"]))

    contents = File.read!(session.jsonl_path)
    refute contents =~ "headless-unit-secret"

    assert :ok = AgentBackend.close_session(backend)
    assert_event(:process_exited)
    refute File.exists?(plugin_dir)
  end

  test "terminal result replaces divergent streamed text instead of concatenating it" do
    artifact_dir = temp_dir("cursor-headless-diverge")
    {:ok, backend} = start_backend(artifact_dir)
    open_backend(backend, artifact_dir)

    assert {:ok, turn_id} = AgentBackend.start_turn(backend, "diverge")
    assert_event(:turn_started)
    assert %{data: %{delta: "Hello"}} = assert_event(:message_delta)

    assert %{data: %{item: %{"text" => "Correct final", "delivery" => "terminal_repair"}}} =
             assert_event(:message_completed)

    assert %{turn_id: ^turn_id} = assert_event(:turn_completed)
    assert :ok = AgentBackend.close_session(backend)
  end

  test "interrupt and steer terminate only the active turn process" do
    artifact_dir = temp_dir("cursor-headless-controls")
    {:ok, backend} = start_backend(artifact_dir)
    open_backend(backend, artifact_dir)

    assert {:ok, held_turn} = AgentBackend.start_turn(backend, "hold")
    assert_event(:turn_started)
    assert wait_for_os_pid(backend.pid)
    assert :ok = AgentBackend.interrupt(backend)

    assert %{turn_id: ^held_turn, data: %{status: "interrupted"}} =
             assert_event(:turn_completed)

    assert {:ok, steered_from} = AgentBackend.start_turn(backend, "hold again")
    assert_event(:turn_started)
    assert wait_for_os_pid(backend.pid)
    assert {:ok, steered_turn} = AgentBackend.steer(backend, "complete after steer")

    assert %{turn_id: ^steered_from, data: %{status: "interrupted", reason: "steered"}} =
             assert_event(:turn_completed)

    assert %{turn_id: ^steered_turn} = assert_event(:turn_started)
    assert %{turn_id: ^steered_turn} = assert_event(:turn_completed)
    assert :ok = AgentBackend.close_session(backend)
  end

  test "a failed stream is not automatically submitted again" do
    artifact_dir = temp_dir("cursor-headless-failure")
    argv_log = Path.join(artifact_dir, "argv.jsonl")
    {:ok, backend} = start_backend(artifact_dir, argv_log)
    open_backend(backend, artifact_dir)

    assert {:ok, turn_id} = AgentBackend.start_turn(backend, "failure")
    assert_event(:turn_started)
    assert_event(:message_delta)

    assert %{turn_id: ^turn_id, data: %{code: :headless_turn_failed, retry: false}} =
             assert_event(:backend_error)

    assert %{turn_id: ^turn_id, data: %{expected: false, retry: false}} =
             assert_event(:process_exited)

    assert read_invocations(argv_log) |> Enum.count(&Enum.member?(&1, "-p")) == 1
    assert :ok = AgentBackend.close_session(backend)
  end

  test "reports an actionable authentication failure before creating a chat" do
    artifact_dir = temp_dir("cursor-headless-auth")
    profile = profile(artifact_dir, Path.join(artifact_dir, "argv.jsonl"))
    profile = %{profile | env: Map.put(profile.env, "PIKA_FAKE_CURSOR_AUTH", "failed")}
    {:ok, backend} = AgentBackend.start_link(Pika.AgentBackend.CursorHeadless, profile, self())

    assert {:error, %{code: :not_authenticated, message: message}} =
             AgentBackend.open_session(
               backend,
               artifact_dir,
               nil,
               nil,
               %{url: "http://127.0.0.1:1/mcp", token: "secret"},
               [],
               "instructions"
             )

    assert message =~ "cursor-agent login"
    assert :ok = AgentBackend.close_session(backend)
  end

  defp start_backend(artifact_dir, argv_log \\ nil) do
    argv_log = argv_log || Path.join(artifact_dir, "argv.jsonl")

    AgentBackend.start_link(
      Pika.AgentBackend.CursorHeadless,
      profile(artifact_dir, argv_log),
      self()
    )
  end

  defp open_backend(backend, workspace) do
    assert {:ok, _session} =
             AgentBackend.open_session(
               backend,
               workspace,
               "gpt-test",
               :low,
               %{url: "http://127.0.0.1:1/mcp", token: "secret"},
               [],
               "instructions"
             )

    assert_event(:session_started)
  end

  defp profile(artifact_dir, argv_log) do
    code_paths =
      Path.wildcard(Path.expand("../../../_build/test/lib/*/ebin", __DIR__))
      |> Enum.flat_map(&["-pa", &1])

    %{
      backend: :cursor_headless,
      command: System.find_executable("elixir"),
      args: code_paths ++ [fake_provider()],
      env: %{"PIKA_FAKE_CURSOR_ARGV_LOG" => argv_log},
      artifact_dir: artifact_dir
    }
  end

  defp assert_event(type) do
    receive do
      {:pika_backend_event, %{type: ^type} = event} -> event
      {:pika_backend_event, _other} -> assert_event(type)
    after
      5_000 -> flunk("timed out waiting for #{type}")
    end
  end

  defp wait_for_os_pid(backend_pid, attempts \\ 50)
  defp wait_for_os_pid(_backend_pid, 0), do: false

  defp wait_for_os_pid(backend_pid, attempts) do
    case Pika.AgentBackend.CursorHeadless.process_os_pid(backend_pid) do
      pid when is_integer(pid) -> pid
      _ -> Process.sleep(20) && wait_for_os_pid(backend_pid, attempts - 1)
    end
  end

  defp read_invocations(path) do
    path
    |> File.stream!()
    |> Enum.map(&Jason.decode!/1)
  end

  defp option(args, name) do
    index = Enum.find_index(args, &(&1 == name))
    if index, do: Enum.at(args, index + 1)
  end

  defp fake_provider, do: Path.expand("../../support/fake_cursor_headless.exs", __DIR__)

  defp fake_skill do
    dir = temp_dir("fake-skill")
    File.write!(Path.join(dir, "SKILL.md"), "---\nname: fake-skill\ndescription: test\n---\n")
    dir
  end

  defp temp_dir(prefix) do
    dir = Path.join(System.tmp_dir!(), "#{prefix}-#{System.unique_integer([:positive])}")
    File.mkdir_p!(dir)
    dir
  end
end
