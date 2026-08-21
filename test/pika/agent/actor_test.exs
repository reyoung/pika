defmodule Pika.Agent.ActorTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{Actor, Directory}
  alias Pika.Agent.Role.{Context, Definition, DomainContext, Tool, Work}
  alias Pika.Agent.Roles.ProgressSummary
  alias Pika.AgentBackend.Session
  alias Pika.Test.CampaignFixtures
  alias Pika.{Config, Persistence, ProgressSummaryStore, Repo, Workspace}

  defmodule Backend do
    @behaviour Pika.AgentBackend

    def start_link(profile, sink), do: Agent.start_link(fn -> %{profile: profile, sink: sink} end)

    def open_session(server, cwd, model, effort, mcp, skill_roots, instructions) do
      state = Agent.get(server, & &1)

      session = %Session{
        id: Ecto.UUID.generate(),
        backend: :fake,
        backend_protocol: "fake-v1",
        backend_session_id: Ecto.UUID.generate(),
        cwd: cwd,
        model: model,
        reasoning_effort: effort,
        jsonl_path: "/dev/null"
      }

      Agent.update(server, &Map.put(&1, :session, session))

      send(
        state.profile.env.test_pid,
        {:actor_backend_opened, mcp, skill_roots, instructions, session}
      )

      {:ok, session}
    end

    def start_turn(server, prompt) do
      state = Agent.get(server, & &1)
      turn_id = Ecto.UUID.generate()
      send(state.profile.env.test_pid, {:actor_turn_started, prompt, turn_id})
      {:ok, turn_id}
    end

    def steer(_server, _input), do: {:error, :unsupported}
    def interrupt(_server), do: :ok

    def close_session(server) do
      if Process.alive?(server) do
        state = Agent.get(server, & &1)
        send(state.profile.env.test_pid, :actor_backend_closed)
      end

      :ok
    end

    def capabilities(_server), do: %{protocol: "fake-v1"}
  end

  defmodule BlockingDomain do
    @behaviour Pika.Agent.Role.DomainAdapter

    @impl true
    def prepare(%Work{}, workspace) do
      {:ok,
       %DomainContext{
         facts: %{done: false, _revision: 1},
         durable_context: %{},
         cwd: workspace.root,
         skill_roots: []
       }}
    end

    @impl true
    def invoke(%Work{}, "wait", %{"test_pid" => test_pid}, _meta) do
      send(test_pid, {:blocking_role_waiting, self()})

      receive do
        :release -> {:ok, %{released: true}}
      end
    end
  end

  defmodule BlockingRole do
    @behaviour Pika.Agent.Role

    @impl true
    def definition do
      %Definition{
        id: "blocking_test",
        contract_revision: 1,
        activation: :automatic,
        work_kind: :blocking_test,
        profile_key: "blocking_test",
        domain_adapter: Pika.Agent.ActorTest.BlockingDomain,
        template: %{relative_path: "prompts/roles/blocking_test.md", builtin: "Wait safely."},
        tools: [
          %Tool{
            name: "wait",
            description: "Wait for an external answer.",
            kind: :query,
            input_schema: %{"type" => "object", "additionalProperties" => true}
          }
        ],
        completion: %{
          terminals: [{:completed, {:fact, :done}}],
          suggestions: []
        }
      }
    end

    @impl true
    def build_system_instructions(%Context{} = context), do: {:ok, context.template}

    @impl true
    def initial_prompt(%Context{}), do: {:ok, "Start waiting."}

    @impl true
    def recovery_prompt(%Context{}), do: {:ok, "Recover waiting."}
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

    %{workspace: workspace, campaign: campaign, directory: directory}
  end

  test "Actor owns one frozen Session, routes Role calls, then revokes its token", context do
    profile = %{
      "backend" => :fake,
      "model" => "fake-model",
      "reasoning_effort" => "medium",
      "env" => %{test_pid: self()}
    }

    assert {:ok, request} =
             ProgressSummaryStore.request(
               context.campaign.id,
               %{phase: "attempts", observed_at: "2026-08-22T01:00:00Z"},
               profile
             )

    work = %Work{
      role_id: "progress_summary",
      kind: :progress_summary,
      id: request.id,
      campaign_id: context.campaign.id
    }

    assert {:ok, actor} =
             Actor.start_link(
               work: work,
               role: ProgressSummary,
               workspace: context.workspace,
               profile: profile,
               directory: context.directory,
               backend_modules: %{fake: Backend},
               mcp_url: "http://127.0.0.1:8080/mcp",
               notify: self()
             )

    monitor = Process.monitor(actor)

    assert_receive {:actor_backend_opened, mcp, [], instructions, session}
    assert mcp.url == "http://127.0.0.1:8080/mcp"
    assert instructions =~ "Progress Summary Actor"
    assert {:ok, binding} = Directory.lookup(mcp.token, context.directory)
    assert binding.actor == actor
    assert binding.work == work
    assert binding.session_id == session.id

    assert_receive {:actor_turn_started, prompt, _turn_id}
    assert prompt =~ "attempts"

    assert {:ok, outcome} =
             Actor.invoke(
               actor,
               "submit_progress_summary",
               %{
                 "idempotency_key" => "actor-summary",
                 "content" => "The attempt is active."
               }
             )

    assert outcome.actor_directive == :finish
    assert_receive :actor_backend_closed
    assert_receive {:DOWN, ^monitor, :process, ^actor, :normal}
    assert {:error, :unauthorized} = Directory.lookup(mcp.token, context.directory)

    request_id = request.id

    assert [["completed", "progress_summary", "progress_summary", ^request_id, 1]] =
             Repo.query!(
               "SELECT status, role, work_kind, work_id, role_contract_revision FROM agent_sessions WHERE id = ?",
               [session.id]
             ).rows
  end

  test "a long domain interaction does not block status or Stop", context do
    profile = %{
      "backend" => :fake,
      "model" => "fake-model",
      "reasoning_effort" => "medium",
      "env" => %{test_pid: self()}
    }

    work = %Work{
      role_id: "blocking_test",
      kind: :blocking_test,
      id: Ecto.UUID.generate(),
      campaign_id: context.campaign.id
    }

    assert {:ok, actor} =
             Actor.start_link(
               work: work,
               role: BlockingRole,
               workspace: context.workspace,
               profile: profile,
               directory: context.directory,
               backend_modules: %{fake: Backend},
               mcp_url: "http://127.0.0.1:8080/mcp"
             )

    assert_receive {:actor_backend_opened, _mcp, [], _instructions, _session}
    assert_receive {:actor_turn_started, "Start waiting.", _turn_id}

    test_pid = self()

    invocation =
      Task.async(fn -> Actor.invoke(actor, "wait", %{"test_pid" => test_pid}) end)

    assert_receive {:blocking_role_waiting, domain_task}
    assert Process.alive?(domain_task)
    assert %{phase: :running} = Actor.status(actor)

    monitor = Process.monitor(actor)
    assert :ok = Actor.stop(actor)
    assert Task.await(invocation) == {:error, :actor_stopped}
    assert_receive {:DOWN, ^monitor, :process, ^actor, :normal}
    refute Process.alive?(domain_task)
  end
end
