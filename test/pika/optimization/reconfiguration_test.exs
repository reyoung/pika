defmodule Pika.Optimization.ReconfigurationTest do
  use ExUnit.Case, async: true

  import ExUnit.CaptureIO

  alias Pika.Git
  alias Pika.Optimization.{Init, Reconfiguration}

  setup do
    root = Path.join(System.tmp_dir!(), "pika-reconfigure-#{System.unique_integer([:positive])}")
    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    Git.run!(repo, ["init", "--initial-branch=main"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "kernel.py"), "def run(): return 1\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "initial"])
    {:ok, config} = Init.run(workspace, repo, model: "original-model")
    on_exit(fn -> File.rm_rf!(root) end)

    %{workspace: workspace, config_path: config.source_path}
  end

  test "interactively changes one Role's backend, full model choice, and effort", context do
    catalog = fn "cursor_headless" ->
      {:ok,
       Enum.map(1..16, fn index ->
         %{id: "cursor-#{index}", label: "Cursor #{index}", description: nil}
       end)}
    end

    parent = self()

    output =
      capture_io("2\n17\n6\n0\n", fn ->
        assert {:ok, config} =
                 Reconfiguration.run(context.workspace, context.config_path,
                   role: "baseline_alignment",
                   model_catalog: catalog
                 )

        send(parent, {:config, config})
      end)

    assert_receive {:config, config}
    assert config.baseline_alignment.agent.backend == :cursor_headless
    assert config.baseline_alignment.agent.model == "cursor-16"
    assert config.baseline_alignment.agent.reasoning_effort == "ultra"
    assert config.baseline_alignment.agent.approval_policy == "force"
    assert config.baseline_alignment.agent.sandbox == "disabled"

    assert config.baseline_verify.agent.backend == :codex_app_server
    assert config.baseline_verify.agent.model == "original-model"
    assert is_binary(config.token)
    assert Bitwise.band(File.stat!(context.config_path).mode, 0o777) == 0o600
    assert output =~ "cursor-16"
    assert output =~ "active Sessions are unchanged"
  end

  test "reconfigures every Iteration Agent separately and persists expanded YAML", context do
    catalog = fn backend ->
      prefix = if backend == "cursor_headless", do: "cursor", else: "codex"
      {:ok, [%{id: "#{prefix}-model", label: "#{prefix} model", description: nil}]}
    end

    parent = self()

    capture_io("2\n1\n2\n1\n0\n2\n2\n6\n0\n", fn ->
      assert {:ok, config} =
               Reconfiguration.run(context.workspace, context.config_path,
                 role: "iteration",
                 model_catalog: catalog
               )

      send(parent, {:config, config})
    end)

    assert_receive {:config, config}
    assert [first, second] = config.iteration.agents

    assert {first.backend, first.model, first.reasoning_effort} ==
             {:codex_app_server, "codex-model", "low"}

    assert {second.backend, second.model, second.reasoning_effort} ==
             {:cursor_headless, "cursor-model", "ultra"}

    yaml = File.read!(context.config_path)
    assert length(Regex.scan(~r/^      - backend:/m, yaml)) == 2
  end

  test "non-interactive reconfiguration rejects ACP and accepts Cursor Headless", context do
    assert {:error, :cursor_acp_deprecated} =
             Reconfiguration.run(context.workspace, context.config_path,
               role: "progress_summary",
               enabled: true,
               backend: "cursor",
               model: "summary-model",
               reasoning_effort: "max",
               yes: true
             )

    assert {:ok, config} =
             Reconfiguration.run(context.workspace, context.config_path,
               role: "progress_summary",
               enabled: true,
               backend: "cursor-headless",
               model: "summary-model",
               reasoning_effort: "max",
               yes: true
             )

    assert config.progress_summary.agent.backend == :cursor_headless
    assert config.progress_summary.agent.model == "summary-model"
    assert config.progress_summary.agent.reasoning_effort == "max"
    assert config.progress_summary.agent.sandbox == "disabled"
  end

  test "visited ACP slots must migrate while unvisited ACP remains explicitly legacy", context do
    yaml = File.read!(context.config_path)

    legacy =
      String.replace(
        yaml,
        """
          baseline_alignment:
            backend: codex
            model: "original-model"
            reasoning_effort: "high"
            approval_policy: "never"
            sandbox: "danger_full_access"
        """,
        """
          baseline_alignment:
            backend: cursor_acp
            model: "original-model"
            reasoning_effort: "high"
            approval_policy: "force"
            sandbox: "disabled"
        """
      )

    File.write!(context.config_path, legacy)

    assert {:error, :cursor_acp_deprecated} =
             Reconfiguration.run(context.workspace, context.config_path,
               role: "baseline_alignment",
               yes: true
             )

    parent = self()

    capture_io("\n\n\n0\n", fn ->
      assert {:ok, migrated} =
               Reconfiguration.run(context.workspace, context.config_path,
                 role: "baseline_alignment",
                 model_catalog: fn "cursor_headless" -> {:ok, []} end
               )

      send(parent, {:migrated, migrated})
    end)

    assert_receive {:migrated, migrated}
    assert migrated.baseline_alignment.agent.backend == :cursor_headless

    explicit_legacy =
      File.read!(context.config_path)
      |> String.replace(
        """
              - backend: codex
                model: "original-model"
                reasoning_effort: "high"
                approval_policy: "never"
                sandbox: "danger_full_access"
        """,
        """
              - backend: cursor_acp
                model: "original-model"
                reasoning_effort: "high"
                approval_policy: "force"
                sandbox: "disabled"
        """
      )

    File.write!(context.config_path, explicit_legacy)

    assert {:ok, _config} =
             Reconfiguration.run(context.workspace, context.config_path,
               role: "integration",
               backend: "codex",
               model: "integration-model",
               reasoning_effort: "high",
               yes: true
             )

    rendered = File.read!(context.config_path)
    assert rendered =~ "      - backend: cursor_acp"
    refute rendered =~ "      - backend: cursor\n"
  end
end
