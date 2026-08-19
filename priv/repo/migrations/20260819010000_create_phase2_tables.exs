defmodule Pika.Repo.Migrations.CreatePhase2Tables do
  use Ecto.Migration

  def change do
    create table(:spec_revisions, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:revision, :integer, null: false)
      add(:status, :string,
        null: false,
        check: %{
          name: "spec_revisions_status",
          expr: "status IN ('draft','awaiting_confirmation','confirmed','superseded','rejected')"
        }
      )
      add(:spec_json, :text, null: false)
      add(:protected_paths_json, :text, null: false, default: "[]")
      add(:protected_digest, :string)
      add(:baseline_sha, :string)
      add(:reference_snapshot_json, :text, null: false, default: "[]")
      add(:skill_snapshot_json, :text, null: false, default: "[]")
      add(:confirmed_at, :integer)
      add(:inserted_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:spec_revisions, [:campaign_id, :revision]))

    create table(:benchmark_cases, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :delete_all),
        null: false
      )
      add(:ordinal, :integer, null: false)
      add(:name, :string, null: false)
      add(:kind, :string,
        null: false,
        check: %{
          name: "benchmark_cases_kind",
          expr: "kind IN ('target','guard','informational')"
        }
      )
      add(:shape_json, :text, null: false)
      add(:dtype_json, :text, null: false)
      add(:layout_json, :text, null: false)
      add(:distribution_json, :text)
      add(:frequency_weight, :float)
    end

    create(unique_index(:benchmark_cases, [:spec_revision_id, :ordinal]))
    create(unique_index(:benchmark_cases, [:spec_revision_id, :name]))

    create table(:metric_definitions, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :delete_all),
        null: false
      )
      add(:name, :string, null: false)
      add(:unit, :string, null: false)
      add(:direction, :string,
        null: false,
        check: %{
          name: "metric_definitions_direction",
          expr: "direction IN ('minimize','maximize')"
        }
      )
      add(:role, :string,
        null: false,
        check: %{
          name: "metric_definitions_role",
          expr: "role IN ('target','guard','informational')"
        }
      )
      add(:min_improvement_ratio, :float, null: false, default: 0.01)
      add(:parser_json, :text, null: false, default: "{}")
    end

    create(unique_index(:metric_definitions, [:spec_revision_id, :name]))

    create table(:sampling_revisions, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :restrict),
        null: false
      )
      add(:sequence, :integer, null: false)
      add(:cause, :string, null: false)
      add(:source_attempt_id, :string)
      add(:summary, :text, null: false)
      add(:estimated_cost_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:sampling_revisions, [:campaign_id, :spec_revision_id, :sequence]))

    create table(:sampling_revision_cases, primary_key: false) do
      add(:sampling_revision_id,
        references(:sampling_revisions, type: :string, on_delete: :delete_all),
        primary_key: true
      )
      add(:benchmark_case_id,
        references(:benchmark_cases, type: :string, on_delete: :restrict),
        primary_key: true
      )
      add(:reason, :text, null: false)
      add(:evidence_json, :text)
    end

    create table(:best_revisions, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :restrict), null: false)
      add(:sequence, :integer, null: false)
      add(:sha, :string, null: false)
      add(:cause, :string, null: false)
      add(:attempt_id, :string)
      add(:sync_run_id, :string)
      add(:spec_revision_id, references(:spec_revisions, type: :string, on_delete: :restrict),
        null: false
      )
      add(:summary, :text, null: false)
      add(:inserted_at, :integer, null: false)
    end

    create(unique_index(:best_revisions, [:campaign_id, :sequence]))
    create(unique_index(:best_revisions, [:campaign_id, :sha, :spec_revision_id]))

    create table(:best_metrics, primary_key: false) do
      add(:best_revision_id, references(:best_revisions, type: :string, on_delete: :delete_all),
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
      add(:improvement_ratio, :float, null: false, default: 0.0)
      add(:mad, :float, null: false)
      add(:noise_tolerance, :float, null: false)
      add(:pair_count, :integer, null: false)
      add(:valid_pair_count, :integer, null: false)
      add(:source, :string, null: false, default: "baseline")
      add(:measured_at, :integer, null: false)
    end

    # The normalized tables above are authoritative for domain queries. This compact
    # checkpoint preserves the in-flight conversation and Backend recovery cursor.
    create table(:campaign_runtime_snapshots, primary_key: false) do
      add(:campaign_id, references(:campaigns, type: :string, on_delete: :delete_all),
        primary_key: true
      )
      add(:state_blob, :binary, null: false)
      add(:updated_at, :integer, null: false)
    end
  end
end
