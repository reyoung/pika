defmodule Pika.Repo.Migrations.SeparateOptimizationTarget do
  use Ecto.Migration

  def change do
    create table(:target_snapshots, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)

      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:source_kind, :string,
        null: false,
        check: %{
          name: "target_snapshots_source_kind",
          expr: "source_kind IN ('development_snapshot','reference_project')"
        }
      )

      add(:source_reference_id, :string)
      add(:source_sha, :string, null: false)
      add(:tree_sha, :string, null: false)
      add(:entrypoint, :text, null: false)
      add(:digest, :string, null: false)
      add(:checkout_relative_path, :text, null: false)
      add(:inserted_at, :integer, null: false)
    end

    create(unique_index(:target_snapshots, [:spec_revision_id]))
    create(unique_index(:target_snapshots, [:campaign_id, :digest]))

    alter table(:spec_revisions) do
      add(:target_snapshot_id, references(:target_snapshots, type: :string, on_delete: :restrict))
      add(:development_baseline_sha, :string)
      add(:implementation_manifest_json, :text, null: false, default: "{}")
    end

    alter table(:best_metrics) do
      add(:target_snapshot_id, references(:target_snapshots, type: :string, on_delete: :restrict))
      add(:target_value, :float)
      add(:target_relative_improvement, :float)
      add(:best_relative_improvement, :float)
    end

    alter table(:attempt_metrics) do
      add(:target_snapshot_id, references(:target_snapshots, type: :string, on_delete: :restrict))
      add(:target_value, :float)
      add(:target_relative_improvement, :float)
      add(:best_relative_improvement, :float)
    end
  end
end
