defmodule Pika.Repo.Migrations.CreateAgentFollowupRequests do
  use Ecto.Migration

  def change do
    create table(:agent_followup_requests, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all), null: false)
      add(:target_role, :string, null: false)
      add(:target_work_kind, :string, null: false)
      add(:target_work_id, :string, null: false)
      add(:target_session_id, references(:agent_sessions, type: :string, on_delete: :delete_all), null: false)
      add(:required_operations_json, :text, null: false)
      add(:context_json, :text, null: false)
      add(:message, :text)
      add(:status, :string, null: false)
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
      add(:completed_at, :integer)
      add(:delivered_at, :integer)
    end

    create(index(:agent_followup_requests, [:campaign_id, :status, :created_at]))

    create(
      unique_index(:agent_followup_requests, [:target_session_id],
        where: "status IN ('requested', 'running', 'completed')",
        name: :agent_followup_requests_one_pending
      )
    )
  end
end
