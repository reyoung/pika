defmodule Pika.Repo.Migrations.ScopeIntegrationRuns do
  use Ecto.Migration

  def up do
    alter table(:integration_runs) do
      add(:run_sequence, :integer, null: false, default: 1)
    end

    drop(unique_index(:integration_runs, [:attempt_id]))
    create(unique_index(:integration_runs, [:attempt_id, :run_sequence]))

    # The legacy retry flow reused the rejected row. Preserve that row as the
    # terminal first run; the runtime will allocate a fresh row for the retry.
    execute("""
    UPDATE integration_runs
    SET status = 'rejected', outcome = 'rejected'
    WHERE status = 'retry_requested' AND result_artifact_id IS NOT NULL
    """)
  end

  def down do
    drop(unique_index(:integration_runs, [:attempt_id, :run_sequence]))
    create(unique_index(:integration_runs, [:attempt_id]))

    alter table(:integration_runs) do
      remove(:run_sequence)
    end
  end
end
