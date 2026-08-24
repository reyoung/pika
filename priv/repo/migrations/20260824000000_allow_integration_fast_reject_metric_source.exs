defmodule Pika.Repo.Migrations.AllowIntegrationFastRejectMetricSource do
  use Ecto.Migration

  def up do
    execute("""
    CREATE TABLE attempt_metrics_source_upgrade (
      attempt_id TEXT CONSTRAINT attempt_metrics_attempt_id_fkey REFERENCES attempts(id) ON DELETE CASCADE,
      benchmark_case_id TEXT CONSTRAINT attempt_metrics_benchmark_case_id_fkey REFERENCES benchmark_cases(id) ON DELETE RESTRICT,
      metric_definition_id TEXT CONSTRAINT attempt_metrics_metric_definition_id_fkey REFERENCES metric_definitions(id) ON DELETE RESTRICT,
      measured_sha TEXT NOT NULL,
      value NUMERIC NOT NULL,
      baseline_value NUMERIC NOT NULL,
      improvement_ratio NUMERIC NOT NULL,
      mad NUMERIC NOT NULL,
      noise_tolerance NUMERIC NOT NULL,
      pair_count INTEGER NOT NULL,
      valid_pair_count INTEGER NOT NULL,
      source TEXT NOT NULL CONSTRAINT attempt_metrics_source CHECK (source IN ('iteration','integration_screen','integration_full','integration_fast_reject')),
      measured_at INTEGER NOT NULL,
      target_snapshot_id TEXT CONSTRAINT attempt_metrics_target_snapshot_id_fkey REFERENCES target_snapshots(id) ON DELETE RESTRICT,
      target_value NUMERIC,
      target_relative_improvement NUMERIC,
      best_relative_improvement NUMERIC,
      PRIMARY KEY (attempt_id, benchmark_case_id, metric_definition_id)
    )
    """)

    execute("""
    INSERT INTO attempt_metrics_source_upgrade (
      attempt_id, benchmark_case_id, metric_definition_id, measured_sha,
      value, baseline_value, improvement_ratio, mad, noise_tolerance,
      pair_count, valid_pair_count, source, measured_at, target_snapshot_id,
      target_value, target_relative_improvement, best_relative_improvement
    )
    SELECT
      attempt_id, benchmark_case_id, metric_definition_id, measured_sha,
      value, baseline_value, improvement_ratio, mad, noise_tolerance,
      pair_count, valid_pair_count, source, measured_at, target_snapshot_id,
      target_value, target_relative_improvement, best_relative_improvement
    FROM attempt_metrics
    """)

    execute("DROP TABLE attempt_metrics")
    execute("ALTER TABLE attempt_metrics_source_upgrade RENAME TO attempt_metrics")
  end
end
