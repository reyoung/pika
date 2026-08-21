defmodule Pika.Repo.Migrations.CreateProgressSummaries do
  use Ecto.Migration

  def change do
    create table(:progress_summaries, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:phase, :string, null: false)
      add(:attempt_ids_json, :text, null: false, default: "[]")
      add(:backend, :string, null: false)
      add(:model, :string)
      add(:reasoning_effort, :string)
      add(:content, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create(index(:progress_summaries, [:campaign_id, :created_at]))
  end
end
