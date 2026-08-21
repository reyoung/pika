defmodule Pika.ProgressSummaryCoordinatorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Directory, Symphony}
  alias Pika.Test.CampaignFixtures

  alias Pika.{
    Config,
    Persistence,
    ProgressSummaryCoordinator,
    ProgressSummaryStore,
    Repo,
    Workspace
  }

  setup do
    root = CampaignFixtures.workspace()
    config_path = Path.join(root, "pika.yaml")

    File.write!(config_path, """
    campaign:
      progress_summary:
        enabled: true
        interval_minutes: 10
        backend: codex_app_server
        model: summary-model
        reasoning_effort: low
    """)

    {:ok, config} = Config.load(config_path, workspace: root)
    {:ok, plan} = Workspace.plan(config)
    {:ok, workspace} = Workspace.activate(plan)

    Application.put_env(:pika, Repo,
      database: workspace.database,
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()
    assert {:ok, campaign, :initialized} = Persistence.initialize_or_recover(workspace)
    Repo.query!("UPDATE campaigns SET status = 'building_baseline' WHERE id = ?", [campaign.id])

    directory = start_supervised!({Directory, name: nil})
    actor_supervisor = start_supervised!({DynamicSupervisor, strategy: :one_for_one, name: nil})

    symphony =
      start_supervised!(
        {Symphony,
         name: nil,
         workspace: workspace,
         campaign_id: campaign.id,
         directory: directory,
         actor_supervisor: actor_supervisor,
         capacity: 0,
         reconcile_interval_ms: :infinity}
      )

    %{workspace: workspace, campaign: campaign, symphony: symphony}
  end

  test "timer persists a request and delegates execution to Symphony", context do
    coordinator =
      start_supervised!(
        {ProgressSummaryCoordinator,
         name: nil,
         workspace: context.workspace,
         campaign_id: context.campaign.id,
         symphony: context.symphony,
         interval_ms: :infinity}
      )

    assert :ok = ProgressSummaryCoordinator.trigger(coordinator)

    assert [work] = ProgressSummaryStore.runnable_work(context.campaign.id)
    assert {:ok, request} = ProgressSummaryStore.get(work.id)
    assert request.status == "requested"
    assert request.model == "summary-model"
    assert request.reasoning_effort == "low"
    assert request.context["phase"] == "baseline"
    assert [] == Symphony.active(context.symphony)
    assert [[0]] = Repo.query!("SELECT COUNT(*) FROM agent_sessions").rows
  end
end
