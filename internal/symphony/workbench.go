package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WorkbenchRecords returns normalized read models. Callers never need to know
// the SQLite schema or infer metrics from raw evidence JSON.
func (e *Engine) WorkbenchRecords(ctx context.Context) (WorkbenchRecords, error) {
	var records WorkbenchRecords
	metricRows, err := e.db.QueryContext(ctx, `SELECT baseline_revision_id, metric_id, label, unit, role, direction, sample_statistic, aggregation
		FROM benchmark_metric_definitions ORDER BY baseline_revision_id, metric_id`)
	if err != nil {
		return records, fmt.Errorf("read Workbench metric definitions: %w", err)
	}
	for metricRows.Next() {
		var item BenchmarkMetricDefinitionView
		if err := metricRows.Scan(&item.BaselineRevisionID, &item.MetricID, &item.Label, &item.Unit, &item.Role, &item.Direction, &item.SampleStatistic, &item.Aggregation); err != nil {
			_ = metricRows.Close()
			return records, err
		}
		records.Metrics = append(records.Metrics, item)
	}
	if err := metricRows.Close(); err != nil {
		return records, err
	}

	caseRows, err := e.db.QueryContext(ctx, `SELECT baseline_revision_id, case_id, weight, ordinal FROM benchmark_case_weights ORDER BY baseline_revision_id, ordinal`)
	if err != nil {
		return records, err
	}
	for caseRows.Next() {
		var item BenchmarkCaseWeightView
		if err := caseRows.Scan(&item.BaselineRevisionID, &item.CaseID, &item.Weight, &item.Ordinal); err != nil {
			_ = caseRows.Close()
			return records, err
		}
		records.CaseWeights = append(records.CaseWeights, item)
	}
	if err := caseRows.Close(); err != nil {
		return records, err
	}

	setRows, err := e.db.QueryContext(ctx, `SELECT id, baseline_revision_id, work_id, integration_id, experiment_id, receipt_id, scope_best_sha, best_sequence, kind, created_at FROM benchmark_measurement_sets ORDER BY rowid`)
	if err != nil {
		return records, err
	}
	for setRows.Next() {
		var item BenchmarkMeasurementSetView
		var workID, integrationID, experimentID, receiptID, scopeBestSHA sql.NullString
		var bestSequence sql.NullInt64
		if err := setRows.Scan(&item.ID, &item.BaselineRevisionID, &workID, &integrationID, &experimentID, &receiptID, &scopeBestSHA, &bestSequence, &item.Kind, &item.CreatedAt); err != nil {
			_ = setRows.Close()
			return records, err
		}
		item.WorkID, item.IntegrationID = workID.String, integrationID.String
		item.ExperimentID, item.ReceiptID, item.ScopeBestSHA = experimentID.String, receiptID.String, scopeBestSHA.String
		if bestSequence.Valid {
			value := bestSequence.Int64
			item.BestSequence = &value
		}
		records.MeasurementSets = append(records.MeasurementSets, item)
	}
	if err := setRows.Close(); err != nil {
		return records, err
	}

	valueRows, err := e.db.QueryContext(ctx, `SELECT measurement_set_id, case_id, metric_id, value FROM benchmark_case_values ORDER BY measurement_set_id, case_id, metric_id`)
	if err != nil {
		return records, err
	}
	for valueRows.Next() {
		var item BenchmarkCaseValueView
		if err := valueRows.Scan(&item.MeasurementSetID, &item.CaseID, &item.MetricID, &item.Value); err != nil {
			_ = valueRows.Close()
			return records, err
		}
		records.CaseValues = append(records.CaseValues, item)
	}
	if err := valueRows.Close(); err != nil {
		return records, err
	}

	comparisonRows, err := e.db.QueryContext(ctx, `SELECT integration_id, experiment_id, receipt_id, scope_best_sha, case_id, metric_id, reference_value, candidate_value, speedup,
		regression, regression_fraction, aggregate_speedup, max_case_speedup, max_case_id FROM benchmark_derived_comparisons ORDER BY integration_id, metric_id, case_id`)
	if err != nil {
		return records, err
	}
	for comparisonRows.Next() {
		var item BenchmarkComparisonView
		var reference, candidate, aggregate, maxCase sql.NullFloat64
		var regression int
		var experimentID, receiptID, scopeBestSHA, maxCaseID sql.NullString
		if err := comparisonRows.Scan(&item.IntegrationID, &experimentID, &receiptID, &scopeBestSHA, &item.CaseID, &item.MetricID, &reference, &candidate, &item.Speedup,
			&regression, &item.RegressionFraction, &aggregate, &maxCase, &maxCaseID); err != nil {
			_ = comparisonRows.Close()
			return records, err
		}
		item.Regression = regression != 0
		item.ExperimentID, item.ReceiptID, item.ScopeBestSHA = experimentID.String, receiptID.String, scopeBestSHA.String
		if reference.Valid {
			value := reference.Float64
			item.ReferenceValue = &value
		}
		if candidate.Valid {
			value := candidate.Float64
			item.CandidateValue = &value
		}
		if aggregate.Valid {
			value := aggregate.Float64
			item.AggregateSpeedup = &value
		}
		if maxCase.Valid {
			value := maxCase.Float64
			item.MaxCaseSpeedup = &value
		}
		item.MaxCaseID = maxCaseID.String
		records.Comparisons = append(records.Comparisons, item)
	}
	if err := comparisonRows.Close(); err != nil {
		return records, err
	}
	experimentComparisonRows, err := e.db.QueryContext(ctx, `SELECT experiment_id, receipt_id, scope_best_sha, case_id, metric_id, reference_value, candidate_value, speedup,
		regression, regression_fraction, aggregate_speedup, max_case_speedup, max_case_id
		FROM benchmark_experiment_derived_comparisons ORDER BY experiment_id, metric_id, case_id`)
	if err != nil {
		return records, err
	}
	for experimentComparisonRows.Next() {
		var item BenchmarkComparisonView
		var reference, candidate, aggregate, maxCase sql.NullFloat64
		var maxCaseID sql.NullString
		var regression int
		if err := experimentComparisonRows.Scan(&item.ExperimentID, &item.ReceiptID, &item.ScopeBestSHA, &item.CaseID, &item.MetricID,
			&reference, &candidate, &item.Speedup, &regression, &item.RegressionFraction, &aggregate, &maxCase, &maxCaseID); err != nil {
			_ = experimentComparisonRows.Close()
			return records, err
		}
		item.Regression = regression != 0
		if reference.Valid {
			value := reference.Float64
			item.ReferenceValue = &value
		}
		if candidate.Valid {
			value := candidate.Float64
			item.CandidateValue = &value
		}
		if aggregate.Valid {
			value := aggregate.Float64
			item.AggregateSpeedup = &value
		}
		if maxCase.Valid {
			value := maxCase.Float64
			item.MaxCaseSpeedup = &value
		}
		item.MaxCaseID = maxCaseID.String
		records.Comparisons = append(records.Comparisons, item)
	}
	if err := experimentComparisonRows.Close(); err != nil {
		return records, err
	}

	artifactRows, err := e.db.QueryContext(ctx, `SELECT id, work_id, receipt_id, relative_path, byte_size, content_sha256, contract_version FROM evidence_artifacts ORDER BY created_at, id`)
	if err != nil {
		return records, err
	}
	for artifactRows.Next() {
		var item EvidenceArtifact
		var receiptID sql.NullString
		if err := artifactRows.Scan(&item.ID, &item.WorkID, &receiptID, &item.RelativePath, &item.ByteSize, &item.ContentSHA256, &item.ContractVersion); err != nil {
			_ = artifactRows.Close()
			return records, err
		}
		item.ReceiptID = receiptID.String
		records.Artifacts = append(records.Artifacts, item)
	}
	if err := artifactRows.Close(); err != nil {
		return records, err
	}
	if err := e.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM benchmark_measurement_sets) + (SELECT COUNT(*) FROM benchmark_case_values) +
		(SELECT COUNT(*) FROM benchmark_derived_comparisons) + (SELECT COUNT(*) FROM benchmark_experiment_derived_comparisons)`).Scan(&records.MeasurementSequence); err != nil {
		return records, err
	}
	return records, nil
}

func (e *Engine) EvidenceArtifact(ctx context.Context, artifactID string) (EvidenceArtifact, error) {
	var artifact EvidenceArtifact
	var receiptID sql.NullString
	if err := e.db.QueryRowContext(ctx, `SELECT id, work_id, receipt_id, relative_path, byte_size, content_sha256, contract_version
		FROM evidence_artifacts WHERE id = ?`, artifactID).Scan(&artifact.ID, &artifact.WorkID, &receiptID, &artifact.RelativePath, &artifact.ByteSize, &artifact.ContentSHA256, &artifact.ContractVersion); errors.Is(err, sql.ErrNoRows) {
		return EvidenceArtifact{}, domainError(CodeInvalidCommand, "evidence artifact was not found")
	} else if err != nil {
		return EvidenceArtifact{}, err
	}
	artifact.ReceiptID = receiptID.String
	return artifact, nil
}
