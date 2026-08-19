defmodule Pika.Repo.Migrations.CreateSyncControlTables do
  use Ecto.Migration

  def change do
    create table(:sync_runs, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:status, :string,
        null: false,
        check: %{
          name: "sync_runs_status",
          expr:
            "status IN ('requested','preparing','fetching','merging','awaiting_spec_confirmation','validating','pushing','advancing_best','completed','failed','blocked')"
        }
      )

      add(:remote, :string, null: false)
      add(:branch, :string, null: false)
      add(:sync_branch, :string, null: false)
      add(:worktree_relative_path, :string, null: false)
      add(:base_sha, :string, null: false)
      add(:remote_before_sha, :string, null: false)
      add(:candidate_sha, :string)
      add(:remote_after_sha, :string)
      add(:best_after_sha, :string)
      add(:protected_paths_json, :text, null: false, default: "[]")
      add(:protected_digest, :string)
      add(:validation_metrics_json, :text)
      add(:summary, :text)
      add(:failure_reason, :text)
      add(:correctness_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:metrics_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:trail_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:intent_id, references(:operation_intents, type: :string, on_delete: :nilify_all))
      add(:started_at, :integer, null: false)
      add(:completed_at, :integer)
    end

    create(index(:sync_runs, [:campaign_id, :status, :started_at]))

    create table(:control_actions, primary_key: false) do
      add(:idempotency_key, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:action, :string, null: false)
      add(:request_sha256, :string, null: false)
      add(:response_json, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create(index(:control_actions, [:campaign_id, :created_at]))
  end
end
