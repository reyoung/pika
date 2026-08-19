defmodule Pika.Alignment.MCP.RouterTest do
  use ExUnit.Case, async: false

  import Plug.Conn
  import Plug.Test

  alias Pika.Alignment.{Campaign, Workspace}
  alias Pika.Test.AlignmentFixtures

  @token "alignment-http-mcp-token"

  setup do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    repo = AlignmentFixtures.git_repo()
    {:ok, workspace} = Workspace.prepare(repo)
    skill_dir = Path.join(workspace.root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)

    File.write!(
      Path.join(skill_dir, "SKILL.md"),
      "---\nname: ncu-report-skill\ndescription: test\n---\n"
    )

    {:ok, pid} =
      Campaign.start_link(
        workspace: workspace,
        start_backend: false,
        resolve_references: false,
        mcp_url: "http://127.0.0.1:1/mcp",
        mcp_token: @token,
        skill: %{
          name: "ncu-report-skill",
          url: "test",
          sha: String.duplicate("e", 40),
          path: skill_dir
        }
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)
    :ok
  end

  test "authenticates initialize and exposes the Boundary tool contract" do
    assert %{"result" => %{"serverInfo" => %{"name" => "pika-alignment"}}} =
             rpc(@token, %{
               "jsonrpc" => "2.0",
               "id" => 1,
               "method" => "initialize",
               "params" => %{}
             })

    assert %{"result" => %{"tools" => tools}} =
             rpc(@token, %{"jsonrpc" => "2.0", "id" => 2, "method" => "tools/list"})

    assert Enum.map(tools, & &1["name"]) == [
             "get_context",
             "register_artifact",
             "submit_spec",
             "submit_harness",
             "complete_setup_merge",
             "submit_baseline",
             "submit_iteration_sample"
           ]

    assert %{"result" => %{"structuredContent" => %{"required_operations" => required}}} =
             rpc(@token, %{
               "jsonrpc" => "2.0",
               "id" => 3,
               "method" => "tools/call",
               "params" => %{"name" => "get_context", "arguments" => %{}}
             })

    assert required == ["submit_harness", "submit_spec"]
  end

  test "rejects a missing bearer token" do
    conn =
      :post
      |> conn("/", Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "tools/list"}))
      |> put_req_header("content-type", "application/json")
      |> Pika.Alignment.MCP.Router.call(Pika.Alignment.MCP.Router.init([]))

    assert conn.status == 401
  end

  defp rpc(token, request) do
    conn =
      :post
      |> conn("/", Jason.encode!(request))
      |> put_req_header("content-type", "application/json")
      |> put_req_header("authorization", "Bearer #{token}")
      |> Pika.Alignment.MCP.Router.call(Pika.Alignment.MCP.Router.init([]))

    assert conn.status == 200
    Jason.decode!(conn.resp_body)
  end
end
