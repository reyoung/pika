defmodule Pika.Integration.LifecycleTest do
  use ExUnit.Case, async: false

  @moduletag :lifecycle

  alias Pika.Integration.Lifecycle
  alias Pika.Test.OptimizationFixtures
  alias Pika.{AttemptStore, Repo}

  setup do
    context = OptimizationFixtures.setup_campaign(max_attempts: 1)
    {:ok, attempt} = AttemptStore.create_attempt(context.campaign.id, 0)

    Repo.query!(
      "UPDATE attempts SET status = 'ready_for_integration', candidate_sha = ? WHERE id = ?",
      [context.best_sha, attempt.id]
    )

    on_exit(&OptimizationFixtures.stop_repo/0)

    session_id = Ecto.UUID.generate()
    insert_session(context, attempt, session_id)

    identity = %Lifecycle.Identity{
      campaign_id: context.campaign.id,
      attempt_id: attempt.id,
      session_id: session_id
    }

    %{context: context, identity: identity}
  end

  test "projects the next required Integration operation from committed facts", %{
    identity: identity
  } do
    assert {:ok,
            %Lifecycle.Projection{
              decision: %Lifecycle.Decision{operation: :acquire_integration_lease},
              state: %Lifecycle.State{
                attempt_status: "ready_for_integration",
                campaign_status: "optimizing"
              }
            }} = Lifecycle.project(identity)
  end

  test "acquires the Integration Lease through the lifecycle interface", %{
    context: context,
    identity: identity
  } do
    assert {:ok, projection} = Lifecycle.project(identity)

    command = %Lifecycle.Command{
      operation: :acquire_integration_lease,
      facts_revision: projection.revision,
      idempotency_key: "lease-#{identity.attempt_id}",
      params: %{expected_best_sha: context.best_sha}
    }

    assert {:ok, lease} = Lifecycle.execute(identity, command, context.workspace)
    assert is_binary(lease["id"])

    assert {:ok,
            %Lifecycle.Projection{
              decision: %Lifecycle.Decision{operation: :submit_full_regression},
              state: %Lifecycle.State{lease_session_id: lease_session_id}
            }} = Lifecycle.project(identity)

    assert lease_session_id == identity.session_id
  end

  test "replays a committed command but rejects a new command based on stale facts", %{
    context: context,
    identity: identity
  } do
    assert {:ok, projection} = Lifecycle.project(identity)

    command = %Lifecycle.Command{
      operation: :acquire_integration_lease,
      facts_revision: projection.revision,
      idempotency_key: "lease-#{identity.attempt_id}",
      params: %{expected_best_sha: context.best_sha}
    }

    assert {:ok, first} = Lifecycle.execute(identity, command, context.workspace)
    assert {:ok, replayed} = Lifecycle.execute(identity, command, context.workspace)
    assert replayed["id"] == first["id"]

    conflicting = %{
      command
      | params: %{expected_best_sha: String.duplicate("0", String.length(context.best_sha))}
    }

    assert {:error, :idempotency_conflict} =
             Lifecycle.execute(identity, conflicting, context.workspace)

    stale_command = %{command | idempotency_key: "different-request"}

    assert {:error,
            {:stale_integration_facts, %{expected: current_revision, actual: original_revision}}} =
             Lifecycle.execute(identity, stale_command, context.workspace)

    assert current_revision > original_revision

    invalid_command = %{stale_command | idempotency_key: nil}

    assert {:error, :invalid_integration_command} =
             Lifecycle.execute(identity, invalid_command, context.workspace)
  end

  test "projects the Lease, Refresh, Receipt, Intent, Merge and terminal matrix", %{
    context: context,
    identity: identity
  } do
    assert {:ok, projection} = Lifecycle.project(identity)

    assert {:ok, lease} =
             Lifecycle.execute(
               identity,
               %Lifecycle.Command{
                 operation: :acquire_integration_lease,
                 facts_revision: projection.revision,
                 idempotency_key: "matrix-lease-#{identity.attempt_id}",
                 params: %{expected_best_sha: context.best_sha}
               },
               context.workspace
             )

    assert next_operation(identity) == :submit_full_regression

    recovering_identity = %{identity | session_id: Ecto.UUID.generate()}
    assert next_operation(recovering_identity) == :acquire_integration_lease

    Repo.query!("UPDATE attempts SET base_sha = ? WHERE id = ?", [
      String.duplicate("a", 40),
      identity.attempt_id
    ])

    assert next_operation(identity) == :complete_refresh

    Repo.query!("UPDATE attempts SET base_sha = ? WHERE id = ?", [
      context.best_sha,
      identity.attempt_id
    ])

    receipt_id = insert_receipt(context, identity, lease["id"], "rejected")
    assert next_operation(identity) == :reject_attempt

    Repo.query!("UPDATE full_regression_receipts SET status = 'passed' WHERE id = ?", [receipt_id])

    assert next_operation(identity) == :create_merge_intent

    intent_id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO operation_intents(id, campaign_id, kind, owner_type, owner_id, state, expected_best_sha, target_sha, idempotency_key, payload_json, created_at, updated_at) VALUES (?, ?, 'merge', 'attempt', ?, 'pending', ?, ?, ?, '{}', ?, ?)",
      [
        intent_id,
        identity.campaign_id,
        identity.attempt_id,
        context.best_sha,
        context.best_sha,
        "matrix-intent-#{identity.attempt_id}",
        now,
        now
      ]
    )

    Repo.query!("UPDATE integration_leases SET intent_id = ? WHERE id = ?", [
      intent_id,
      lease["id"]
    ])

    assert next_operation(identity) == :complete_merge

    Repo.query!("UPDATE campaigns SET status = 'blocked' WHERE id = ?", [identity.campaign_id])
    assert next_operation(identity) == nil

    Repo.query!("UPDATE campaigns SET status = 'optimizing' WHERE id = ?", [identity.campaign_id])
    assert {:ok, fallback_projection} = Lifecycle.project(identity)

    assert {:ok, rejected} =
             Lifecycle.execute(
               identity,
               %Lifecycle.Command{
                 operation: :reject_stalled_attempt,
                 facts_revision: fallback_projection.revision,
                 idempotency_key: "reject-stalled-#{identity.attempt_id}",
                 params: %{reason: "unclassified Integration failure with unchanged Best"}
               },
               context.workspace
             )

    assert rejected["status"] == "rejected"
    assert next_operation(identity) == nil
  end

  test "recovers an existing Lease into a replacement Agent session", %{
    context: context,
    identity: identity
  } do
    assert {:ok, projection} = Lifecycle.project(identity)

    command = %Lifecycle.Command{
      operation: :acquire_integration_lease,
      facts_revision: projection.revision,
      idempotency_key: "recover-lease-#{identity.attempt_id}",
      params: %{expected_best_sha: context.best_sha}
    }

    assert {:ok, original} = Lifecycle.execute(identity, command, context.workspace)

    replacement_session_id = Ecto.UUID.generate()
    {:ok, attempt} = AttemptStore.attempt(identity.attempt_id)
    insert_session(context, attempt, replacement_session_id)
    replacement = %{identity | session_id: replacement_session_id}

    assert {:ok, recovered} = Lifecycle.execute(replacement, command, context.workspace)
    assert recovered["id"] == original["id"]
    assert next_operation(replacement) == :submit_full_regression

    assert {:ok, lease} = Pika.IntegrationStore.lease(identity.campaign_id)
    assert lease.backend_session_id == replacement_session_id
  end

  defp next_operation(identity) do
    {:ok, projection} = Lifecycle.project(identity)
    projection.decision.operation
  end

  defp insert_receipt(context, identity, lease_id, status) do
    artifact_id = Ecto.UUID.generate()
    receipt_id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)

    Repo.query!(
      "INSERT INTO artifacts(id, campaign_id, owner_type, owner_id, kind, relative_path, sha256, byte_size, mime_type, metadata_json, created_at) VALUES (?, ?, 'attempt', ?, 'test', ?, ?, 0, 'application/json', '{}', ?)",
      [
        artifact_id,
        identity.campaign_id,
        identity.attempt_id,
        "artifacts/lifecycle/#{artifact_id}.json",
        String.duplicate("f", 64),
        now
      ]
    )

    Repo.query!(
      "INSERT INTO full_regression_receipts(id, campaign_id, attempt_id, lease_id, base_sha, candidate_sha, harness_digest, status, regressed_case_ids_json, metrics_json, correctness_artifact_id, screening_artifact_id, full_artifact_id, issued_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', '[]', ?, ?, ?, ?)",
      [
        receipt_id,
        identity.campaign_id,
        identity.attempt_id,
        lease_id,
        context.best_sha,
        context.best_sha,
        String.duplicate("d", 64),
        status,
        artifact_id,
        artifact_id,
        artifact_id,
        now
      ]
    )

    receipt_id
  end

  defp insert_session(context, attempt, session_id) do
    session = %Pika.AgentBackend.Session{
      id: session_id,
      backend: :codex_app_server,
      backend_protocol: "lifecycle-test",
      backend_session_id: "provider-#{session_id}",
      cwd: context.workspace.repo,
      model: "test",
      reasoning_effort: :high,
      jsonl_path: "/dev/null"
    }

    :ok =
      AttemptStore.insert_session(
        context.campaign.id,
        %{
          attempt_id: attempt.id,
          sync_run_id: nil,
          role: :integration,
          slot_index: nil,
          token_hash: String.duplicate("f", 64)
        },
        session,
        %{},
        ["acquire_integration_lease"]
      )

    Repo.query!("UPDATE agent_sessions SET work_kind = 'attempt', work_id = ? WHERE id = ?", [
      attempt.id,
      session_id
    ])
  end
end
