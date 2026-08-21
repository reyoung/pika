defmodule Pika.Agent.AlignmentRolesTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Directory, Symphony}
  alias Pika.Alignment.Campaign
  alias Pika.Test.CampaignFixtures
  alias Pika.{CampaignStore, Config, Persistence, Repo, Workspace}

  setup do
    stop_campaign()
    root = CampaignFixtures.workspace()
    config_path = CampaignFixtures.config_file()
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
    assert {:ok, stage_workspace} = Pika.CampaignWorkspace.prepare(workspace, campaign)

    skill_dir = Path.join(root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)
    File.write!(Path.join(skill_dir, "SKILL.md"), "---\nname: ncu-report-skill\n---\n")

    skill = %{
      name: "ncu-report-skill",
      url: "https://example.invalid/ncu-report-skill.git",
      sha: String.duplicate("c", 40),
      path: skill_dir
    }

    references = [
      %{
        id: "cutlass",
        url: "https://github.com/NVIDIA/cutlass.git",
        description: "NVIDIA CUTLASS",
        selected: true,
        status: :resolved,
        branch: "main",
        sha: String.duplicate("a", 40)
      }
    ]

    actor_supervisor =
      start_supervised!({DynamicSupervisor, strategy: :one_for_one, name: nil})

    profile = %{
      "backend" => :alignment_fake,
      "reasoning_effort" => "low",
      "env" => %{},
      "protocol_config" => %{}
    }

    symphony =
      start_supervised!(
        {Symphony,
         name: nil,
         workspace: workspace,
         campaign_id: campaign.id,
         directory: Directory,
         actor_supervisor: actor_supervisor,
         sources: [Pika.Agent.WorkSources.Alignment],
         role_owners: %{
           "alignment" => :actor,
           "setup_merge" => :actor,
           "baseline" => :actor
         },
         actor_opts: [
           profile: profile,
           backend_modules: %{alignment_fake: Pika.Test.AlignmentAgentBackend},
           mcp_url: "http://127.0.0.1:18080/mcp",
           max_followups: 8
         ],
         capacity: 1,
         reconcile_interval_ms: :infinity}
      )

    {:ok, campaign_pid} =
      Campaign.start_link(
        workspace: stage_workspace,
        campaign_id: campaign.id,
        persistence: CampaignStore,
        backend: :codex_app_server,
        start_backend: true,
        role_owners: %{
          "alignment" => :actor,
          "setup_merge" => :actor,
          "baseline" => :actor
        },
        symphony: symphony,
        resolve_references: false,
        materialize_references: false,
        references: references,
        mcp_url: "http://127.0.0.1:18080/mcp",
        skill: skill
      )

    Process.unlink(campaign_pid)
    on_exit(&stop_campaign/0)

    %{
      workspace: workspace,
      campaign: campaign,
      campaign_pid: campaign_pid,
      symphony: symphony
    }
  end

  test "Alignment, Setup Merge, and Baseline run as separate Role Actors", context do
    assert :ok = Symphony.reconcile(context.symphony)

    eventually(fn ->
      match?(
        [["alignment", "running"]],
        Repo.query!(
          "SELECT role, status FROM agent_sessions WHERE campaign_id = ? ORDER BY started_at",
          [context.campaign.id]
        ).rows
      )
    end)

    refute Campaign.snapshot().agent_responding
    assert :ok = Campaign.send_message("创建可审阅的 Campaign Spec 和实现。")

    eventually(
      fn ->
        snapshot = Campaign.snapshot()
        snapshot.status == :awaiting_confirmation and not snapshot.agent_responding
      end,
      300
    )

    assert {:ok, review} = Campaign.implementation_review()
    evidence = Campaign.snapshot().implementation_review_evidence

    assert :ok =
             Campaign.confirm_spec(
               review.target_snapshot.digest,
               evidence.development_sha,
               evidence.digest
             )

    eventually(fn -> Campaign.snapshot().status == :optimizing end, 500)

    eventually(fn ->
      Repo.query!(
        "SELECT COUNT(*) FROM agent_sessions WHERE campaign_id = ? AND role IN ('alignment', 'setup_merge', 'baseline') AND status != 'completed'",
        [context.campaign.id]
      ).rows == [[0]]
    end)

    rows =
      Repo.query!(
        "SELECT role, work_kind, session_mode, status, provider_session_id FROM agent_sessions WHERE campaign_id = ? ORDER BY started_at",
        [context.campaign.id]
      ).rows

    assert Enum.map(rows, &Enum.at(&1, 0)) == ~w(alignment setup_merge baseline)
    assert Enum.all?(rows, &(Enum.at(&1, 1) == "campaign_revision"))
    assert Enum.all?(rows, &(Enum.at(&1, 2) == "fresh"))
    assert Enum.all?(rows, &(Enum.at(&1, 3) == "completed"))
    assert rows |> Enum.map(&List.last/1) |> Enum.uniq() |> length() == 3

    assert Repo.query!(
             "SELECT COUNT(*) FROM agent_sessions WHERE campaign_id = ? AND role = 'boundary'",
             [context.campaign.id]
           ).rows == [[0]]
  end

  defp eventually(predicate, attempts \\ 200)

  defp eventually(predicate, attempts) when attempts > 0 do
    if predicate.() do
      :ok
    else
      Process.sleep(20)
      eventually(predicate, attempts - 1)
    end
  end

  defp eventually(_predicate, 0), do: flunk("condition did not become true")

  defp stop_campaign do
    case Process.whereis(Campaign) do
      nil -> :ok
      pid -> GenServer.stop(pid, :normal, 15_000)
    end
  catch
    :exit, _reason -> :ok
  end
end
