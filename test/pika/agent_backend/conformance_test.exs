Code.require_file(Path.expand("../../support/fake_agent_backend.exs", __DIR__))

defmodule Pika.AgentBackend.ConformanceTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend

  test "domain caller uses one protocol-neutral API" do
    {:ok, backend} =
      AgentBackend.start_link(Pika.Test.FakeAgentBackend, %{backend: :fake}, self())

    assert {:ok, session} = AgentBackend.open_session(backend, File.cwd!(), "fake", :low, %{}, [])
    assert session.backend_protocol == "fake-v1"
    assert {:ok, turn_id} = AgentBackend.start_turn(backend, "hello")
    assert is_binary(turn_id)

    assert_receive {:pika_backend_event, %{type: :session_started}}
    assert_receive {:pika_backend_event, %{type: :turn_started}}
    assert_receive {:pika_backend_event, %{type: :message_delta}}
    assert_receive {:pika_backend_event, %{type: :turn_completed}}

    assert %{protocol: "fake-v1"} = AgentBackend.capabilities(backend)
    assert :ok = AgentBackend.close_session(backend)
  end
end
