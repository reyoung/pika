defmodule Pika.Agent.SymphonyTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Actor, Directory, Symphony}
  alias Pika.Agent.Role.Work
  alias Pika.AgentBackend.Session
  alias Pika.Test.CampaignFixtures
  alias Pika.{Config, Persistence, ProgressSummaryStore, Repo, Workspace}

  defmodule Backend do
    @behaviour Pika.AgentBackend

    def start_link(profile, sink), do: Agent.start_link(fn -> %{profile: profile, sink: sink} end)

    def open_session(server, cwd, model, effort, mcp, _skills, instructions) do
      state = Agent.get(server, & &1)

      session = %Session{
        id: Ecto.UUID.generate(),
        backend: :symphony_fake,
        backend_protocol: "symphony-fake-v1",
        backend_session_id: Ecto.UUID.generate(),
        cwd: cwd,
        model: model,
        reasoning_effort: effort,
        jsonl_path: "/dev/null"
      }

      send(state.profile.env.test_pid, {:symphony_session_opened, session, mcp, instructions})
      {:ok, session}
    end

    def start_turn(_server, prompt) do
      send(test_pid(), {:symphony_turn_started, prompt})
      {:ok, Ecto.UUID.generate()}
    end

    def steer(_server, _input), do: {:error, :unsupported}
    def interrupt(_server), do: :ok
    def close_session(_server), do: :ok
    def capabilities(_server), do: %{}

    defp test_pid, do: :persistent_term.get({__MODULE__, :test_pid})
  end

  setup do
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

    directory = start_supervised!({Directory, name: nil})

    actor_supervisor =
      start_supervised!({DynamicSupervisor, strategy: :one_for_one, name: nil})

    :persistent_term.put({Backend, :test_pid}, self())
    on_exit(fn -> :persistent_term.erase({Backend, :test_pid}) end)

    %{
      workspace: workspace,
      campaign: campaign,
      directory: directory,
      actor_supervisor: actor_supervisor
    }
  end

  test "reconcile starts one Actor per Work and recovery creates a fresh Session", context do
    profile = %{
      "backend" => :symphony_fake,
      "model" => "fake-model",
      "reasoning_effort" => "low",
      "env" => %{test_pid: self()}
    }

    assert {:ok, request} =
             ProgressSummaryStore.request(context.campaign.id, %{phase: "attempts"}, profile)

    symphony =
      start_supervised!(
        {Symphony,
         name: nil,
         workspace: context.workspace,
         campaign_id: context.campaign.id,
         directory: context.directory,
         actor_supervisor: context.actor_supervisor,
         reconcile_interval_ms: :infinity,
         actor_opts: [
           profile: profile,
           backend_modules: %{symphony_fake: Backend},
           mcp_url: "http://127.0.0.1:8080/mcp"
         ]}
      )

    assert :ok = Symphony.reconcile(symphony)
    assert :ok = Symphony.reconcile(symphony)
    assert_receive {:symphony_session_opened, first_session, _mcp, _instructions}
    assert_receive {:symphony_turn_started, first_prompt}
    refute first_prompt =~ "恢复这次中断"

    assert [{work, first_actor}] = Symphony.active(symphony)
    assert work.id == request.id

    assert [[1]] =
             Repo.query!("SELECT COUNT(*) FROM agent_sessions WHERE work_id = ?", [request.id]).rows

    assert :ok = DynamicSupervisor.terminate_child(context.actor_supervisor, first_actor)

    wait_until(fn ->
      match?({:error, :not_found}, Directory.lookup_work(work, context.directory))
    end)

    assert :ok = Symphony.reconcile(symphony)
    assert_receive {:symphony_session_opened, second_session, _mcp, recovery_instructions}
    assert first_session.id != second_session.id
    assert recovery_instructions =~ "Progress Summary Actor"
    assert_receive {:symphony_turn_started, recovery_prompt}
    assert recovery_prompt =~ "恢复这次中断"

    assert [["interrupted"], ["running"]] =
             Repo.query!(
               "SELECT status FROM agent_sessions WHERE work_id = ? ORDER BY started_at",
               [request.id]
             ).rows

    assert [["fresh"], ["recovering"]] =
             Repo.query!(
               "SELECT session_mode FROM agent_sessions WHERE work_id = ? ORDER BY started_at",
               [request.id]
             ).rows
  end

  test "a restarted Symphony adopts an Actor already registered for its Work", context do
    profile = %{
      "backend" => :symphony_fake,
      "model" => "fake-model",
      "reasoning_effort" => "low",
      "env" => %{test_pid: self()}
    }

    assert {:ok, request} =
             ProgressSummaryStore.request(context.campaign.id, %{phase: "attempts"}, profile)

    work = %Work{
      role_id: "progress_summary",
      kind: :progress_summary,
      id: request.id,
      campaign_id: context.campaign.id
    }

    actor_opts = [
      work: work,
      workspace: context.workspace,
      profile: profile,
      directory: context.directory,
      backend_modules: %{symphony_fake: Backend},
      mcp_url: "http://127.0.0.1:8080/mcp"
    ]

    assert {:ok, actor} =
             DynamicSupervisor.start_child(context.actor_supervisor, {Actor, actor_opts})

    assert_receive {:symphony_session_opened, _session, _mcp, _instructions}
    assert_receive {:symphony_turn_started, _prompt}

    symphony =
      start_supervised!(
        {Symphony,
         name: nil,
         workspace: context.workspace,
         campaign_id: context.campaign.id,
         directory: context.directory,
         actor_supervisor: context.actor_supervisor,
         reconcile_interval_ms: :infinity,
         actor_opts: [
           profile: profile,
           backend_modules: %{symphony_fake: Backend},
           mcp_url: "http://127.0.0.1:8080/mcp"
         ]}
      )

    assert :ok = Symphony.reconcile(symphony)
    assert [{^work, ^actor}] = Symphony.active(symphony)
    refute_receive {:symphony_session_opened, _session, _mcp, _instructions}, 100

    assert Repo.query!("SELECT COUNT(*) FROM agent_sessions WHERE work_id = ?", [request.id]).rows ==
             [[1]]
  end

  defp wait_until(predicate, attempts \\ 100)

  defp wait_until(predicate, attempts) when attempts > 0 do
    if predicate.() do
      :ok
    else
      Process.sleep(10)
      wait_until(predicate, attempts - 1)
    end
  end

  defp wait_until(_predicate, 0), do: flunk("condition did not become true")
end
