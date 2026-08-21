defmodule Pika.Agent.SyncRoleTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.Role.{Invocation, Work}
  alias Pika.Agent.Roles
  alias Pika.Agent.Roles.Sync
  alias Pika.Test.OptimizationFixtures
  alias Pika.SyncStore

  setup do
    on_exit(fn -> OptimizationFixtures.stop_repo() end)
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)

    assert {:ok, run} =
             SyncStore.request(
               context.campaign.id,
               "origin",
               "main",
               context.best_sha,
               context.best_sha,
               "sync-role-request"
             )

    assert {:ok, _} = SyncStore.begin_prepare(run.id)
    assert {:ok, run} = SyncStore.mark_merging(run.id)

    work = %Work{
      role_id: "sync",
      kind: :sync,
      id: run.id,
      campaign_id: context.campaign.id
    }

    %{context: context, run: run, work: work}
  end

  test "Sync Role projects the committed run into its catalog and completion gate", test do
    profile = %{"backend" => "codex_app_server", "reasoning_effort" => "high"}

    assert {:ok, prepared} =
             Roles.prepare(test.work, :fresh,
               role: Sync,
               workspace: test.context.workspace,
               profile: profile
             )

    assert prepared.definition.id == "sync"
    assert prepared.cwd == Path.join(test.context.workspace.root, test.run.worktree_relative_path)
    assert prepared.progress.required_operations == ["report_sync_candidate"]
    assert prepared.instructions.system =~ "Pika Sync Agent"

    assert Enum.map(prepared.definition.tools, & &1.name) == [
             "get_sync_context",
             "register_artifact",
             "report_sync_candidate",
             "submit_sync_validation",
             "create_sync_intent",
             "complete_sync"
           ]

    assert {:ok, outcome} =
             Roles.invoke(prepared, %Invocation{
               operation: "get_sync_context",
               arguments: %{}
             })

    assert outcome.value.sync_run.id == test.run.id
    assert outcome.value.required_operations == ["report_sync_candidate"]
    assert outcome.actor_directive == :keep_running
  end
end
