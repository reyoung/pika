Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Test.ProjectedActor do
  use GenServer, restart: :temporary

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts)
  def kickoff(pid, message), do: GenServer.call(pid, {:kickoff, message})
  def stop(pid), do: GenServer.stop(pid, :normal)

  @impl true
  def init(opts) do
    if notify = Keyword.get(opts, :test_notify),
      do: send(notify, {:projected_actor_started, Keyword.fetch!(opts, :work)})

    {:ok, opts}
  end

  @impl true
  def handle_call({:kickoff, message}, _from, state), do: {:reply, {:ok, message}, state}
end

defmodule Pika.Agent.SymphonyTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Directory, Symphony}
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root = Path.join(System.tmp_dir!(), "pika-v2-symphony-#{System.unique_integer([:positive])}")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(workspace, "pika.yaml")
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

    assert {:ok, _draft} = BaselineLifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             BaselineLifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, approved} =
             BaselineLifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             BaselineLifecycle.finish_verification(
               0,
               baseline.root,
               "baseline-verification-result.json"
             )

    directory = start_supervised!({Directory, name: nil})
    actor_supervisor = start_supervised!({DynamicSupervisor, strategy: :one_for_one})

    on_exit(fn -> File.rm_rf!(root) end)

    %{actor_supervisor: actor_supervisor, directory: directory, workspace: workspace}
  end

  test "starts every Iteration slot without a global Actor capacity", %{
    actor_supervisor: actor_supervisor,
    directory: directory,
    workspace: workspace
  } do
    symphony =
      start_supervised!(
        {Symphony,
         name: nil,
         workspace: workspace,
         directory: directory,
         actor_supervisor: actor_supervisor,
         actor_module: Pika.Test.ProjectedActor,
         actor_opts: [test_notify: self()],
         reconcile_interval_ms: :infinity}
      )

    assert :ok = Symphony.reconcile(symphony)
    assert_receive {:projected_actor_started, first}, 1_000
    assert_receive {:projected_actor_started, second}, 1_000
    assert Enum.sort([first.id, second.id]) == ["1", "2"]
    assert first.role_id == "iteration"
    assert second.role_id == "iteration"
    assert length(Symphony.active(symphony)) == 2

    assert :ok = Symphony.reconcile(symphony)
    refute_receive {:projected_actor_started, _work}, 100
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
