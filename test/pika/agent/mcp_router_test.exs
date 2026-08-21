defmodule Pika.Agent.MCP.RouterTest do
  use ExUnit.Case, async: true
  import Plug.Conn
  import Plug.Test

  alias Pika.Agent.Directory
  alias Pika.Agent.Role.{Outcome, Progress, Tool, Work}
  alias Pika.Agent.MCP.Router

  defmodule ActorStub do
    use GenServer

    def start_link(test_pid), do: GenServer.start_link(__MODULE__, test_pid)
    def init(test_pid), do: {:ok, test_pid}

    def handle_call(:catalog, _from, test_pid) do
      {:reply,
       {:ok,
        [
          %Tool{
            name: "observe",
            description: "Observe committed state.",
            kind: :query,
            input_schema: %{"type" => "object", "additionalProperties" => false}
          }
        ]}, test_pid}
    end

    def handle_call({:invoke, "observe", arguments}, _from, test_pid) do
      send(test_pid, {:actor_stub_invoked, arguments})

      {:reply,
       {:ok,
        %Outcome{
          value: %{"observed" => true},
          progress: %Progress{state: :open, required_operations: [], facts_revision: 3},
          actor_directive: :keep_running
        }}, test_pid}
    end
  end

  setup do
    directory = start_supervised!({Directory, name: nil})
    actor = start_supervised!({ActorStub, self()})

    work = %Work{
      role_id: "iteration",
      kind: :stub,
      id: "work-1",
      campaign_id: "campaign-1"
    }

    catalog = [
      %Tool{
        name: "observe",
        description: "Observe committed state.",
        kind: :query,
        input_schema: %{"type" => "object", "additionalProperties" => false}
      }
    ]

    assert {:ok, token} = Directory.issue(actor, work, "iteration", catalog, directory)
    %{directory: directory, token: token}
  end

  test "lists the Actor-frozen catalog even when the registered Role has the same id", context do
    response = rpc(context, %{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"})
    assert response.status == 200
    assert %{"result" => %{"tools" => [tool]}} = Jason.decode!(response.resp_body)
    assert tool["name"] == "observe"
    assert tool["inputSchema"]["additionalProperties"] == false

    response =
      rpc(context, %{
        "jsonrpc" => "2.0",
        "id" => 2,
        "method" => "tools/call",
        "params" => %{"name" => "observe", "arguments" => %{"scope" => "committed"}}
      })

    assert response.status == 200
    assert_receive {:actor_stub_invoked, %{"scope" => "committed"}}

    assert %{
             "result" => %{
               "structuredContent" => %{"observed" => true},
               "isError" => false
             }
           } = Jason.decode!(response.resp_body)
  end

  test "rejects an unknown bearer token before dispatch", context do
    response =
      context
      |> Map.put(:token, "unknown")
      |> rpc(%{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"})

    assert response.status == 401
  end

  defp rpc(context, request) do
    conn(:post, "/", Jason.encode!(request))
    |> put_req_header("content-type", "application/json")
    |> put_req_header("authorization", "Bearer #{context.token}")
    |> Router.call(Router.init(directory: context.directory))
  end
end
