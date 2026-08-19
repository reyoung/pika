defmodule Pika.ConfigWorkspaceTest do
  use ExUnit.Case, async: false

  alias Pika.Test.Phase1Fixtures
  alias Pika.{Config, Workspace}

  test "validates YAML fields and applies documented defaults" do
    workspace = Phase1Fixtures.workspace()
    config_path = Phase1Fixtures.config_file("campaign:\n  history_n: 7\n")

    assert {:ok, config} = Config.load(config_path, workspace: workspace)
    assert config.host == "127.0.0.1"
    assert config.port == 8080
    assert config.backend["type"] == "codex_app_server"
    assert config.campaign["history_n"] == 7
  end

  test "reports all field validation failures without guessing values" do
    yaml = """
    server:
      port: nope
      surprise: true
    backend:
      type: mystery
      command: []
    campaign:
      plan: yes
      max_attempts: -2
      history_n: -1
      reference_catalog: nope
      stop_conditions:
        mode: fastest
      extra: value
    """

    assert {:error, {:invalid_config, errors}} =
             Config.load(Phase1Fixtures.config_file(yaml), workspace: Phase1Fixtures.workspace())

    assert Enum.any?(errors, &String.contains?(&1, "server.surprise"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.extra"))
    assert Enum.any?(errors, &String.contains?(&1, "server.port"))
    assert Enum.any?(errors, &String.contains?(&1, "backend.type"))
    assert Enum.any?(errors, &String.contains?(&1, "backend.command"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.plan"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.max_attempts"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.history_n"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.reference_catalog"))
    assert Enum.any?(errors, &String.contains?(&1, "campaign.stop_conditions.mode"))
  end

  test "initializes and recovers the fixed owned Workspace layout" do
    root = Phase1Fixtures.workspace()
    config_path = Phase1Fixtures.config_file()
    {:ok, config} = Config.load(config_path, workspace: root)
    {:ok, plan} = Workspace.plan(config)
    {:ok, first} = Workspace.activate(plan)
    File.touch!(first.database)

    assert File.dir?(first.repo)
    assert File.dir?(first.attempts)

    for kind <- Workspace.artifact_directories() do
      assert File.dir?(Path.join(first.artifacts, kind))
    end

    assert File.regular?(first.config_path)
    assert byte_size(first.config_hash) == 64
    assert Pika.Stage0.Git.run!(first.repo, ["rev-parse", "--abbrev-ref", "HEAD"]) == "pika/best"

    {:ok, recovered_config} = Config.load(config_path, workspace: root)
    {:ok, recovered_plan} = Workspace.plan(recovered_config)
    {:ok, second} = Workspace.activate(recovered_plan)
    assert second.config_hash == first.config_hash
    assert second.base_sha == first.base_sha
  end

  test "rejects a non-empty directory that is not a Pika Workspace" do
    root = Phase1Fixtures.workspace()
    File.write!(Path.join(root, "foreign-file"), "do not touch")

    {:ok, config} = Config.load(Phase1Fixtures.config_file(), workspace: root)
    assert {:error, {:workspace_not_empty, rejected_root}} = Workspace.plan(config)
    assert rejected_root == config.workspace
    assert File.read!(Path.join(root, "foreign-file")) == "do not touch"
  end

  test "rejects immutable listen and backend changes but accepts mutable Campaign settings" do
    root = Phase1Fixtures.workspace()
    initial_path = Phase1Fixtures.config_file()
    {:ok, config} = Config.load(initial_path, workspace: root)
    {:ok, plan} = Workspace.plan(config)
    {:ok, workspace} = Workspace.activate(plan)
    File.touch!(workspace.database)

    mutable = Phase1Fixtures.default_config() |> String.replace("history_n: 10", "history_n: 23")
    assert {:ok, changed} = Config.load(Phase1Fixtures.config_file(mutable), workspace: root)
    assert changed.campaign["history_n"] == 23

    immutable = Phase1Fixtures.default_config(18_081)

    assert {:error, {:immutable_config_changed, differences}} =
             Config.load(Phase1Fixtures.config_file(immutable), workspace: root)

    assert Enum.any?(differences, &String.contains?(&1, "listen.port"))
  end

  test "adds the foreground serve command to an assembled Elixir Release" do
    root = Phase1Fixtures.workspace()
    bin = Path.join(root, "bin")
    File.mkdir_p!(bin)
    executable = Path.join(bin, "pika")

    File.write!(
      executable,
      "#!/bin/sh\ncase $1 in\n  start) true;;\nesac\nThe known commands are:\n\n"
    )

    release = %Mix.Release{name: :pika, path: root}
    assert ^release = Pika.Release.add_cli(release)
    generated = File.read!(executable)
    assert generated =~ "serve)"
    assert generated =~ "Pika.CLI.main([\"serve\" | System.argv()])"
    assert generated =~ ~s(--boot "$REL_VSN_DIR/$RELEASE_BOOT_SCRIPT_CLEAN")
    assert generated =~ "serve          Starts Pika Server in the foreground"
  end
end
