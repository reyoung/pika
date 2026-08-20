defmodule Pika.PersistenceArtifactTest do
  use ExUnit.Case, async: false

  import Ecto.Query

  alias Pika.Persistence.{AgentSession, Campaign, DomainEvent}
  alias Pika.Test.CampaignFixtures
  alias Pika.{ArtifactStore, CampaignStore, Config, Git, Persistence, Repo, Workspace}

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

  test "creates the singleton Campaign and required WAL schema", %{
    workspace: workspace,
    campaign: campaign
  } do
    assert campaign.status == "drafting_spec"
    assert campaign.workspace_mode == "owned_repo"
    assert campaign.config_hash == workspace.config_hash

    assert Persistence.pragma_values() == %{
             "foreign_keys" => 1,
             "journal_mode" => "wal",
             "synchronous" => 2,
             "busy_timeout" => 5_000
           }

    assert {:ok, recovered, :recovered} = Persistence.initialize_or_recover(workspace)
    assert recovered.id == campaign.id
    assert Repo.aggregate(Campaign, :count) == 1

    tables =
      Repo.query!("SELECT name FROM sqlite_master WHERE type='table'").rows
      |> List.flatten()

    for table <-
          ~w(campaigns artifacts operation_intents domain_events agent_sessions idempotency_records) do
      assert table in tables
    end
  end

  test "recovers after Best advances while preserving the historical Campaign Base", %{
    workspace: workspace,
    campaign: campaign
  } do
    Git.run!(workspace.repo, ["config", "user.name", "Pika Test"])
    Git.run!(workspace.repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(workspace.repo, "candidate.txt"), "accepted\n")
    Git.run!(workspace.repo, ["add", "candidate.txt"])
    Git.run!(workspace.repo, ["commit", "-m", "advance best"])
    advanced_best = Git.run!(workspace.repo, ["rev-parse", "HEAD"])

    {1, _} =
      Campaign
      |> where([persisted], persisted.id == ^campaign.id)
      |> Repo.update_all(
        set: [best_sha: advanced_best, status: "blocked", resume_state: "optimizing"]
      )

    restarted_workspace = %{workspace | base_sha: advanced_best}

    assert {:ok, recovered, :recovered} =
             Persistence.initialize_or_recover(restarted_workspace)

    assert recovered.base_sha == campaign.base_sha
    assert recovered.best_sha == advanced_best
    assert recovered.status == "optimizing"
    assert is_nil(recovered.resume_state)
  end

  test "upgrades an existing Campaign status constraint without losing state", %{
    campaign: campaign
  } do
    [[sql]] =
      Repo.query!("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'campaigns'").rows

    old_sql =
      sql
      |> String.replace(~r/CREATE TABLE\s+["`]?campaigns["`]?/i, "CREATE TABLE campaigns_old")
      |> String.replace(",'selecting_iteration_sample'", "")

    columns =
      Repo.query!("PRAGMA table_info(campaigns)").rows
      |> Enum.map(fn [_cid, name | _] -> ~s("#{name}") end)
      |> Enum.join(", ")

    Repo.query!("PRAGMA foreign_keys = OFF")

    assert {:ok, _} =
             Repo.transaction(fn ->
               Repo.query!(old_sql)

               Repo.query!(
                 "INSERT INTO campaigns_old (#{columns}) SELECT #{columns} FROM campaigns"
               )

               Repo.query!("DROP TABLE campaigns")
               Repo.query!("ALTER TABLE campaigns_old RENAME TO campaigns")

               Repo.query!(
                 "CREATE UNIQUE INDEX campaigns_singleton_key_index ON campaigns(singleton_key)"
               )
             end)

    Repo.query!("PRAGMA foreign_keys = ON")

    refute Repo.query!("SELECT sql FROM sqlite_master WHERE name = 'campaigns'").rows |> inspect() =~
             "selecting_iteration_sample"

    assert :ok = Persistence.migrate()
    assert Repo.get!(Campaign, campaign.id).id == campaign.id

    assert Repo.query!(
             "UPDATE campaigns SET status = 'selecting_iteration_sample' WHERE id = ?",
             [campaign.id]
           ).num_rows == 1
  end

  test "commits state and Domain Event together and broadcasts only after commit", %{
    campaign: campaign
  } do
    Phoenix.PubSub.subscribe(Pika.PubSub, Persistence.topic(campaign.id))
    before_count = Repo.aggregate(DomainEvent, :count)

    assert {:error, :campaign, changeset} =
             Persistence.transition_campaign(
               campaign,
               %{status: "not-a-state"},
               "invalid_transition",
               %{}
             )

    refute changeset.valid?
    assert Repo.aggregate(DomainEvent, :count) == before_count
    refute_receive {:domain_event, _event}, 100

    assert {:ok, paused, event} =
             Persistence.transition_campaign(
               campaign,
               %{status: "paused"},
               "campaign_paused",
               %{}
             )

    assert paused.status == "paused"
    assert event.event_type == "campaign_paused"
    assert Repo.aggregate(DomainEvent, :count) == before_count + 1
    assert_receive {:domain_event, ^event}
  end

  test "atomically writes and registers file metadata by recomputing hash", %{
    workspace: workspace,
    campaign: campaign
  } do
    attrs = artifact_attrs(campaign, %{sha256: String.duplicate("0", 64)})

    assert {:ok, artifact} =
             ArtifactStore.write(workspace, "artifacts/plans/plan.json", "{\"ok\":true}\n", attrs)

    expected = :crypto.hash(:sha256, "{\"ok\":true}\n") |> Base.encode16(case: :lower)
    assert artifact.sha256 == expected
    refute artifact.sha256 == attrs.sha256
    assert artifact.byte_size == 12
    assert artifact.relative_path == "artifacts/plans/plan.json"
    assert :ok = ArtifactStore.verify_all(workspace)

    columns = Repo.query!("PRAGMA table_info(artifacts)").rows
    refute Enum.any?(columns, fn [_cid, _name, type | _] -> String.upcase(type) == "BLOB" end)

    File.write!(Path.join(workspace.root, artifact.relative_path), "tampered")

    assert {:error, {:artifact_verification_failed, [failure]}} =
             ArtifactStore.verify_all(workspace)

    assert failure.artifact_id == artifact.id
    assert failure.path == artifact.relative_path
  end

  test "rejects noncanonical, escaping, absolute, and symlink Artifact paths", %{
    workspace: workspace,
    campaign: campaign
  } do
    attrs = artifact_attrs(campaign)

    assert {:error, reason} =
             ArtifactStore.write(workspace, "artifacts/logs/../escape", "x", attrs)

    assert reason in [:noncanonical_artifact_path, :artifact_path_escape]

    assert {:error, :absolute_artifact_path} =
             ArtifactStore.write(workspace, Path.join(workspace.root, "escape"), "x", attrs)

    assert {:error, :outside_artifact_root} =
             ArtifactStore.write(workspace, "repo/escape", "x", attrs)

    outside = CampaignFixtures.workspace()
    File.ln_s!(outside, Path.join(workspace.artifacts, "logs/link"))

    assert {:error, {:artifact_symlink_forbidden, _}} =
             ArtifactStore.write(workspace, "artifacts/logs/link/escape", "x", attrs)
  end

  test "drops only a trailing partial JSONL line and keeps prior records", %{
    workspace: workspace,
    campaign: campaign
  } do
    attrs = artifact_attrs(campaign, %{kind: "backend_log"})
    path = "artifacts/logs/session.jsonl"
    assert {:ok, _} = ArtifactStore.append_jsonl(workspace, path, %{sequence: 1}, attrs)
    absolute = Path.join(workspace.root, path)
    File.write!(absolute, "{\"sequence\":", [:append])

    assert :ok = ArtifactStore.verify_all(workspace)
    assert {:ok, [%{"sequence" => 1}]} = ArtifactStore.replay_jsonl(workspace, path)
    assert {:ok, _} = ArtifactStore.append_jsonl(workspace, path, %{sequence: 2}, attrs)

    assert {:ok, [%{"sequence" => 1}, %{"sequence" => 2}]} =
             ArtifactStore.replay_jsonl(workspace, path)

    assert :ok = ArtifactStore.verify_all(workspace)
  end

  test "migrates legacy Workspace-local Artifacts and runtime references without deleting the source",
       %{
         workspace: workspace,
         campaign: campaign
       } do
    artifact_id = Ecto.UUID.generate()
    legacy_path = "setup/1/reference_review_evidence.json"
    source = Path.join(workspace.root, legacy_path)
    contents = "{\"latency_us\":12.5}\n"
    sha256 = :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
    now = System.system_time(:microsecond)
    File.mkdir_p!(Path.dirname(source))
    File.write!(source, contents)

    {1, _} =
      Repo.insert_all("artifacts", [
        %{
          id: artifact_id,
          campaign_id: campaign.id,
          owner_type: "campaign",
          owner_id: campaign.id,
          kind: "reference_review_evidence",
          relative_path: legacy_path,
          sha256: sha256,
          byte_size: byte_size(contents),
          mime_type: "application/json",
          metadata_json: "{}",
          created_at: now
        }
      ])

    durable = %{
      status: :optimizing,
      artifacts: %{
        legacy_path => %{
          id: "legacy-runtime-artifact",
          kind: "reference_review_evidence",
          relative_path: legacy_path,
          sha256: sha256,
          size: byte_size(contents),
          mime: "application/json",
          metadata: %{}
        }
      },
      reference_review_evidence: %{output_artifact: legacy_path},
      messages: [%{at: DateTime.utc_now(), attachments: [%{relative_path: legacy_path}]}]
    }

    blob = :erlang.term_to_binary(durable, compressed: 6)

    Repo.query!(
      "INSERT INTO campaign_runtime_snapshots(campaign_id, state_blob, updated_at) VALUES (?, ?, ?)",
      [campaign.id, {:blob, blob}, now]
    )

    assert :ok = ArtifactStore.migrate_legacy_paths(workspace)

    [[recovered_path]] =
      Repo.query!("SELECT relative_path FROM artifacts WHERE id = ?", [artifact_id]).rows

    assert recovered_path ==
             "artifacts/recovered/#{artifact_id}/reference_review_evidence.json"

    assert File.read!(Path.join(workspace.root, recovered_path)) == contents
    assert File.read!(source) == contents
    assert :ok = ArtifactStore.verify_all(workspace)
    assert :ok = ArtifactStore.migrate_legacy_paths(workspace)

    assert {:ok, restored} = CampaignStore.load(campaign.id)
    assert Map.has_key?(restored.artifacts, recovered_path)
    refute Map.has_key?(restored.artifacts, legacy_path)
    assert restored.artifacts[recovered_path].relative_path == recovered_path
    assert restored.reference_review_evidence.output_artifact == recovered_path

    assert get_in(restored, [:messages, Access.at(0), :attachments, Access.at(0), :relative_path]) ==
             recovered_path

    assert %DateTime{} = hd(restored.messages).at
  end

  test "recovers lost Agent sessions once with a stable idempotency record", %{campaign: campaign} do
    session_id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)

    {1, _} =
      Repo.insert_all("agent_sessions", [
        %{
          id: session_id,
          campaign_id: campaign.id,
          role: "iteration",
          profile_json: "{}",
          backend: "codex_app_server",
          backend_protocol: "codex_app_server",
          backend_capabilities_json: "{}",
          status: "running",
          process_pid: "999999",
          process_started_at: now,
          required_operations_json: "[]",
          last_turn_sequence: 0,
          last_event_seq: 0,
          started_at: now
        }
      ])

    assert {:ok, [_event]} = Persistence.recover_agent_sessions(campaign.id)
    assert {:ok, []} = Persistence.recover_agent_sessions(campaign.id)
    assert Repo.get!(AgentSession, session_id).status == "interrupted"

    assert Repo.query!("SELECT count(*) FROM idempotency_records").rows == [[1]]

    count =
      Repo.aggregate(
        from(event in DomainEvent, where: event.event_type == "agent_session_interrupted"),
        :count
      )

    assert count == 1
  end

  test "records a Blocked recovery action once with an idempotency key", %{campaign: campaign} do
    before_events = Repo.aggregate(DomainEvent, :count)
    reason = {:artifact_verification_failed, "fixture"}

    assert {:ok, blocked, {:blocked, ^reason}} = Persistence.block_campaign(campaign, reason)
    assert blocked.status == "blocked"
    assert {:ok, same, {:blocked, ^reason}} = Persistence.block_campaign(blocked, reason)
    assert same.id == blocked.id

    assert Repo.aggregate(DomainEvent, :count) == before_events + 1

    assert Repo.query!("SELECT kind, state, count(*) FROM operation_intents GROUP BY kind, state").rows ==
             [["recovery", "verified", 1]]

    assert {:ok, resumed, _event} =
             Persistence.transition_campaign(
               blocked,
               %{status: "optimizing", resume_state: nil},
               "test_recovery_resumed",
               %{}
             )

    assert {:ok, reblocked, {:blocked, ^reason}} =
             Persistence.block_campaign(resumed, reason)

    assert reblocked.status == "blocked"
    assert reblocked.resume_state == "optimizing"

    blocked_events =
      Repo.aggregate(
        from(event in DomainEvent, where: event.event_type == "campaign_blocked"),
        :count
      )

    assert blocked_events == 1
  end

  defp artifact_attrs(campaign, overrides \\ %{}) do
    Map.merge(
      %{
        campaign_id: campaign.id,
        owner_type: "campaign",
        owner_id: campaign.id,
        kind: "plan",
        metadata: %{}
      },
      overrides
    )
  end
end
