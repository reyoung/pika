defmodule Pika.AgentBackend.JSONLPortTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend.JSONLPort

  test "reassembles fragmented stdout lines and keeps stderr separate" do
    root = Path.join(System.tmp_dir!(), "pika-port-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    jsonl_path = Path.join(root, "wire.jsonl")
    stderr_path = Path.join(root, "provider.stderr.log")

    script =
      ~S|printf '%s' '{"id":1'; sleep 0.05; printf '%s\n' ',"result":{"ok":true}}'; printf '%s\n' 'separate-stderr' >&2|

    assert {:ok, transport} =
             JSONLPort.start(
               owner: self(),
               command: System.find_executable("sh"),
               args: ["-c", script],
               env: %{},
               stderr_path: stderr_path,
               jsonl_path: jsonl_path
             )

    assert_receive {:backend_wire, ^transport, %{"id" => 1, "result" => %{"ok" => true}}}, 2_000
    assert_receive {:backend_process_exited, ^transport, 0, false}, 2_000
    assert File.read!(stderr_path) =~ "separate-stderr"
    refute File.read!(jsonl_path) =~ "separate-stderr"
  end
end
