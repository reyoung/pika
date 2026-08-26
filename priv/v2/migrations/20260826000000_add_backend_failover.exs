defmodule Pika.Repo.Migrations.AddBackendFailover do
  use Ecto.Migration

  def change do
    alter table(:agent_sessions) do
      add(:backend_chain_sha256, :string)
      add(:backend_chain_index, :integer, null: false, default: 0)
    end

    create table(:agent_backend_failures, primary_key: false) do
      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:role, :string, null: false)
      add(:work_kind, :string, null: false)
      add(:work_id, :string, null: false)
      add(:chain_sha256, :string, null: false)
      add(:chain_length, :integer, null: false)
      add(:endpoint_index, :integer, null: false)
      add(:backend, :string, null: false)
      add(:category, :string, null: false)
      add(:provider_code, :string)
      add(:message, :text, null: false)
      add(:retry_at, :integer)
      add(:details_json, :text, null: false, default: "{}")
      add(:active, :boolean, null: false, default: true)
      add(:failed_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
      add(:cleared_at, :integer)
    end

    create(
      unique_index(:agent_backend_failures, [
        :optimization_id,
        :role,
        :work_kind,
        :work_id,
        :chain_sha256,
        :endpoint_index
      ])
    )

    create(
      index(:agent_backend_failures, [
        :optimization_id,
        :role,
        :work_kind,
        :work_id,
        :active
      ])
    )
  end
end
