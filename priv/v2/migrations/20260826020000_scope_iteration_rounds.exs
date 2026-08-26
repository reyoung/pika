defmodule Pika.Repo.Migrations.ScopeIterationRounds do
  use Ecto.Migration

  def up do
    alter table(:iteration_rounds) do
      add(:work_relative_path, :text)
      add(:branch, :text)
      add(:candidate_sha, :text)
    end

    execute("""
    UPDATE iteration_rounds
    SET work_relative_path = (
          SELECT attempts.work_relative_path
          FROM attempts
          WHERE attempts.id = iteration_rounds.attempt_id
        ),
        branch = (
          SELECT attempts.branch
          FROM attempts
          WHERE attempts.id = iteration_rounds.attempt_id
        )
    """)
  end

  def down do
    alter table(:iteration_rounds) do
      remove(:candidate_sha)
      remove(:branch)
      remove(:work_relative_path)
    end
  end
end
