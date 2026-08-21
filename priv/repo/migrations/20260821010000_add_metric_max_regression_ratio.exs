defmodule Pika.Repo.Migrations.AddMetricMaxRegressionRatio do
  use Ecto.Migration

  def change do
    alter table(:metric_definitions) do
      add(:max_regression_ratio, :float, null: false, default: 0.0)
    end
  end
end
