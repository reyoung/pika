defmodule Pika.Repo.Migrations.CreateIntegrationTables do
  use Ecto.Migration

  def change do
    create table(:full_regression_receipts, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:attempt_id, references(:attempts, type: :string, on_delete: :restrict), null: false)
      add(:lease_id, :string, null: false)
      add(:base_sha, :string, null: false)
      add(:candidate_sha, :string, null: false)
      add(:harness_digest, :string, null: false)
      add(:status, :string,
        null: false,
        check: %{
          name: "full_regression_receipts_status",
          expr: "status IN ('passed','rejected')"
        }
      )

      add(:regressed_case_ids_json, :text, null: false, default: "[]")
      add(:metrics_json, :text, null: false)
      add(:correctness_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )

      add(:screening_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )

      add(:full_artifact_id, references(:artifacts, type: :string, on_delete: :restrict))
      add(:issued_at, :integer, null: false)
    end

    create(unique_index(:full_regression_receipts, [:lease_id]))

    create table(:integration_leases, primary_key: false) do
      add(:singleton_key, :integer,
        primary_key: true,
        default: 1,
        check: %{name: "integration_leases_singleton", expr: "singleton_key = 1"}
      )

      add(:id, :string, null: false)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:backend_session_id,
        references(:agent_sessions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:attempt_id, references(:attempts, type: :string, on_delete: :restrict), null: false)
      add(:intent_id, references(:operation_intents, type: :string, on_delete: :restrict))
      add(:expected_best_sha, :string, null: false)
      add(:state, :string, null: false, default: "lease_acquired")
      add(:acquired_at, :integer, null: false)
    end

    create(unique_index(:integration_leases, [:id]))
    create(unique_index(:integration_leases, [:attempt_id]))
  end
end
