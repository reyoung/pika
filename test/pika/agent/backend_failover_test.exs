Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Agent.BackendFailoverTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.{BackendFailover, ConversationJournal, Work}
  alias Pika.AgentBackend.Failure
  alias Pika.Optimization.{Config, ConfigFile, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-backend-failover-#{System.unique_integer([:positive])}")

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

    on_exit(fn -> File.rm_rf!(root) end)

    %{work: %Work{role_id: "integration", kind: :optimization, id: "one"}}
  end

  test "advances a three-endpoint chain in order, isolates Works, and persists a blocked chain",
       %{
         work: work
       } do
    agent = chain()
    other_work = %{work | id: "two"}

    assert {:ok, primary} = BackendFailover.select(agent, work, 100)
    assert primary.chain_index == 0
    assert primary.endpoint.model == "primary"
    assert {:ok, untouched} = BackendFailover.select(agent, other_work, 100)
    assert untouched.chain_index == 0

    primary_session = start_session(work, primary)

    assert :ok =
             BackendFailover.block(
               work,
               primary,
               Failure.new(:capacity_exhausted, "primary quota", code: "UsageLimitExceeded"),
               primary_session.id
             )

    assert ConversationJournal.session(primary_session.id).status == "interrupted"
    assert ConversationJournal.session(primary_session.id).ended_reason =~ "backend_failover"
    assert {:ok, second} = BackendFailover.select(agent, work, 100)
    assert {second.chain_index, second.endpoint.model} == {1, "second"}

    second_session = start_session(work, second)

    assert :ok =
             BackendFailover.block(
               work,
               second,
               Failure.new(:authentication_failed, "login expired", code: "Unauthorized"),
               second_session.id
             )

    assert {:ok, third} = BackendFailover.select(agent, work, 100)
    assert {third.chain_index, third.endpoint.model} == {2, "third"}

    third_session = start_session(work, third)

    assert :ok =
             BackendFailover.block(
               work,
               third,
               Failure.new(:capacity_exhausted, "last quota"),
               third_session.id
             )

    assert {:blocked, blocked} = BackendFailover.select(agent, work, 100)
    assert blocked.chain_length == 3
    assert Enum.map(blocked.failures, & &1.endpoint_index) == [0, 1, 2]

    assert Enum.map(blocked.failures, & &1.category) == [
             "capacity_exhausted",
             "authentication_failed",
             "capacity_exhausted"
           ]

    # Selection is reconstructed entirely from SQLite, so the same assertion also covers restart recovery.
    assert [snapshot] = BackendFailover.blocked_works(100)

    assert Map.take(snapshot, [:role, :work_kind, :work_id, :chain_length]) == %{
             role: "integration",
             work_kind: "optimization",
             work_id: "one",
             chain_length: 3
           }

    assert {:ok, still_untouched} = BackendFailover.select(agent, other_work, 100)
    assert still_untouched.chain_index == 0

    assert :ok = BackendFailover.retry(work, agent)
    assert {:ok, retried} = BackendFailover.select(agent, work, 100)
    assert retried.chain_index == 0
  end

  test "reset time automatically re-enables an endpoint and chain changes start at primary", %{
    work: work
  } do
    agent = chain()
    assert {:ok, primary} = BackendFailover.select(agent, work, 100)
    session = start_session(work, primary)

    assert :ok =
             BackendFailover.block(
               work,
               primary,
               Failure.new(:capacity_exhausted, "temporary quota", retry_at: 110),
               session.id
             )

    assert {:ok, second} = BackendFailover.select(agent, work, 109)
    assert second.chain_index == 1

    assert {:ok, reset_primary} = BackendFailover.select(agent, work, 110)
    assert reset_primary.chain_index == 0

    session = start_session(work, reset_primary)

    assert :ok =
             BackendFailover.block(
               work,
               reset_primary,
               Failure.new(:authentication_failed, "expired credential"),
               session.id
             )

    changed = %{agent | model: "new-primary-model"}
    assert {:ok, changed_primary} = BackendFailover.select(changed, work, 111)
    assert changed_primary.chain_index == 0
    assert changed_primary.endpoint.model == "new-primary-model"
    refute changed_primary.chain_sha256 == primary.chain_sha256
  end

  test "non-eligible failures never exclude the current endpoint", %{work: work} do
    agent = chain()
    assert {:ok, primary} = BackendFailover.select(agent, work, 100)
    session = start_session(work, primary)

    assert {:error, :failure_not_eligible_for_failover} =
             BackendFailover.block(
               work,
               primary,
               Failure.new(:transient, "network disconnected"),
               session.id
             )

    assert {:ok, selected_again} = BackendFailover.select(agent, work, 100)
    assert selected_again.chain_index == 0
    assert ConversationJournal.session(session.id).status == "running"
  end

  defp start_session(work, selection) do
    assert {:ok, session} =
             ConversationJournal.start_session(
               work.role_id,
               work.kind,
               work.id,
               Config.Agent.snapshot(selection.endpoint),
               "system",
               "context",
               backend_chain_sha256: selection.chain_sha256,
               backend_chain_index: selection.chain_index
             )

    assert session.backend_chain_sha256 == selection.chain_sha256
    assert session.backend_chain_index == selection.chain_index
    session
  end

  defp chain do
    primary = ConfigFile.agent("codex", "primary", "high")
    second = ConfigFile.agent("cursor-headless", "second", "high")
    third = ConfigFile.agent("codex", "third", "medium")
    %{primary | fallbacks: [second, third]}
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
      integration:
        backend: codex
        approval_policy: never
        sandbox: workspace-write
    """
  end
end
