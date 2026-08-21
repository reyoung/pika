defmodule Pika.PreviewAuthCLITest do
  use ExUnit.Case, async: false

  alias Pika.CLI
  alias Pika.PreviewAuth, as: Auth
  alias Pika.Test.CampaignFixtures

  setup do
    Auth.clear()
    on_exit(&Auth.clear/0)
    :ok
  end

  test "stores only a token hash marker and invalidates the old startup token" do
    first = Auth.generate()
    refute first.token == first.marker
    assert {:ok, first.marker} == Auth.authenticate(first.token)
    assert Auth.authenticated_marker?(first.marker)

    second = Auth.generate()
    assert {:error, :unauthorized} = Auth.authenticate(first.token)
    assert {:ok, second.marker} == Auth.authenticate(second.token)
  end

  test "parses the Preview public CLI options with safe defaults" do
    assert {:ok, opts} =
             CLI.parse_preview([
               "--backend",
               "cursor",
               "--effort",
               "xhigh",
               "--port",
               "8080",
               "--skill-root",
               "/tmp/a",
               "--skill-root",
               "/tmp/b"
             ])

    assert opts[:backend] == "cursor"
    assert opts[:effort] == "xhigh"
    assert opts[:port] == 8080
    assert Keyword.get_values(opts, :skill_root) == ["/tmp/a", "/tmp/b"]
    assert {:error, _} = CLI.parse_preview(["--backend", "unknown"])
    assert {:error, _} = CLI.parse_preview(["--effort", "infinite"])
  end

  test "parses Init options and rejects conflicting repository modes" do
    assert {:ok, opts} =
             CLI.parse_init([
               "/tmp/pika-workspace",
               "--repo",
               "/tmp/repo",
               "--alignment-backend",
               "cursor",
               "--alignment-model",
               "cursor-test",
               "--alignment-effort",
               "xhigh",
               "--alignment-approval-policy",
               "auto_review",
               "--alignment-sandbox-policy",
               "enabled",
               "--iteration-backend",
               "codex",
               "--iteration-model",
               "codex-test",
               "--iteration-effort",
               "max",
               "--iteration-approval-policy",
               "untrusted",
               "--iteration-sandbox-policy",
               "workspace_write",
               "--integration-backend",
               "cursor",
               "--integration-model",
               "integration-test",
               "--integration-effort",
               "high",
               "--integration-approval-policy",
               "auto_review",
               "--integration-sandbox-policy",
               "enabled",
               "--iteration-agents",
               "3",
               "--max-attempts",
               "12",
               "--no-sync",
               "--yes"
             ])

    assert opts[:workspace] == "/tmp/pika-workspace"
    assert opts[:alignment_backend] == "cursor"
    assert opts[:alignment_model] == "cursor-test"
    assert opts[:alignment_effort] == "xhigh"
    assert opts[:alignment_approval_policy] == "auto_review"
    assert opts[:alignment_sandbox_policy] == "enabled"
    assert opts[:iteration_backend] == "codex"
    assert opts[:iteration_model] == "codex-test"
    assert opts[:iteration_effort] == "max"
    assert opts[:iteration_approval_policy] == "untrusted"
    assert opts[:iteration_sandbox_policy] == "workspace_write"
    assert opts[:integration_backend] == "cursor"
    assert opts[:integration_model] == "integration-test"
    assert opts[:integration_effort] == "high"
    assert opts[:integration_approval_policy] == "auto_review"
    assert opts[:integration_sandbox_policy] == "enabled"
    assert opts[:iteration_agents] == 3
    assert opts[:max_attempts] == 12
    assert opts[:yes]

    assert {:error, message} = CLI.parse_init(["--owned", "--repo", "/tmp/repo"])
    assert message =~ "--owned and --repo cannot be combined"
    assert {:error, _message} = CLI.parse_init(["--backend", "unknown"])
    assert {:error, _message} = CLI.parse_init(["--alignment-backend", "unknown"])
    assert {:error, _message} = CLI.parse_init(["--iteration-backend", "unknown"])
    assert {:error, _message} = CLI.parse_init(["--alignment-effort", "infinite"])
    assert {:error, _message} = CLI.parse_init(["--iteration-effort", "infinite"])
    assert {:error, _message} = CLI.parse_init(["--integration-backend", "unknown"])
    assert {:error, _message} = CLI.parse_init(["--integration-effort", "infinite"])

    assert {:error, _message} =
             CLI.parse_init([
               "--alignment-backend",
               "cursor",
               "--alignment-approval-policy",
               "on_request"
             ])

    assert {:error, _message} =
             CLI.parse_init([
               "--iteration-backend",
               "codex",
               "--iteration-sandbox-policy",
               "enabled"
             ])

    assert {:ok, legacy} = CLI.parse_init(["--backend", "cursor", "--yes"])
    assert legacy[:backend] == "cursor"
    assert {:error, _message} = CLI.parse_init(["a", "b"])
  end

  test "discovers serve options from the current Workspace" do
    workspace = CampaignFixtures.workspace()
    config = Path.join(workspace, "pika.yaml")
    File.write!(config, "campaign:\n  history_n: 10\n")

    File.cd!(workspace, fn ->
      assert {:ok, opts} = CLI.parse_serve([])
      assert opts[:workspace] == workspace
      assert opts[:config] == config
    end)

    assert {:ok, explicit} = CLI.parse_serve(["--workspace", workspace])
    assert explicit[:workspace] == workspace
    assert explicit[:config] == config

    File.rm!(config)
    File.write!(Path.join(workspace, "config.json"), "{}")

    File.cd!(workspace, fn ->
      assert {:error, message} = CLI.parse_serve([])
      refute message =~ "--workspace is required"
      assert message =~ "--config is required"
    end)
  end
end
