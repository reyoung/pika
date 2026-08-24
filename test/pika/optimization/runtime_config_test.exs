defmodule Pika.Optimization.RuntimeConfigTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.RuntimeConfig

  test "reloads expanded Backend configs and projects Role concurrency" do
    workspace = workspace(minimal_yaml(2))

    assert {:ok, configs} = RuntimeConfig.backend_configs(workspace, :iteration)
    assert length(configs) == 2
    assert Enum.map(configs, & &1.backend) == [:codex_app_server, :cursor_acp]

    assert {:ok, 1} = RuntimeConfig.concurrency(workspace, :integration)
    assert {:ok, 0} = RuntimeConfig.concurrency(workspace, :progress_summary)
  end

  test "new calls observe changed YAML while the old value remains immutable" do
    workspace = workspace(minimal_yaml(1))

    assert {:ok, first} = RuntimeConfig.backend_configs(workspace, :iteration)
    assert length(first) == 1

    File.write!(Path.join(workspace, "pika.yaml"), minimal_yaml(2))

    assert {:ok, second} = RuntimeConfig.backend_configs(workspace, :iteration)
    assert length(second) == 2
    assert length(first) == 1
  end

  defp workspace(yaml) do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-runtime-config-#{System.unique_integer([:positive])}")

    File.mkdir_p!(root)
    File.write!(Path.join(root, "pika.yaml"), yaml)
    on_exit(fn -> File.rm_rf!(root) end)
    root
  end

  defp minimal_yaml(iteration_agents) do
    cursor =
      if iteration_agents == 2 do
        "      - backend: cursor\n        approval_policy: never\n        sandbox: workspace-write\n"
      else
        ""
      end

    """
    version: 2
    repo: repo
    workspace: .
    agents:
      baseline_alignment:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      baseline_verify:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
      iteration:
        agents:
          - backend: codex
            approval_policy: never
            sandbox: workspace-write
    #{cursor}  integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
