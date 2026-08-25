defmodule Pika.Repo.Migrations.CreateConversationTimelineItems do
  use Ecto.Migration

  def change do
    create table(:conversation_timeline_items, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(
        :turn_id,
        references(:conversation_turns, type: :integer, on_delete: :delete_all),
        null: false
      )

      add(:sequence, :integer, null: false)
      add(:kind, :string, null: false)
      add(:item_index, :integer, null: false)
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:conversation_timeline_items, [:turn_id, :sequence]))
    create(unique_index(:conversation_timeline_items, [:turn_id, :kind, :item_index]))
  end
end
