defmodule Pika.Repo.Migrations.CreateCampaignCoreTables do
  use Ecto.Migration

  def change do
    create table(:campaigns, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:singleton_key, :integer,
        null: false,
        default: 1,
        check: %{name: "campaigns_singleton_key", expr: "singleton_key = 1"}
      )

      add(:status, :string,
        null: false,
        check: %{
          name: "campaigns_status",
          expr:
            "status IN ('drafting_spec','awaiting_confirmation','building_baseline','selecting_iteration_sample','optimizing','draining','completed','paused','blocked','stopped','awaiting_spec_confirmation')"
        }
      )
      add(:resume_state, :string)
      add(:dispatch_gate, :string)
      add(:workspace_mode, :string,
        null: false,
        check: %{
          name: "campaigns_workspace_mode",
          expr: "workspace_mode IN ('owned_repo','managed_repo')"
        }
      )
      add(:repo_relative_path, :string, null: false, default: "repo")
      add(:managed_repo_canonical_path, :string)
      add(:git_common_dir, :string, null: false)
      add(:base_sha, :string,
        null: false,
        check: %{
          name: "campaigns_base_sha",
          expr:
            "length(base_sha) IN (40,64) AND base_sha NOT GLOB '*[^0-9a-f]*'"
        }
      )

      add(:best_branch, :string, null: false, default: "pika/best")

      add(:best_sha, :string,
        null: false,
        check: %{
          name: "campaigns_best_sha",
          expr:
            "length(best_sha) IN (40,64) AND best_sha NOT GLOB '*[^0-9a-f]*'"
        }
      )
      add(:current_spec_revision_id, :string)
      add(:attempts_created, :integer,
        null: false,
        default: 0,
        check: %{
          name: "campaigns_nonnegative_counts",
          expr:
            "attempts_created >= 0 AND (max_attempts IS NULL OR max_attempts >= 0) AND history_limit >= 0"
        }
      )
      add(:max_attempts, :integer)
      add(:plan_enabled, :boolean, null: false, default: true)
      add(:history_limit, :integer, null: false, default: 10)
      add(:stop_mode, :string, null: false, default: "all_goals")
      add(:config_hash, :string,
        null: false,
        check: %{
          name: "campaigns_config_hash",
          expr: "length(config_hash) = 64 AND config_hash NOT GLOB '*[^0-9a-f]*'"
        }
      )
      add(:lock_version, :integer, null: false, default: 1)
      add(:inserted_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:campaigns, [:singleton_key]))
    create table(:artifacts, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:owner_type, :string, null: false)
      add(:owner_id, :string, null: false)
      add(:kind, :string, null: false)
      add(:relative_path, :string, null: false)
      add(:sha256, :string,
        null: false,
        check: %{
          name: "artifacts_sha256",
          expr: "length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'"
        }
      )

      add(:byte_size, :integer,
        null: false,
        check: %{name: "artifacts_size", expr: "byte_size >= 0"}
      )
      add(:mime_type, :string, null: false)
      add(:metadata_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:artifacts, [:relative_path]))
    create(index(:artifacts, [:owner_type, :owner_id, :kind]))
    create table(:operation_intents, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:kind, :string,
        null: false,
        check: %{
          name: "operation_intents_kind",
          expr: "kind IN ('merge','sync','cleanup','recovery')"
        }
      )
      add(:owner_type, :string, null: false)
      add(:owner_id, :string, null: false)
      add(:state, :string,
        null: false,
        check: %{
          name: "operation_intents_state",
          expr: "state IN ('pending','applied','verified','aborted')"
        }
      )
      add(:expected_best_sha, :string,
        check: %{
          name: "operation_intents_expected_best_sha",
          expr:
            "expected_best_sha IS NULL OR (length(expected_best_sha) IN (40,64) AND expected_best_sha NOT GLOB '*[^0-9a-f]*')"
        }
      )

      add(:target_sha, :string,
        check: %{
          name: "operation_intents_target_sha",
          expr:
            "target_sha IS NULL OR (length(target_sha) IN (40,64) AND target_sha NOT GLOB '*[^0-9a-f]*')"
        }
      )
      add(:idempotency_key, :string, null: false)
      add(:payload_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:operation_intents, [:idempotency_key]))

    create table(:domain_events, primary_key: false) do
      add(:sequence, :integer, primary_key: true, autogenerate: true)
      add(:event_id, :string, null: false)
      add(:aggregate_type, :string, null: false)
      add(:aggregate_id, :string, null: false)
      add(:event_type, :string, null: false)
      add(:payload_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:domain_events, [:event_id]))
    create(index(:domain_events, [:aggregate_type, :aggregate_id, :sequence]))

    create table(:agent_sessions, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:attempt_id, :string)
      add(:sync_run_id, :string)
      add(:role, :string, null: false)
      add(:slot_index, :integer)
      add(:profile_json, :text, null: false, default: "{}")
      add(:backend, :string, null: false)
      add(:backend_protocol, :string, null: false)
      add(:backend_version, :string)
      add(:provider_session_id, :string)
      add(:backend_capabilities_json, :text, null: false, default: "{}")
      add(:model, :string)
      add(:reasoning_effort, :string)
      add(:status, :string,
        null: false,
        check: %{
          name: "agent_sessions_status",
          expr:
            "status IN ('starting','running','awaiting_report','interrupted','completed','failed','stopped')"
        }
      )
      add(:process_pid, :string)
      add(:process_started_at, :integer)
      add(:mcp_token_hash, :string,
        check: %{
          name: "agent_sessions_mcp_token_hash",
          expr:
            "mcp_token_hash IS NULL OR (length(mcp_token_hash) = 64 AND mcp_token_hash NOT GLOB '*[^0-9a-f]*')"
        }
      )
      add(:log_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:required_operations_json, :text, null: false, default: "[]")
      add(:last_turn_sequence, :integer, null: false, default: 0)
      add(:last_event_seq, :integer, null: false, default: 0)
      add(:started_at, :integer, null: false)
      add(:ended_at, :integer)
    end

    create(index(:agent_sessions, [:status, :role]))
    create(index(:agent_sessions, [:attempt_id, :status]))

    create table(:idempotency_records, primary_key: false) do
      add(
        :backend_session_id,
        references(:agent_sessions, type: :string, on_delete: :delete_all),
        primary_key: true
      )

      add(:tool_name, :string, primary_key: true)
      add(:idempotency_key, :string, primary_key: true)
      add(:request_sha256, :string,
        null: false,
        check: %{
          name: "idempotency_records_request_hash",
          expr: "length(request_sha256) = 64 AND request_sha256 NOT GLOB '*[^0-9a-f]*'"
        }
      )
      add(:response_json, :text, null: false)
      add(:created_at, :integer, null: false)
    end

  end
end
