package symphony

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
)

func persistMeasurementDefinition(ctx context.Context, tx *sql.Tx, baselineID string, definition benchmarkintegrity.MeasurementDefinition) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM benchmark_metric_definitions WHERE baseline_revision_id = ?`, baselineID); err != nil {
		return fmt.Errorf("replace benchmark metric definitions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM benchmark_case_weights WHERE baseline_revision_id = ?`, baselineID); err != nil {
		return fmt.Errorf("replace benchmark case weights: %w", err)
	}
	for ordinal, item := range definition.Cases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_case_weights
			(baseline_revision_id, case_id, weight, ordinal) VALUES (?, ?, ?, ?)`, baselineID, item.CaseID, item.Weight, ordinal); err != nil {
			return fmt.Errorf("persist benchmark case weight: %w", err)
		}
	}
	for _, metric := range definition.Metrics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_metric_definitions
			(baseline_revision_id, metric_id, label, unit, role, direction, sample_statistic, aggregation)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, baselineID, metric.ID, metric.Label, metric.Unit, metric.Role, metric.Direction, metric.SampleStatistic, metric.Aggregation); err != nil {
			return fmt.Errorf("persist benchmark metric definition: %w", err)
		}
	}
	return nil
}

func persistBaselineMeasurementSet(ctx context.Context, tx *sql.Tx, baselineID, workID, now string, set benchmarkintegrity.MeasurementSet) error {
	setID := baselineID + ":development-baseline"
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_measurement_sets
		(id, baseline_revision_id, work_id, best_sequence, kind, created_at) VALUES (?, ?, ?, 0, 'development_baseline', ?)`, setID, baselineID, workID, now); err != nil {
		return fmt.Errorf("persist Development Baseline measurement set: %w", err)
	}
	for _, item := range set.Cases {
		for metricID, value := range item.Values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_case_values
				(measurement_set_id, case_id, metric_id, value) VALUES (?, ?, ?, ?)`, setID, item.CaseID, metricID, value); err != nil {
				return fmt.Errorf("persist Development Baseline value: %w", err)
			}
		}
	}
	return nil
}

func persistExperimentMeasurements(ctx context.Context, tx *sql.Tx, baselineID, workID, experimentID, receiptID, scopeBestSHA, now string, comparison benchmarkintegrity.MeasurementComparison) error {
	for _, kind := range []string{"reference", "candidate"} {
		setID := experimentID + ":" + kind
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_measurement_sets
			(id, baseline_revision_id, work_id, experiment_id, receipt_id, scope_best_sha, kind, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, setID, baselineID, workID, experimentID, receiptID, scopeBestSHA, kind, now); err != nil {
			return fmt.Errorf("persist Experiment %s measurement set: %w", kind, err)
		}
		for _, item := range comparison.Cases {
			for metricID, value := range item.Metrics {
				measured := value.Reference
				if kind == "candidate" {
					measured = value.Candidate
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_case_values
					(measurement_set_id, case_id, metric_id, value) VALUES (?, ?, ?, ?)`, setID, item.CaseID, metricID, measured); err != nil {
					return fmt.Errorf("persist Experiment %s value: %w", kind, err)
				}
			}
		}
	}
	for _, item := range comparison.Cases {
		for metricID, value := range item.Metrics {
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_experiment_derived_comparisons
				(experiment_id, case_id, metric_id, reference_value, candidate_value, speedup, regression, regression_fraction, receipt_id, scope_best_sha)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, experimentID, item.CaseID, metricID, value.Reference, value.Candidate, value.Speedup, value.Regression, value.RegressionFraction, receiptID, scopeBestSHA); err != nil {
				return fmt.Errorf("persist Experiment benchmark comparison: %w", err)
			}
		}
	}
	for metricID, value := range comparison.Metrics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_experiment_derived_comparisons
			(experiment_id, case_id, metric_id, speedup, regression, regression_fraction, aggregate_speedup, max_case_speedup, max_case_id, receipt_id, scope_best_sha)
			VALUES (?, '', ?, ?, 0, 0, ?, ?, ?, ?, ?)`, experimentID, metricID, value.AggregateSpeedup, value.AggregateSpeedup, value.MaxCaseSpeedup, value.MaxCaseID, receiptID, scopeBestSHA); err != nil {
			return fmt.Errorf("persist Experiment benchmark aggregate comparison: %w", err)
		}
	}
	return nil
}

func persistReferenceMeasurementSet(ctx context.Context, tx *sql.Tx, baselineID, workID, referenceReceiptID, scopeBestSHA, now string, set benchmarkintegrity.MeasurementSet) error {
	setID := referenceReceiptID + ":reference"
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_measurement_sets
		(id, baseline_revision_id, work_id, receipt_id, scope_best_sha, kind, created_at)
		VALUES (?, ?, ?, ?, ?, 'reference', ?)`, setID, baselineID, workID, referenceReceiptID, scopeBestSHA, now); err != nil {
		return fmt.Errorf("persist Experiment Cycle reference measurement set: %w", err)
	}
	return persistMeasurementSetValues(ctx, tx, setID, set)
}

func persistFlowV3ExperimentMeasurements(ctx context.Context, tx *sql.Tx, baselineID, workID, experimentID, experimentReceiptID, referenceReceiptID, scopeBestSHA, now string, candidate benchmarkintegrity.MeasurementSet, comparison benchmarkintegrity.MeasurementComparison) error {
	if _, err := tx.ExecContext(ctx, `UPDATE benchmark_measurement_sets SET experiment_id = ? WHERE receipt_id = ? AND kind = 'reference'`, experimentID, referenceReceiptID); err != nil {
		return fmt.Errorf("link reference measurement set to Experiment: %w", err)
	}
	setID := experimentID + ":candidate"
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_measurement_sets
		(id, baseline_revision_id, work_id, experiment_id, receipt_id, scope_best_sha, kind, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'candidate', ?)`, setID, baselineID, workID, experimentID, experimentReceiptID, scopeBestSHA, now); err != nil {
		return fmt.Errorf("persist flow-v3 candidate measurement set: %w", err)
	}
	if err := persistMeasurementSetValues(ctx, tx, setID, candidate); err != nil {
		return err
	}
	for _, item := range comparison.Cases {
		for metricID, value := range item.Metrics {
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_experiment_derived_comparisons
				(experiment_id, case_id, metric_id, reference_value, candidate_value, speedup, regression, regression_fraction, receipt_id, scope_best_sha)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, experimentID, item.CaseID, metricID, value.Reference, value.Candidate, value.Speedup, value.Regression, value.RegressionFraction, experimentReceiptID, scopeBestSHA); err != nil {
				return fmt.Errorf("persist flow-v3 Experiment comparison: %w", err)
			}
		}
	}
	for metricID, value := range comparison.Metrics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_experiment_derived_comparisons
			(experiment_id, case_id, metric_id, speedup, regression, regression_fraction, aggregate_speedup, max_case_speedup, max_case_id, receipt_id, scope_best_sha)
			VALUES (?, '', ?, ?, 0, 0, ?, ?, ?, ?, ?)`, experimentID, metricID, value.AggregateSpeedup, value.AggregateSpeedup, value.MaxCaseSpeedup, value.MaxCaseID, experimentReceiptID, scopeBestSHA); err != nil {
			return fmt.Errorf("persist flow-v3 Experiment aggregate comparison: %w", err)
		}
	}
	return nil
}

func persistMeasurementSetValues(ctx context.Context, tx *sql.Tx, setID string, set benchmarkintegrity.MeasurementSet) error {
	for _, item := range set.Cases {
		for metricID, value := range item.Values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_case_values
				(measurement_set_id, case_id, metric_id, value) VALUES (?, ?, ?, ?)`, setID, item.CaseID, metricID, value); err != nil {
				return fmt.Errorf("persist measurement set value: %w", err)
			}
		}
	}
	return nil
}

func persistIntegrationMeasurements(ctx context.Context, tx *sql.Tx, baselineID, workID, integrationID, experimentID, receiptID, scopeBestSHA, now string, comparison benchmarkintegrity.MeasurementComparison) error {
	experimentRef := nullable(experimentID)
	for _, kind := range []string{"reference", "candidate"} {
		setID := integrationID + ":" + kind
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_measurement_sets
			(id, baseline_revision_id, work_id, integration_id, experiment_id, receipt_id, scope_best_sha, kind, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, setID, baselineID, workID, integrationID, experimentRef, receiptID, scopeBestSHA, kind, now); err != nil {
			return fmt.Errorf("persist Integration %s measurement set: %w", kind, err)
		}
		for _, item := range comparison.Cases {
			for metricID, value := range item.Metrics {
				measured := value.Reference
				if kind == "candidate" {
					measured = value.Candidate
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_case_values
					(measurement_set_id, case_id, metric_id, value) VALUES (?, ?, ?, ?)`, setID, item.CaseID, metricID, measured); err != nil {
					return fmt.Errorf("persist Integration %s value: %w", kind, err)
				}
			}
		}
	}
	for _, item := range comparison.Cases {
		for metricID, value := range item.Metrics {
			if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_derived_comparisons
				(integration_id, experiment_id, case_id, metric_id, reference_value, candidate_value, speedup, regression, regression_fraction, receipt_id, scope_best_sha)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, integrationID, experimentRef, item.CaseID, metricID, value.Reference, value.Candidate, value.Speedup, value.Regression, value.RegressionFraction, receiptID, scopeBestSHA); err != nil {
				return fmt.Errorf("persist benchmark comparison: %w", err)
			}
		}
	}
	for metricID, value := range comparison.Metrics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_derived_comparisons
			(integration_id, experiment_id, case_id, metric_id, speedup, regression, regression_fraction, aggregate_speedup, max_case_speedup, max_case_id, receipt_id, scope_best_sha)
			VALUES (?, ?, '', ?, ?, 0, 0, ?, ?, ?, ?, ?)`, integrationID, experimentRef, metricID, value.AggregateSpeedup, value.AggregateSpeedup, value.MaxCaseSpeedup, value.MaxCaseID, receiptID, scopeBestSHA); err != nil {
			return fmt.Errorf("persist benchmark aggregate comparison: %w", err)
		}
	}
	return nil
}
