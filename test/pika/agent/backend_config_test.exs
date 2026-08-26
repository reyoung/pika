defmodule Pika.Agent.BackendConfigTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.BackendConfig
  alias Pika.Optimization.Config.Agent

  test "selects Cursor Headless and its local cursor-agent command" do
    agent = %Agent{
      backend: :cursor_headless,
      approval_policy: "force",
      sandbox: "disabled"
    }

    assert {:ok, Pika.AgentBackend.CursorHeadless} = BackendConfig.module(agent)

    assert %{
             backend: :cursor_headless,
             command: "cursor-agent",
             args: [],
             approval_policy: "force",
             sandbox_policy: "disabled"
           } = BackendConfig.launch_config(agent, "/tmp/pika-headless-artifacts")
  end
end
