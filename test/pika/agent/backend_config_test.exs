defmodule Pika.Agent.BackendConfigTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.{BackendConfig, Work}
  alias Pika.AgentBackend.LaunchConfig
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

  test "injects Attempt paths for Iteration and Integration independently of cwd" do
    launch_config = %LaunchConfig{
      backend: :codex_app_server,
      command: "codex",
      env: %{
        "KEEP_ME" => "yes",
        "PIKA_CANDIDATE_MANIFEST" => "/stale/manifest.json"
      },
      artifact_dir: "/tmp/artifacts"
    }

    paths = %{work_root: "/workspace/attempts/000007", cwd: "/workspace/best"}

    for role_id <- ["iteration", "integration"] do
      work = %Work{role_id: role_id, kind: :attempt, id: "7"}
      configured = BackendConfig.inject_attempt_runtime_env(launch_config, work, paths)

      assert configured.env["PIKA_ATTEMPT_ROOT"] == "/workspace/attempts/000007"

      assert configured.env["PIKA_CANDIDATE_MANIFEST"] ==
               "/workspace/attempts/000007/repo/candidate/manifest.json"

      assert configured.env["KEEP_ME"] == "yes"
    end
  end
end
