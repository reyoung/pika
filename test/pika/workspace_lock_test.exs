defmodule Pika.WorkspaceLockTest do
  use ExUnit.Case, async: true

  test "allows exactly one live owner and removes its own lock on shutdown" do
    workspace = Path.join(System.tmp_dir!(), "pika-v2-lock-#{System.unique_integer([:positive])}")
    File.mkdir_p!(workspace)
    on_exit(fn -> File.rm_rf!(workspace) end)

    assert {:ok, first} = Pika.WorkspaceLock.start_link(workspace: workspace, name: nil)
    metadata = Pika.WorkspaceLock.snapshot(first)
    assert metadata["os_pid"] == System.pid()
    assert File.regular?(Path.join(workspace, ".pika.lock"))

    Process.flag(:trap_exit, true)

    assert {:error, {:workspace_already_locked, owner}} =
             Pika.WorkspaceLock.start_link(workspace: workspace, name: nil)

    assert owner["token"] == metadata["token"]
    GenServer.stop(first)
    refute File.exists?(Path.join(workspace, ".pika.lock"))

    assert {:ok, second} = Pika.WorkspaceLock.start_link(workspace: workspace, name: nil)
    GenServer.stop(second)
  end
end
