defmodule Pika.Repo.Migrations.CreateAttemptLoopTables do
  use Ecto.Migration

  def change do
    create table(:attempts, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:ordinal, :integer, null: false)
      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:slot_index, :integer,
        null: false,
        check: %{name: "attempts_nonnegative_slot", expr: "slot_index >= 0"}
      )

      add(:sampling_revision_id,
        references(:sampling_revisions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:status, :string,
        null: false,
        check: %{
          name: "attempts_status",
          expr:
            "status IN ('queued','running','awaiting_report','ready_for_integration','refreshing','integrating','interrupted','accepted','rejected','cancelled')"
        }
      )

      add(:resume_state, :string)
      add(:base_sha, :string,
        null: false,
        check: %{
          name: "attempts_base_sha",
          expr: "length(base_sha) IN (40,64) AND base_sha NOT GLOB '*[^0-9a-f]*'"
        }
      )

      add(:candidate_sha, :string,
        check: %{
          name: "attempts_candidate_sha",
          expr:
            "candidate_sha IS NULL OR (length(candidate_sha) IN (40,64) AND candidate_sha NOT GLOB '*[^0-9a-f]*')"
        }
      )

      add(:branch_name, :string, null: false)
      add(:worktree_relative_path, :string, null: false)
      add(:description, :text)
      add(:summary, :text)
      add(:modification_scope_json, :text, null: false, default: "[]")
      add(:risk_json, :text, null: false, default: "[]")
      add(:profiler_summary, :text)
      add(:recommended_outcome, :string)
      add(:outcome_reason, :text)
      add(:patch_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:plan_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:correctness_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:metrics_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:accepted_sha, :string,
        check: %{
          name: "attempts_accepted_sha",
          expr:
            "accepted_sha IS NULL OR (length(accepted_sha) IN (40,64) AND accepted_sha NOT GLOB '*[^0-9a-f]*')"
        }
      )

      add(:created_at, :integer, null: false)
      add(:started_at, :integer)
      add(:completed_at, :integer)
      add(:lock_version, :integer, null: false, default: 1)
    end

    create(unique_index(:attempts, [:campaign_id, :ordinal]))
    create(unique_index(:attempts, [:branch_name]))
    create(unique_index(:attempts, [:worktree_relative_path]))
    create(index(:attempts, [:campaign_id, :status, :ordinal]))

    create table(:attempt_metrics, primary_key: false) do
      add(:attempt_id, references(:attempts, type: :string, on_delete: :delete_all),
        primary_key: true
      )

      add(:benchmark_case_id, references(:benchmark_cases, type: :string, on_delete: :restrict),
        primary_key: true
      )

      add(:metric_definition_id,
        references(:metric_definitions, type: :string, on_delete: :restrict),
        primary_key: true
      )

      add(:measured_sha, :string, null: false)
      add(:value, :float, null: false)
      add(:baseline_value, :float, null: false)
      add(:improvement_ratio, :float, null: false)
      add(:mad, :float, null: false)
      add(:noise_tolerance, :float, null: false)
      add(:pair_count, :integer, null: false)
      add(:valid_pair_count, :integer, null: false)
      add(:source, :string,
        null: false,
        check: %{
          name: "attempt_metrics_source",
          expr: "source IN ('iteration','integration_screen','integration_full')"
        }
      )

      add(:measured_at, :integer, null: false)
    end

    create table(:agent_messages, primary_key: false) do
      add(:sequence, :integer, primary_key: true, autogenerate: true)
      add(:id, :string, null: false)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:from_session_id, references(:agent_sessions, type: :string, on_delete: :nilify_all))
      add(:to_session_id, references(:agent_sessions, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:scope, :string, null: false, default: "direct")
      add(:body, :text, null: false)
      add(:priority, :string,
        null: false,
        default: "normal",
        check: %{
          name: "agent_messages_priority",
          expr: "priority IN ('normal','high')"
        }
      )

      add(:status, :string,
        null: false,
        default: "queued",
        check: %{
          name: "agent_messages_status",
          expr: "status IN ('queued','delivered','acknowledged')"
        }
      )

      add(:created_at, :integer, null: false)
      add(:delivered_at, :integer)
      add(:acknowledged_at, :integer)
    end

    create(unique_index(:agent_messages, [:id]))
    create(index(:agent_messages, [:to_session_id, :status, :sequence]))

    create table(:guidance, primary_key: false) do
      add(:sequence, :integer, primary_key: true, autogenerate: true)
      add(:id, :string, null: false)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:kind, :string,
        null: false,
        check: %{
          name: "guidance_kind",
          expr: "kind IN ('campaign','attempt','side')"
        }
      )

      add(:attempt_id, references(:attempts, type: :string, on_delete: :delete_all))
      add(:parent_session_id, references(:agent_sessions, type: :string, on_delete: :nilify_all))
      add(:body, :text, null: false)
      add(:status, :string,
        null: false,
        default: "unread",
        check: %{
          name: "guidance_status",
          expr: "status IN ('unread','injected','acknowledged')"
        }
      )

      add(:created_at, :integer, null: false)
      add(:injected_at, :integer)
    end

    create(unique_index(:guidance, [:id]))
    create(index(:guidance, [:campaign_id, :kind, :sequence]))
    create(index(:guidance, [:attempt_id, :status, :sequence]))
  end
end
