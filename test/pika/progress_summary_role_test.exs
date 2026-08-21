defmodule Pika.ProgressSummaryRoleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.Role.{Invocation, Work}
  alias Pika.Agent.Roles
  alias Pika.Agent.Roles.ProgressSummary
  alias Pika.Test.CampaignFixtures
  alias Pika.{Config, Persistence, ProgressSummaryStore, Repo, Workspace}

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

    %{workspace: workspace, campaign: campaign}
  end

  test "creates one durable Progress Summary Request as runnable Agent Work", context do
    snapshot = %{
      phase: "attempts",
      observed_at: "2026-08-22T00:00:00Z",
      active_attempts: [%{id: "attempt-1"}]
    }

    profile = %{
      "backend" => "codex_app_server",
      "model" => "summary-model",
      "reasoning_effort" => "medium"
    }

    assert {:ok, request} =
             ProgressSummaryStore.request(context.campaign.id, snapshot, profile)

    assert request.status == "requested"
    assert request.context == snapshot

    assert [%Work{} = work] = ProgressSummaryStore.runnable_work(context.campaign.id)
    assert work.role_id == "progress_summary"
    assert work.kind == :progress_summary
    assert work.id == request.id

    assert {:ok, same_request} =
             ProgressSummaryStore.request(context.campaign.id, snapshot, profile)

    assert same_request.id == request.id
  end

  test "Progress Summary Role exposes only snapshot and durable submission operations", context do
    snapshot = %{
      phase: "attempts",
      observed_at: "2026-08-22T00:00:00Z",
      active_attempts: [%{id: "attempt-1", status: "integrating"}]
    }

    profile = %{
      "backend" => "codex_app_server",
      "model" => "summary-model",
      "reasoning_effort" => "medium"
    }

    assert {:ok, request} = ProgressSummaryStore.request(context.campaign.id, snapshot, profile)
    [work] = ProgressSummaryStore.runnable_work(context.campaign.id)

    assert {:ok, prepared} =
             Roles.prepare(work, :fresh,
               role: ProgressSummary,
               workspace: context.workspace,
               profile: profile
             )

    assert Enum.map(prepared.definition.tools, &{&1.name, &1.kind}) == [
             {"get_progress_context", :query},
             {"submit_progress_summary", :command}
           ]

    assert {:start_turn, prompt} = prepared.activation
    assert prompt =~ "attempts"
    assert prepared.progress.required_operations == ["submit_progress_summary"]

    assert {:ok, observed} =
             Roles.invoke(prepared, %Invocation{
               operation: "get_progress_context",
               arguments: %{}
             })

    assert observed.value.context["observed_at"] == snapshot.observed_at
    assert observed.actor_directive == :keep_running

    assert {:ok, submitted} =
             Roles.invoke(prepared, %Invocation{
               operation: "submit_progress_summary",
               arguments: %{"content" => "Attempt 1 is currently integrating."},
               idempotency_key: "summary-#{request.id}"
             })

    assert submitted.actor_directive == :finish
    assert submitted.progress.state == {:terminal, :completed}
    assert {:ok, stored} = ProgressSummaryStore.get(request.id)
    assert stored.status == "completed"
    assert stored.content == "Attempt 1 is currently integrating."
  end

  test "command idempotency is scoped to Role Work and rejects a changed request", context do
    profile = %{"backend" => "codex_app_server"}

    assert {:ok, request} =
             ProgressSummaryStore.request(context.campaign.id, %{phase: "baseline"}, profile)

    [work] = ProgressSummaryStore.runnable_work(context.campaign.id)

    assert {:ok, prepared} =
             Roles.prepare(work, :fresh,
               role: ProgressSummary,
               workspace: context.workspace,
               profile: profile
             )

    invocation = %Invocation{
      operation: "submit_progress_summary",
      arguments: %{"content" => "Baseline measurement is running."},
      idempotency_key: "submit-once"
    }

    assert {:ok, first} = Roles.invoke(prepared, invocation)
    assert {:ok, replayed} = Roles.invoke(prepared, invocation)
    assert replayed.value == first.value

    changed = put_in(invocation.arguments["content"], "A conflicting summary.")
    assert {:error, error} = Roles.invoke(prepared, changed)
    assert error.code == :idempotency_conflict

    assert [[1]] =
             Repo.query!("SELECT COUNT(*) FROM agent_operation_receipts WHERE work_id = ?", [
               request.id
             ]).rows

    assert {:ok, stored} = ProgressSummaryStore.get(request.id)
    assert stored.content == "Baseline measurement is running."
  end
end
