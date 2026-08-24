Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Optimization.StopPolicyTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.WorkProjector
  alias Pika.Baseline.Lifecycle
  alias Pika.Optimization.{Config, Persistence, StopPolicy}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-stop-policy-#{System.unique_integer([:positive])}")

    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)

    definition = V2BaselineFixtures.read_json(baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_json(
      baseline.root,
      "baseline-definition.json",
      put_in(definition, ["stopping"], %{
        "mode" => "attempt_limit",
        "max_attempts" => 1,
        "max_duration_seconds" => nil
      })
    )

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

    assert {:ok, _draft} = Lifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             Lifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, approved} =
             Lifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             Lifecycle.finish_verification(0, baseline.root, "baseline-verification-result.json")

    on_exit(fn -> File.rm_rf!(root) end)
    %{config: config}
  end

  test "stops new Attempt creation at the reviewed limit and enters Draining", %{config: config} do
    assert :continue = StopPolicy.evaluate()
    assert {:ok, [%{role_id: "iteration", id: "1"}]} = WorkProjector.reconcile(config)
    assert {:drain, "attempt_limit"} = StopPolicy.evaluate()
    assert {:ok, [%{role_id: "iteration", id: "1"}]} = WorkProjector.reconcile(config)
    assert Persistence.current().status == "draining"
    assert Persistence.current().stop_reason == "attempt_limit"
    assert Repo.query!("SELECT COUNT(*) FROM attempts").rows == [[1]]
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
