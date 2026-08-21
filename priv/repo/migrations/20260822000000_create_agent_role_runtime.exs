defmodule Pika.Repo.Migrations.CreateAgentRoleRuntime do
  use Ecto.Migration

  def change do
    alter table(:agent_sessions) do
      add(:work_kind, :string)
      add(:work_id, :string)
      add(:role_contract_revision, :integer)
      add(:session_mode, :string)
      add(:role_definition_sha256, :string)
      add(:template_sha256, :string)
      add(:instructions_sha256, :string)

      add(
        :instructions_artifact_id,
        references(:artifacts, type: :string, on_delete: :nilify_all)
      )
    end

    create(
      index(:agent_sessions, [:campaign_id, :role, :work_kind, :work_id, :status],
        name: :agent_sessions_role_work_status
      )
    )

    create table(:agent_operation_receipts, primary_key: false) do
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        primary_key: true
      )

      add(:role, :string, primary_key: true)
      add(:work_kind, :string, primary_key: true)
      add(:work_id, :string, primary_key: true)
      add(:operation, :string, primary_key: true)
      add(:idempotency_key, :string, primary_key: true)
      add(:request_sha256, :string, null: false)
      add(:response_json, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create(index(:agent_operation_receipts, [:campaign_id, :created_at]))

    alter table(:progress_summaries) do
      add(:status, :string, null: false, default: "completed")
      add(:context_json, :text, null: false, default: "{}")
      add(:failure_reason, :text)
      add(:started_at, :integer)
      add(:completed_at, :integer)
      add(:updated_at, :integer)
    end

    create(
      index(:progress_summaries, [:campaign_id, :status, :created_at],
        name: :progress_summaries_campaign_status
      )
    )

    create(
      unique_index(:progress_summaries, [:campaign_id],
        where: "status IN ('requested', 'running')",
        name: :progress_summaries_one_active_request
      )
    )
  end
end
