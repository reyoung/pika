defmodule Pika.Agent.MCPRouterTest do
  use ExUnit.Case, async: false
  import Plug.Conn
  import Plug.Test

  alias Pika.Agent.{SessionBinding, Directory}
  alias Pika.Agent.MCP.Router, as: MCPRouter

  setup do
    directory = start_supervised!({Directory, name: nil})

    context_dir =
      Path.join(System.tmp_dir!(), "pika-v2-mcp-#{System.unique_integer([:positive])}")

    File.mkdir_p!(context_dir)
    context_file = Path.join(context_dir, "context.json")
    File.write!(context_file, "{}")

    binding = %SessionBinding{
      actor: self(),
      role_id: "iteration",
      work_kind: :attempt,
      work_id: "42",
      session_id: Ecto.UUID.generate(),
      context_file: context_file,
      work_root: context_dir
    }

    assert {:ok, token} = Directory.issue(binding, directory)
    on_exit(fn -> File.rm_rf!(context_dir) end)
    %{directory: directory, token: token}
  end

  test "lists only the bound Role catalog", %{directory: directory, token: token} do
    response = rpc(directory, token, %{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"})
    assert response.status == 200
    names = response.body_params["result"]["tools"] |> Enum.map(& &1["name"])
    assert names == ~w(get_context query_attempt_history finish_iteration)
  end

  test "invokes the bound Actor and returns structured MCP content", %{
    directory: directory,
    token: token
  } do
    parent = self()

    invoker = fn actor, name, arguments ->
      send(parent, {:invoked, actor, name, arguments})
      {:ok, %{status: "ready_for_integration"}}
    end

    response =
      rpc(
        directory,
        token,
        %{
          "jsonrpc" => "2.0",
          "id" => 2,
          "method" => "tools/call",
          "params" => %{
            "name" => "finish_iteration",
            "arguments" => %{
              "result_path" => "iteration-result.json",
              "idempotency_key" => "finish-1"
            }
          }
        },
        invoker
      )

    assert response.status == 200

    assert response.body_params["result"]["structuredContent"]["status"] ==
             "ready_for_integration"

    assert_received {:invoked, actor, "finish_iteration", %{"idempotency_key" => "finish-1"}}
    assert actor == self()
  end

  test "rejects unknown tokens", %{directory: directory} do
    response = rpc(directory, "wrong", %{"jsonrpc" => "2.0", "id" => 1, "method" => "ping"})
    assert response.status == 401
    assert response.body_params["error"]["code"] == -32_001
  end

  defp rpc(directory, token, body, invoker \\ fn _actor, _name, _arguments -> {:ok, %{}} end) do
    conn(:post, "/mcp", Jason.encode!(body))
    |> put_req_header("content-type", "application/json")
    |> put_req_header("authorization", "Bearer #{token}")
    |> MCPRouter.call(directory: directory, invoker: invoker)
    |> then(fn conn -> %{status: conn.status, body_params: Jason.decode!(conn.resp_body)} end)
  end
end
