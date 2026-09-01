package symphony

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
	"github.com/reyoung/pika-go/internal/kdacontract"
)

// ReplayIterationExperiment resolves an already committed MCP request from
// durable domain state before callers inspect mutable Git or artifact files.
// Artifact metadata is deliberately absent from this lookup: the canonical
// client identity is Work + request key + raw Experiment contract.
func (e *Engine) ReplayIterationExperiment(ctx context.Context, workID, requestID string, experiment json.RawMessage) (Receipt, bool, error) {
	if workID == "" || requestID == "" || len(experiment) == 0 || !json.Valid(experiment) {
		return Receipt{}, false, domainError(CodeInvalidCommand, "work_id, request_id, and a valid experiment are required")
	}
	var receipt Receipt
	var storedExperiment, storedWorkID sql.NullString
	err := e.db.QueryRowContext(ctx, `SELECT r.receipt_id, r.command_type, r.revision, r.result_json,
		CAST(e.experiment_json AS TEXT),
		(SELECT w.id FROM works w WHERE w.role = 'iteration' AND w.attempt_id = e.attempt_id AND w.iteration_round = e.iteration_round LIMIT 1)
		FROM operation_receipts r
		LEFT JOIN iteration_experiments e ON e.receipt_id = r.receipt_id
		WHERE r.request_id = ?`, requestID).Scan(&receipt.ID, &receipt.Command, &receipt.Revision, &receipt.Result, &storedExperiment, &storedWorkID)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read Experiment receipt replay: %w", err)
	}
	if receipt.Command != (RecordIterationExperiment{}).commandName() {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used for a different command")
	}
	if !storedExperiment.Valid || !storedWorkID.Valid {
		return Receipt{}, false, domainError(CodeStateCorrupt, "Experiment receipt has no persisted Work or contract")
	}
	requestedCanonical, err := canonicalContractJSON(experiment)
	if err != nil {
		return Receipt{}, false, domainError(CodeInvalidCommand, err.Error())
	}
	storedCanonical, err := canonicalContractJSON(json.RawMessage(storedExperiment.String))
	if err != nil {
		return Receipt{}, false, domainError(CodeStateCorrupt, "stored Experiment contract is invalid")
	}
	if storedWorkID.String != workID || !bytes.Equal(storedCanonical, requestedCanonical) {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used with different Experiment input")
	}
	receipt.RequestID = requestID
	receipt.Replayed = true
	return receipt, true, nil
}

func canonicalContractJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("canonicalize contract JSON: %w", err)
	}
	return json.Marshal(value)
}

func (e *Engine) applyRecordIterationExperiment(ctx context.Context, tx *sql.Tx, command RecordIterationExperiment) (Receipt, error) {
	if command.WorkID == "" || len(command.Experiment) == 0 || !json.Valid(command.Experiment) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and a valid experiment are required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if optimization.FlowVersion != FlowVersion2 {
		return Receipt{}, domainError(CodeInvalidTransition, "Experiments are a flow v2 operation")
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationOptimizing && optimization.Status != OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "Experiment can be recorded only while optimizing")
	}
	var attemptID, baselineID string
	var round int64
	var role WorkRole
	var status WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id, baseline_revision_id, iteration_round, role, status FROM works WHERE id = ? AND optimization_id = ?`, command.WorkID, optimization.ID).Scan(&attemptID, &baselineID, &round, &role, &status); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "Iteration work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read Experiment work: %w", err)
	}
	if role != RoleIteration || status != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active Iteration")
	}
	var currentCheckpoint, scopeBestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT current_checkpoint_sha, base_sha FROM iteration_rounds WHERE attempt_id = ? AND round = ? AND status = 'running'`, attemptID, round).Scan(&currentCheckpoint, &scopeBestSHA); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeInvalidTransition, "Iteration Round is not running")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read Experiment checkpoint: %w", err)
	}
	var baselineDefinition []byte
	if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM baseline_revisions WHERE id = (SELECT baseline_revision_id FROM works WHERE id = ?)`, command.WorkID).Scan(&baselineDefinition); err != nil {
		return Receipt{}, fmt.Errorf("read Experiment Baseline Definition: %w", err)
	}
	caseSet, err := roundIterationCaseSet(ctx, tx, attemptID, round)
	if err != nil {
		return Receipt{}, err
	}
	input, comparison, err := validateIterationExperiment(command.Experiment, currentCheckpoint, baselineDefinition, caseSet.CaseIDs, command.Artifacts)
	if err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, err.Error())
	}
	if input.Hypothesis.DiagnosisHypothesisID != "" {
		found, err := diagnosisHasHypothesis(ctx, tx, baselineID, input.Hypothesis.DiagnosisHypothesisID)
		if err != nil {
			return Receipt{}, err
		}
		if !found {
			return Receipt{}, domainError(CodeInvalidCommand, "experiment references an unknown Diagnosis hypothesis")
		}
	}
	for _, artifact := range command.Artifacts {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_artifacts WHERE work_id = ? AND relative_path = ?)`, command.WorkID, artifact.RelativePath).Scan(&exists); err != nil {
			return Receipt{}, fmt.Errorf("check Experiment artifact identity: %w", err)
		}
		if exists != 0 {
			return Receipt{}, domainError(CodeInvalidCommand, "Experiment artifact path already has a durable receipt for this Work")
		}
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM iteration_experiments WHERE attempt_id = ? AND iteration_round = ?`, attemptID, round).Scan(&sequence); err != nil {
		return Receipt{}, fmt.Errorf("allocate Experiment sequence: %w", err)
	}
	now, receiptID, experimentID := e.timestamp(), e.newID(), e.newID()
	checkpoint := nullable(input.CheckpointSHA)
	if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_experiments
		(id, optimization_id, attempt_id, iteration_round, sequence, outcome, parent_checkpoint_sha, checkpoint_sha, scope_best_sha, receipt_id, experiment_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, experimentID, optimization.ID, attemptID, round, sequence, input.Outcome, input.ParentCheckpointSHA, checkpoint, scopeBestSHA, receiptID, []byte(command.Experiment), now); err != nil {
		return Receipt{}, fmt.Errorf("record Iteration Experiment: %w", err)
	}
	if comparison != nil {
		if err := persistExperimentMeasurements(ctx, tx, baselineID, command.WorkID, experimentID, receiptID, scopeBestSHA, now, *comparison); err != nil {
			return Receipt{}, err
		}
	}
	if input.Outcome == "kept" {
		result, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET current_checkpoint_sha = ? WHERE attempt_id = ? AND round = ? AND current_checkpoint_sha = ?`, input.CheckpointSHA, attemptID, round, currentCheckpoint)
		if err != nil {
			return Receipt{}, fmt.Errorf("advance Experiment checkpoint: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return Receipt{}, fmt.Errorf("count Experiment checkpoint update: %w", err)
		}
		if changed != 1 {
			return Receipt{}, domainError(CodeRevisionConflict, "Iteration checkpoint changed while recording Experiment")
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"experiment_id": experimentID, "attempt_id": attemptID, "iteration_round": round, "sequence": sequence, "outcome": input.Outcome, "checkpoint_sha": input.CheckpointSHA})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "iteration.experiment_recorded", payload, now); err != nil {
		return Receipt{}, err
	}
	for _, artifact := range command.Artifacts {
		if err := e.insertArtifact(ctx, tx, command.WorkID, receiptID, artifact, now); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func diagnosisHasHypothesis(ctx context.Context, tx *sql.Tx, baselineID, hypothesisID string) (bool, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT report_json FROM diagnoses WHERE baseline_revision_id = ? AND status IN ('ready', 'unavailable')`, baselineID).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return false, domainError(CodeStateCorrupt, "Iteration has no terminal Diagnosis report")
	} else if err != nil {
		return false, fmt.Errorf("read Diagnosis hypothesis provenance: %w", err)
	}
	report, err := kdacontract.ParseDiagnosisReport(raw)
	if err != nil {
		return false, domainError(CodeStateCorrupt, "stored Diagnosis report is invalid: "+err.Error())
	}
	for _, hypothesis := range report.Hypotheses {
		if hypothesis.ID == hypothesisID {
			return true, nil
		}
	}
	return false, nil
}

func validateIterationExperiment(raw json.RawMessage, currentCheckpoint string, baselineDefinition []byte, caseIDs []string, artifacts []ArtifactInput) (kdacontract.Experiment, *benchmarkintegrity.MeasurementComparison, error) {
	input, err := kdacontract.ParseExperiment(raw)
	if err != nil {
		return input, nil, fmt.Errorf("experiment does not match the canonical contract: %w", err)
	}
	if !isGitSHA(input.ParentCheckpointSHA) || input.ParentCheckpointSHA != currentCheckpoint || strings.TrimSpace(input.Summary) == "" {
		return input, nil, errors.New("experiment schema, parent checkpoint, or summary is invalid")
	}
	if input.Hypothesis.DiagnosisHypothesisID == "" && strings.TrimSpace(input.Hypothesis.Summary) == "" {
		return input, nil, errors.New("experiment requires a Diagnosis hypothesis ID or inline hypothesis")
	}
	if strings.TrimSpace(input.Change.Summary) == "" || strings.TrimSpace(input.Change.Mechanism) == "" || len(input.Change.Paths) == 0 {
		return input, nil, errors.New("experiment change is incomplete")
	}
	paths := map[string]bool{}
	for _, path := range input.Change.Paths {
		if path == "" || !safeSnapshotRelativePath(path) || paths[path] {
			return input, nil, errors.New("experiment change paths must be unique repository-relative paths")
		}
		paths[path] = true
	}
	if input.Outcome != "kept" && input.Outcome != "rejected" && input.Outcome != "inconclusive" {
		return input, nil, errors.New("experiment outcome must be kept, rejected, or inconclusive")
	}
	artifactPaths := map[string]bool{}
	for _, artifact := range artifacts {
		if artifactPaths[artifact.RelativePath] {
			return input, nil, errors.New("submitted Experiment artifact paths must be unique")
		}
		artifactPaths[artifact.RelativePath] = true
	}
	if len(input.Artifacts) == 0 {
		return input, nil, errors.New("experiment requires at least one submitted artifact")
	}
	for _, artifact := range input.Artifacts {
		if artifact.Path == "" || artifact.Kind == "" || !artifactPaths[artifact.Path] {
			return input, nil, fmt.Errorf("experiment artifact %q has no submitted receipt", artifact.Path)
		}
		delete(artifactPaths, artifact.Path)
	}
	if len(artifactPaths) != 0 {
		return input, nil, errors.New("submitted Experiment artifacts must exactly match the canonical report")
	}
	if input.Outcome != "kept" {
		if input.CheckpointSHA != "" || (input.Correctness != nil && len(input.Correctness.BenchmarkIntegrity) != 0 && !json.Valid(input.Correctness.BenchmarkIntegrity)) {
			return input, nil, errors.New("negative experiment cannot have a checkpoint or invalid correctness evidence")
		}
		if len(input.BenchmarkMeasurements) == 0 {
			return input, nil, nil
		}
		if !json.Valid(input.BenchmarkMeasurements) {
			return input, nil, errors.New("negative experiment benchmark_measurements must be valid JSON when present")
		}
		definition, err := benchmarkintegrity.ParseFrozenMeasurementDefinition(baselineDefinition)
		if err != nil {
			return input, nil, fmt.Errorf("parse baseline benchmark_measurements: %w", err)
		}
		subsetDefinition, err := benchmarkintegrity.FrozenCaseSubset(definition, caseIDs)
		if err != nil {
			return input, nil, fmt.Errorf("freeze Iteration Case subset for benchmark_measurements: %w", err)
		}
		comparisonEnvelope := mustJSON(map[string]json.RawMessage{"benchmark_measurements": input.BenchmarkMeasurements})
		comparison, err := benchmarkintegrity.ParseMeasurementComparison(subsetDefinition, comparisonEnvelope)
		if err != nil {
			return input, nil, fmt.Errorf("invalid negative experiment benchmark_measurements: %w", err)
		}
		return input, &comparison, nil
	}
	if !isGitSHA(input.CheckpointSHA) || input.Correctness == nil || len(input.Correctness.BenchmarkIntegrity) == 0 {
		return input, nil, errors.New("kept experiment requires a checkpoint and correctness evidence")
	}
	if len(input.BenchmarkMeasurements) == 0 || !json.Valid(input.BenchmarkMeasurements) {
		return input, nil, errors.New("kept experiment requires benchmark_measurements comparisons")
	}
	evidence := mustJSON(map[string]json.RawMessage{"benchmark_integrity": input.Correctness.BenchmarkIntegrity})
	if err := benchmarkintegrity.ValidateIterationEvidence(baselineDefinition, caseIDs, evidence); err != nil {
		return input, nil, fmt.Errorf("invalid kept experiment correctness evidence: %w", err)
	}
	definition, err := benchmarkintegrity.ParseFrozenMeasurementDefinition(baselineDefinition)
	if err != nil {
		return input, nil, fmt.Errorf("parse baseline benchmark_measurements: %w", err)
	}
	subsetDefinition, err := benchmarkintegrity.FrozenCaseSubset(definition, caseIDs)
	if err != nil {
		return input, nil, fmt.Errorf("freeze Iteration Case subset for benchmark_measurements: %w", err)
	}
	comparisonEnvelope := mustJSON(map[string]json.RawMessage{"benchmark_measurements": input.BenchmarkMeasurements})
	comparison, err := benchmarkintegrity.ParseMeasurementComparison(subsetDefinition, comparisonEnvelope)
	if err != nil {
		return input, nil, fmt.Errorf("invalid kept experiment benchmark_measurements: %w", err)
	}
	if err := benchmarkintegrity.ValidateIterationGate(subsetDefinition, comparison); err != nil {
		return input, nil, fmt.Errorf("kept experiment did not satisfy iteration_performance_gate: %w", err)
	}
	return input, &comparison, nil
}

func (e *Engine) readIterationExperiments(ctx context.Context, attemptID string, round int64) ([]IterationExperimentView, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT e.id, e.attempt_id, e.iteration_round, e.sequence, e.outcome, e.parent_checkpoint_sha, e.checkpoint_sha,
		e.scope_best_sha, e.receipt_id, e.experiment_json
		FROM iteration_experiments e
		WHERE e.attempt_id = ? AND e.iteration_round = ? ORDER BY e.sequence`, attemptID, round)
	if err != nil {
		return nil, fmt.Errorf("read Iteration Experiments: %w", err)
	}
	defer rows.Close()
	var experiments []IterationExperimentView
	indexByID := map[string]int{}
	for rows.Next() {
		var experiment IterationExperimentView
		var checkpoint sql.NullString
		if err := rows.Scan(&experiment.ID, &experiment.AttemptID, &experiment.IterationRound, &experiment.Sequence, &experiment.Outcome, &experiment.ParentCheckpointSHA, &checkpoint, &experiment.ScopeBestSHA, &experiment.ReceiptID, &experiment.Experiment); err != nil {
			return nil, fmt.Errorf("scan Iteration Experiment: %w", err)
		}
		experiment.CheckpointSHA = checkpoint.String
		experiments = append(experiments, experiment)
		indexByID[experiment.ID] = len(experiments) - 1
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	comparisonRows, err := e.db.QueryContext(ctx, `SELECT c.experiment_id, c.case_id, c.metric_id, c.reference_value, c.candidate_value, c.speedup,
		c.regression, c.regression_fraction, c.aggregate_speedup, c.max_case_speedup, c.max_case_id, c.receipt_id, c.scope_best_sha
		FROM benchmark_experiment_derived_comparisons c
		JOIN iteration_experiments e ON e.id = c.experiment_id
		WHERE e.attempt_id = ? AND e.iteration_round = ?
		ORDER BY c.experiment_id, c.metric_id, c.case_id`, attemptID, round)
	if err != nil {
		return nil, fmt.Errorf("read Iteration Experiment comparisons: %w", err)
	}
	defer comparisonRows.Close()
	for comparisonRows.Next() {
		var experimentID string
		var item BenchmarkComparisonView
		var reference, candidate, aggregate, maxCase sql.NullFloat64
		var maxCaseID, receiptID, scopeBestSHA sql.NullString
		var regression int
		if err := comparisonRows.Scan(&experimentID, &item.CaseID, &item.MetricID, &reference, &candidate, &item.Speedup, &regression, &item.RegressionFraction, &aggregate, &maxCase, &maxCaseID, &receiptID, &scopeBestSHA); err != nil {
			return nil, fmt.Errorf("scan Iteration Experiment comparison: %w", err)
		}
		index, exists := indexByID[experimentID]
		if !exists {
			continue
		}
		item.ExperimentID = experimentID
		item.ReceiptID = receiptID.String
		item.ScopeBestSHA = scopeBestSHA.String
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
		experiments[index].DerivedComparisons = append(experiments[index].DerivedComparisons, item)
		if experiments[index].ScopeBestSHA == "" {
			experiments[index].ScopeBestSHA = item.ScopeBestSHA
		}
		if experiments[index].ReceiptID == "" {
			experiments[index].ReceiptID = item.ReceiptID
		}
	}
	if err := comparisonRows.Err(); err != nil {
		return nil, err
	}
	artifactRows, err := e.db.QueryContext(ctx, `SELECT e.id, a.id
		FROM iteration_experiments e
		JOIN evidence_artifacts a ON a.receipt_id = e.receipt_id
		WHERE e.attempt_id = ? AND e.iteration_round = ?
		ORDER BY e.id, a.id`, attemptID, round)
	if err != nil {
		return nil, fmt.Errorf("read Iteration Experiment artifact IDs: %w", err)
	}
	defer artifactRows.Close()
	for artifactRows.Next() {
		var experimentID, artifactID string
		if err := artifactRows.Scan(&experimentID, &artifactID); err != nil {
			return nil, fmt.Errorf("scan Iteration Experiment artifact ID: %w", err)
		}
		index, exists := indexByID[experimentID]
		if !exists {
			continue
		}
		experiments[index].ArtifactIDs = append(experiments[index].ArtifactIDs, artifactID)
	}
	if err := artifactRows.Err(); err != nil {
		return nil, err
	}
	return experiments, nil
}
