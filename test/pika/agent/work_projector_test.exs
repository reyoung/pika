Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.WorkProjectorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Work, WorkProjector}
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-projector-#{System.unique_integer([:positive])}")

    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(baseline.repo, workspace))
    assert {:ok, config} = Config.load(config_path)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()

    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, baseline.development_sha)

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline: baseline, config: config}
  end

  test "projects Baseline and then fills every configured Iteration slot", %{
    baseline: baseline,
    config: config
  } do
    assert {:ok, draft} = BaselineLifecycle.ensure_draft(baseline.root)

    assert {:ok, [%Work{role_id: "baseline_alignment", id: work_id} = alignment]} =
             WorkProjector.reconcile(config)

    assert work_id == to_string(draft.id)
    refute WorkProjector.terminal?(alignment)

    assert {:ok, _submitted} =
             BaselineLifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert WorkProjector.terminal?(alignment)
    assert {:ok, []} = WorkProjector.reconcile(config)

    assert {:ok, approved} =
             BaselineLifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    assert {:ok, [%Work{role_id: "baseline_verify"}]} = WorkProjector.reconcile(config)
    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             BaselineLifecycle.finish_verification(
               0,
               baseline.root,
               "baseline-verification-result.json"
             )

    assert {:ok, works} = WorkProjector.reconcile(config)
    assert Enum.map(works, & &1.role_id) == ["iteration", "iteration"]
    assert Enum.map(works, & &1.payload.attempt.slot_index) == [0, 1]
    assert Enum.map(works, & &1.id) == ["1", "2"]
  end

  defp yaml(repo, workspace) do
    """
    version: 2
    repo: #{repo}
    workspace: #{workspace}
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
          - backend: cursor
            approval_policy: force
            sandbox: disabled
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
