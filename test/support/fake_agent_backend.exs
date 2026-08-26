defmodule Pika.Test.FakeAgentBackend do
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Error, Event, Failure, Id, Session}

  def start_link(_profile, sink),
    do: Agent.start_link(fn -> %{sink: sink, session: nil, turn: nil} end)

  def open_session(server, cwd, model, effort, _mcp, _skill_roots, _instructions) do
    if model == "authentication-failure" do
      {:error,
       %Error{
         code: :authentication_failed,
         message: "injected authentication failure",
         failure:
           Failure.new(:authentication_failed, "injected authentication failure",
             code: "Unauthorized"
           )
       }}
    else
      session = %Session{
        id: Id.new("session"),
        backend: :fake,
        backend_protocol: "fake-v1",
        backend_session_id: Id.new("backend"),
        cwd: cwd,
        model: model,
        reasoning_effort: effort,
        jsonl_path: "/dev/null"
      }

      Agent.update(server, &%{&1 | session: session})
      emit(server, :session_started)
      {:ok, session}
    end
  end

  def start_turn(server, input) do
    turn_id = Id.new("turn")
    Agent.update(server, &%{&1 | turn: turn_id})
    emit(server, :turn_started)

    case input do
      "emit distinct message items" ->
        command = %{
          "id" => "command-1",
          "type" => "commandExecution",
          "command" => "sed -n '1,20p' lib/pika.ex",
          "status" => "inProgress"
        }

        emit(server, :tool_started, %{item: command, stage: :started})

        emit(server, :message_delta, %{item_id: "message-1", delta: "incomplete first"})

        emit(server, :message_completed, %{
          item: %{
            "id" => "message-1",
            "type" => "agentMessage",
            "phase" => "commentary",
            "text" => "Complete first update."
          }
        })

        emit(server, :message_delta, %{item_id: "message-2", delta: "incomplete final"})

        emit(server, :message_completed, %{
          item: %{
            "id" => "message-2",
            "type" => "agentMessage",
            "phase" => "final_answer",
            "text" => "Complete final answer."
          }
        })

        emit(server, :tool_completed, %{
          item: %{command | "status" => "completed"},
          stage: :completed
        })

      "emit reasoning around tools" ->
        emit(server, :message_delta, %{
          item_id: "reasoning-1",
          phase: "reasoning",
          delta: "Inspect the repository."
        })

        tool = %{
          "id" => "read-1",
          "type" => "commandExecution",
          "command" => "sed -n '1,20p' lib/pika.ex",
          "status" => "inProgress"
        }

        emit(server, :tool_started, %{item: tool, stage: :started})
        emit(server, :tool_completed, %{item: %{tool | "status" => "completed"}})

        emit(server, :message_delta, %{
          item_id: "reasoning-2",
          phase: "reasoning",
          delta: "Measure the candidate."
        })

        emit(server, :message_delta, %{
          item_id: "commentary-3",
          phase: "commentary",
          delta: "Candidate is ready."
        })

      "hold turn open" ->
        :ok

      _input ->
        emit(server, :message_delta, %{input: input})
    end

    if input != "hold turn open" do
      emit(server, :turn_completed, %{status: :completed})
      Agent.update(server, &%{&1 | turn: nil})
    end

    {:ok, turn_id}
  end

  def steer(server, "replace active turn") do
    case Agent.get(server, & &1.turn) do
      nil ->
        {:error, %Error{code: :steer_failed, message: "no active turn"}}

      _old_turn_id ->
        emit(server, :turn_completed, %{status: :cancelled})
        new_turn_id = Id.new("turn")
        Agent.update(server, &%{&1 | turn: new_turn_id})
        {:ok, new_turn_id}
    end
  end

  def steer(_server, "fail active turn") do
    {:error, %Error{code: :steer_failed, message: "injected steer failure"}}
  end

  def steer(server, _input) do
    case Agent.get(server, & &1.turn) do
      nil -> {:error, %Error{code: :steer_failed, message: "no active turn"}}
      turn_id -> {:ok, turn_id}
    end
  end

  def interrupt(_server), do: :ok
  def close_session(server), do: Agent.stop(server)
  def capabilities(_server), do: %{protocol: "fake-v1", native_steer: true}

  defp emit(server, type, data \\ %{}) do
    state = Agent.get(server, & &1)
    event = Event.new(type, :fake, state.session.id, %{turn_id: state.turn, data: data})

    case state.sink do
      pid when is_pid(pid) -> send(pid, {:pika_backend_event, event})
      fun when is_function(fun, 1) -> fun.(event)
    end
  end
end
