defmodule Pika.SyncControlTest do
  use ExUnit.Case, async: false

  alias Pika.{Control, Git, Repo, SyncCoordinator, SyncStore}
  alias Pika.Test.{AlignmentFixtures, OptimizationFixtures, SyncAgentBackend}

  setup do
    on_exit(fn -> OptimizationFixtures.stop_repo() end)
    :ok
  end

  test "user-confirmed Sync merges remote, validates all cases, persists Intent, and advances both remote and Best" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    remote_commit(remote, "README.md", "remote update\n")
    coordinator = start_sync(context)

    assert {:ok, preview} = SyncCoordinator.preview(remote, "main", coordinator)
    assert preview.remote_sha != preview.best_sha
    assert preview.relationship == "remote_ahead"

    assert {:ok, requested} = SyncCoordinator.request(remote, "main", "sync-success", coordinator)
    assert requested.status == "requested"
    assert Pika.Persistence.current_campaign().dispatch_gate == "sync"

    eventually(fn ->
      match?({:ok, %{status: "completed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    {:ok, completed} = SyncStore.latest_run(context.campaign.id)
    assert completed.remote_after_sha == completed.candidate_sha
    assert completed.best_after_sha == completed.candidate_sha
    assert Git.run!(context.workspace.repo, ["rev-parse", "HEAD"]) == completed.candidate_sha

    assert {:ok, completed.candidate_sha} ==
             Pika.SyncWorkspace.remote_sha(context.workspace.repo, remote, "main")

    assert Pika.Persistence.current_campaign().best_sha == completed.candidate_sha
    assert Pika.Persistence.current_campaign().dispatch_gate == nil
    assert {:ok, %{state: "verified"}} = SyncStore.intent_for_run(completed.id)
    assert completed.trail_artifact_id
    refute File.exists?(Path.join(context.workspace.root, completed.worktree_relative_path))

    [[sync_revisions, intents]] =
      Repo.query!(
        "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ? AND cause = 'sync'), (SELECT COUNT(*) FROM operation_intents WHERE campaign_id = ? AND kind = 'sync')",
        [context.campaign.id, context.campaign.id]
      ).rows

    assert {sync_revisions, intents} == {1, 1}
  end

  test "protected input changes require explicit confirmation and rejection leaves Best unchanged" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    remote_commit(remote, "kernel/reference.py", "def reference(x): return x + 1\n")
    coordinator = start_sync(context)

    assert {:ok, _} = SyncCoordinator.request(remote, "main", "sync-protected", coordinator)

    eventually(fn ->
      match?(
        {:ok, %{status: "awaiting_spec_confirmation"}},
        SyncStore.latest_run(context.campaign.id)
      )
    end)

    {:ok, waiting} = SyncStore.latest_run(context.campaign.id)
    assert waiting.protected_paths == ["kernel/reference.py"]
    assert Pika.Persistence.current_campaign().status == "awaiting_spec_confirmation"

    assert {:ok, failed} =
             SyncCoordinator.confirm_spec(waiting.id, false, "reject-protected", coordinator)

    assert failed.status == "failed"
    assert Pika.Persistence.current_campaign().best_sha == context.best_sha
    assert Git.run!(context.workspace.repo, ["rev-parse", "HEAD"]) == context.best_sha
    assert Pika.Persistence.current_campaign().dispatch_gate == nil
  end

  test "remote-push/local-Best crash window recovers without duplicate Push or Best revision" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    advance_local_best(context, "LOCAL.md", "local candidate\n")
    {:ok, crash_count} = Agent.start_link(fn -> 0 end)

    coordinator =
      start_sync(context,
        after_remote_push: fn _run ->
          if Agent.get_and_update(crash_count, &{&1, &1 + 1}) == 0,
            do: {:error, :injected_crash},
            else: :ok
        end
      )

    assert {:ok, _} = SyncCoordinator.request(remote, "main", "sync-recovery", coordinator)

    eventually(fn ->
      match?({:ok, %{status: "completed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    {:ok, completed} = SyncStore.latest_run(context.campaign.id)
    assert Agent.get(crash_count, & &1) >= 1

    assert {:ok, completed.candidate_sha} ==
             Pika.SyncWorkspace.remote_sha(context.workspace.repo, remote, "main")

    [[revisions, intents]] =
      Repo.query!(
        "SELECT (SELECT COUNT(*) FROM best_revisions WHERE campaign_id = ? AND cause = 'sync'), (SELECT COUNT(*) FROM operation_intents WHERE campaign_id = ? AND kind = 'sync')",
        [context.campaign.id, context.campaign.id]
      ).rows

    assert {revisions, intents} == {1, 1}
  end

  test "Sync Agent resolves a real merge conflict on its temporary branch" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    remote_commit(remote, "README.md", "remote side\n")
    local_sha = advance_local_best(context, "README.md", "local side\n")
    coordinator = start_sync(context)

    assert {:ok, preview} = SyncCoordinator.preview(remote, "main", coordinator)
    assert preview.relationship == "diverged"
    assert {:ok, _} = SyncCoordinator.request(remote, "main", "sync-conflict", coordinator)

    eventually(fn ->
      match?({:ok, %{status: "completed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    {:ok, completed} = SyncStore.latest_run(context.campaign.id)

    parents =
      Git.run!(context.workspace.repo, [
        "rev-list",
        "--parents",
        "-n",
        "1",
        completed.candidate_sha
      ])

    assert length(String.split(parents)) == 3
    assert completed.base_sha == local_sha

    assert File.read!(Path.join(context.workspace.repo, "README.md")) ==
             "resolved by sync agent\n"
  end

  test "ordinary push rejection fails the Sync and keeps local Best unchanged" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    local_sha = advance_local_best(context, "LOCAL.md", "cannot publish\n")
    install_rejecting_hook(remote)
    coordinator = start_sync(context)

    assert {:ok, _} = SyncCoordinator.request(remote, "main", "sync-rejected-push", coordinator)

    eventually(fn ->
      match?({:ok, %{status: "failed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    {:ok, failed} = SyncStore.latest_run(context.campaign.id)
    assert Pika.Persistence.current_campaign().best_sha == local_sha
    assert Git.run!(context.workspace.repo, ["rev-parse", "HEAD"]) == local_sha

    assert {:ok, remote_sha} =
             Pika.SyncWorkspace.remote_sha(context.workspace.repo, remote, "main")

    assert remote_sha == failed.remote_before_sha
    assert {:ok, %{state: "aborted"}} = SyncStore.intent_for_run(failed.id)
    refute File.exists?(Path.join(context.workspace.root, failed.worktree_relative_path))
  end

  test "approved protected changes create a new Spec Revision and rebuild Baseline metrics" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 5)
    remote = create_remote(context.workspace.repo)
    remote_commit(remote, "kernel/reference.py", "def reference(x): return x + 1\n")
    coordinator = start_sync(context)

    assert {:ok, _} =
             SyncCoordinator.request(remote, "main", "sync-protected-approved", coordinator)

    eventually(fn ->
      match?(
        {:ok, %{status: "awaiting_spec_confirmation"}},
        SyncStore.latest_run(context.campaign.id)
      )
    end)

    {:ok, waiting} = SyncStore.latest_run(context.campaign.id)

    assert {:ok, %{status: "validating"}} =
             SyncCoordinator.confirm_spec(waiting.id, true, "approve-protected", coordinator)

    eventually(fn ->
      match?({:ok, %{status: "completed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    campaign = Pika.Persistence.current_campaign()
    refute campaign.current_spec_revision_id == context.spec_id

    [[revision, baseline_sha]] =
      Repo.query!(
        "SELECT revision, baseline_sha FROM spec_revisions WHERE id = ?",
        [campaign.current_spec_revision_id]
      ).rows

    assert revision == 2
    assert baseline_sha == campaign.best_sha

    [[source, improvement]] =
      Repo.query!(
        "SELECT bm.source, bm.improvement_ratio FROM best_metrics bm JOIN best_revisions br ON br.id = bm.best_revision_id WHERE br.campaign_id = ? AND br.cause = 'sync'",
        [context.campaign.id]
      ).rows

    assert source == "sync"
    assert improvement == 0.0
  end

  test "Pause preserves in-flight work, Stop interrupts it, Resume recovers, and exhausted work drains to Completed" do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    remote = create_remote(context.workspace.repo)
    remote_commit(remote, "README.md", "wait for control\n")
    parent = self()

    coordinator =
      start_sync(context,
        profile: sync_profile(%{test_pid: self(), barrier: parent})
      )

    assert {:ok, _} = SyncCoordinator.request(remote, "main", "sync-control", coordinator)
    assert_receive {:sync_waiting, task_pid}, 5_000

    assert {:ok, paused} = Control.pause("pause-once")
    assert paused.status == "paused"
    refute_receive {:sync_interrupted, _}, 100

    assert {:ok, stopped} = Control.stop_now("stop-once")
    assert stopped.status == "stopped"
    assert_receive {:sync_interrupted, _}, 2_000

    send(task_pid, :release)
    assert {:ok, resumed} = Control.resume("resume-once")
    assert resumed.status == "optimizing"
    assert_receive {:sync_waiting, resumed_task_pid}, 5_000
    send(resumed_task_pid, :release)

    eventually(fn ->
      match?({:ok, %{status: "completed"}}, SyncStore.latest_run(context.campaign.id))
    end)

    Repo.query!("UPDATE campaigns SET attempts_created = 1, status = 'optimizing' WHERE id = ?", [
      context.campaign.id
    ])

    assert {:ok, %{status: "draining"}} = Control.reconcile(context.campaign.id)
    assert {:ok, %{status: "completed"}} = Control.reconcile(context.campaign.id)
  end

  defp start_sync(context, opts \\ []) do
    profile = Keyword.get(opts, :profile, sync_profile(%{test_pid: self()}))

    {:ok, coordinator} =
      SyncCoordinator.start_link(
        workspace: context.workspace,
        campaign: Pika.Persistence.current_campaign(),
        profile: profile,
        backend_modules: %{codex_app_server: SyncAgentBackend},
        mcp_url: "http://127.0.0.1:18080/mcp",
        after_remote_push: Keyword.get(opts, :after_remote_push)
      )

    Process.unlink(coordinator)

    on_exit(fn ->
      if Process.alive?(coordinator), do: GenServer.stop(coordinator)
    end)

    coordinator
  end

  defp sync_profile(env) do
    %{
      "name" => "sync",
      "backend" => "codex_app_server",
      "command" => ["codex", "app-server", "--listen", "stdio://"],
      "reasoning_effort" => "high",
      "env" => env,
      "protocol_config" => %{}
    }
  end

  defp create_remote(repo) do
    remote = AlignmentFixtures.temp_dir("pika-sync-remote")
    Git.run!(repo, ["init", "--bare", remote])
    Git.run!(repo, ["push", remote, "HEAD:refs/heads/main"])
    remote
  end

  defp remote_commit(remote, relative_path, contents) do
    clone = AlignmentFixtures.temp_dir("pika-sync-clone")
    Git.run!(Path.dirname(clone), ["clone", "--branch", "main", remote, clone])
    Git.run!(clone, ["config", "user.name", "Pika Test"])
    Git.run!(clone, ["config", "user.email", "pika@example.invalid"])
    absolute = Path.join(clone, relative_path)
    File.mkdir_p!(Path.dirname(absolute))
    File.write!(absolute, contents)
    Git.run!(clone, ["add", relative_path])
    Git.run!(clone, ["commit", "-m", "Remote update"])
    Git.run!(clone, ["push", "origin", "main"])
  end

  defp advance_local_best(context, relative_path, contents) do
    absolute = Path.join(context.workspace.repo, relative_path)
    File.write!(absolute, contents)
    Git.run!(context.workspace.repo, ["add", relative_path])
    Git.run!(context.workspace.repo, ["commit", "-m", "Local Best update"])
    sha = Git.run!(context.workspace.repo, ["rev-parse", "HEAD"])
    now = System.system_time(:microsecond)

    Repo.query!("UPDATE campaigns SET best_sha = ?, updated_at = ? WHERE id = ?", [
      sha,
      now,
      context.campaign.id
    ])

    sha
  end

  defp install_rejecting_hook(remote) do
    hook = Path.join([remote, "hooks", "pre-receive"])
    File.write!(hook, "#!/bin/sh\nexit 1\n")
    File.chmod!(hook, 0o755)
  end

  defp eventually(fun, attempts \\ 240)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.() do
      :ok
    else
      Process.sleep(25)
      eventually(fun, attempts - 1)
    end
  end

  defp eventually(fun, 0), do: flunk("condition did not become true: #{inspect(fun.())}")
end
