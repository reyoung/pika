defmodule Pika.Optimization.OperationReceiptsTest do
  use ExUnit.Case, async: false

  alias Pika.Optimization.{Config, OperationReceipts, Persistence}
  alias Pika.Repo

  setup do
    root =
      Path.join(System.tmp_dir!(), "pika-v2-receipts-#{System.unique_integer([:positive])}")

    repo = Path.join(root, "repo")
    workspace = Path.join(root, "workspace")
    File.mkdir_p!(repo)
    File.mkdir_p!(workspace)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(repo, workspace))
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
             Persistence.initialize_or_recover(config, String.duplicate("a", 40))

    counter = start_supervised!({Agent, fn -> 0 end})
    on_exit(fn -> File.rm_rf!(root) end)
    %{counter: counter}
  end

  test "replays the same canonical request and rejects key reuse for another request", %{
    counter: counter
  } do
    identity = %{role: "iteration", work_kind: :attempt, work_id: "42"}

    callback = fn ->
      value = Agent.get_and_update(counter, &{&1 + 1, &1 + 1})
      {:ok, %{attempt: 42, invocation: value}}
    end

    assert {:ok, %{"attempt" => 42, "invocation" => 1}, :executed} =
             OperationReceipts.run(
               identity,
               "finish_iteration",
               "same-key",
               %{b: 2, a: 1},
               callback
             )

    assert {:ok, %{"attempt" => 42, "invocation" => 1}, :replayed} =
             OperationReceipts.run(
               identity,
               "finish_iteration",
               "same-key",
               %{"a" => 1, "b" => 2},
               callback
             )

    assert Agent.get(counter, & &1) == 1

    assert {:error, :idempotency_conflict} =
             OperationReceipts.run(
               identity,
               "finish_iteration",
               "same-key",
               %{a: 2, b: 2},
               callback
             )
  end

  test "does not record failed commands", %{counter: counter} do
    identity = %{role: "integration", work_kind: :attempt, work_id: "7"}

    assert {:error, :not_ready} =
             OperationReceipts.run(identity, "finish_integration", "retry-key", %{}, fn ->
               {:error, :not_ready}
             end)

    assert {:ok, %{"ok" => true}, :executed} =
             OperationReceipts.run(identity, "finish_integration", "retry-key", %{}, fn ->
               Agent.update(counter, &(&1 + 1))
               {:ok, %{ok: true}}
             end)

    assert Agent.get(counter, & &1) == 1
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
