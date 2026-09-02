package symphony

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
)

func caseSnapshotDigest(caseIDs []string) string {
	encoded := mustJSON(caseIDs)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// startExperimentCycle is the only flow-v3 seam that can make a Benchmark
// Work runnable. Callers must already have created and frozen the Round.
func (e *Engine) startExperimentCycle(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, attemptID string, round int64, checkpointSHA, now string) error {
	if !isGitSHA(checkpointSHA) {
		return domainError(CodeStateCorrupt, "Experiment Cycle checkpoint is invalid")
	}
	caseSet, err := roundIterationCaseSet(ctx, tx, attemptID, round)
	if err != nil {
		return err
	}
	var definitionDigest string
	if err := tx.QueryRowContext(ctx, `SELECT definition_digest FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&definitionDigest); err != nil {
		return fmt.Errorf("read Experiment Cycle Baseline digest: %w", err)
	}
	if !isSHA256(definitionDigest) {
		return domainError(CodeStateCorrupt, "Experiment Cycle Baseline digest is invalid")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM experiment_cycles WHERE attempt_id = ? AND iteration_round = ?`, attemptID, round).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate Experiment Cycle sequence: %w", err)
	}
	cycleID := e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO experiment_cycles
		(id, optimization_id, baseline_revision_id, attempt_id, iteration_round, sequence, checkpoint_sha,
		 baseline_definition_sha256, case_snapshot_sha256, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'benchmark_pending', ?)`, cycleID, optimizationID, baselineID, attemptID, round,
		sequence, checkpointSHA, definitionDigest, caseSnapshotDigest(caseSet.CaseIDs), now); err != nil {
		return fmt.Errorf("create Experiment Cycle: %w", err)
	}
	return e.startBenchmarkRun(ctx, tx, optimizationID, baselineID, attemptID, round, cycleID, now)
}

func (e *Engine) startBenchmarkRun(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, attemptID string, round int64, cycleID, now string) error {
	var runNumber int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(run_number), 0) + 1 FROM benchmark_runs WHERE experiment_cycle_id = ?`, cycleID).Scan(&runNumber); err != nil {
		return fmt.Errorf("allocate Benchmark Run number: %w", err)
	}
	workID, runID := e.newID(), e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, experiment_cycle_id, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`, workID, optimizationID, baselineID, RoleBenchmark, WorkPending, attemptID, round, cycleID, now); err != nil {
		return fmt.Errorf("create Benchmark Work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO benchmark_runs
		(id, experiment_cycle_id, work_id, run_number, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`, runID, cycleID, workID, runNumber, now); err != nil {
		return fmt.Errorf("create Benchmark Run: %w", err)
	}
	return insertEffect(ctx, tx, e.newID(), optimizationID, "work.start_requested", mustJSON(map[string]any{
		"work_id": workID, "attempt_id": attemptID, "iteration_round": round, "experiment_cycle_id": cycleID,
	}), now)
}

func (e *Engine) applyFinishIterationBenchmark(ctx context.Context, tx *sql.Tx, command FinishIterationBenchmark) (Receipt, error) {
	if command.WorkID == "" || (command.Outcome != BenchmarkMeasured && command.Outcome != BenchmarkUnavailable) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and measured or unavailable outcome are required")
	}
	if len(command.Environment) != 0 {
		var environment map[string]any
		if err := json.Unmarshal(command.Environment, &environment); err != nil || environment == nil {
			return Receipt{}, domainError(CodeInvalidCommand, "benchmark environment must be a JSON object")
		}
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if optimization.FlowVersion != FlowVersion3 {
		return Receipt{}, domainError(CodeInvalidTransition, "Benchmark Work is a flow v3 operation")
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationOptimizing && optimization.Status != OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "Benchmark Work can finish only while optimizing")
	}
	var baselineID, attemptID, cycleID, runID, checkpointSHA, definitionDigest, caseDigest string
	var round int64
	var role WorkRole
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT w.baseline_revision_id, w.attempt_id, w.iteration_round, w.role, w.status,
		w.experiment_cycle_id, r.id, c.checkpoint_sha, c.baseline_definition_sha256, c.case_snapshot_sha256
		FROM works w JOIN benchmark_runs r ON r.work_id = w.id JOIN experiment_cycles c ON c.id = r.experiment_cycle_id
		WHERE w.id = ? AND w.optimization_id = ? AND r.status = 'pending'`, command.WorkID, optimization.ID).Scan(
		&baselineID, &attemptID, &round, &role, &workStatus, &cycleID, &runID, &checkpointSHA, &definitionDigest, &caseDigest); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "active Benchmark Work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read Benchmark Work: %w", err)
	}
	if role != RoleBenchmark || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active Benchmark")
	}
	var currentCheckpoint, scopeBestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT current_checkpoint_sha, base_sha FROM iteration_rounds WHERE attempt_id = ? AND round = ? AND status = 'running'`, attemptID, round).Scan(&currentCheckpoint, &scopeBestSHA); err != nil {
		return Receipt{}, domainError(CodeInvalidTransition, "Benchmark Iteration Round is not running")
	}
	caseSet, err := roundIterationCaseSet(ctx, tx, attemptID, round)
	if err != nil {
		return Receipt{}, err
	}
	var definition []byte
	if err := tx.QueryRowContext(ctx, `SELECT definition_json FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&definition); err != nil {
		return Receipt{}, fmt.Errorf("read Benchmark Baseline Definition: %w", err)
	}
	digest := sha256.Sum256(definition)
	if checkpointSHA != currentCheckpoint || definitionDigest != hex.EncodeToString(digest[:]) || caseDigest != caseSnapshotDigest(caseSet.CaseIDs) {
		return Receipt{}, domainError(CodeStateCorrupt, "Benchmark Work scope no longer matches its Experiment Cycle")
	}
	now := e.timestamp()
	if command.Outcome == BenchmarkUnavailable {
		if strings.TrimSpace(command.Reason) == "" || len(command.Measurements) != 0 || len(command.Artifacts) != 0 {
			return Receipt{}, domainError(CodeInvalidCommand, "unavailable Benchmark requires a reason and no measurements or artifacts")
		}
		if optimization.Status == OptimizationDraining {
			return Receipt{}, domainError(CodeInvalidTransition, "Benchmark cannot pause a draining Optimization")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE benchmark_runs SET status = 'unavailable', failure_reason = ?, environment_json = ?, provider = NULLIF(?, ''), model = NULLIF(?, ''), finished_at = ? WHERE id = ?`,
			strings.TrimSpace(command.Reason), nullableBytes(command.Environment), command.Provider, command.Model, now, runID); err != nil {
			return Receipt{}, fmt.Errorf("record unavailable Benchmark Run: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE experiment_cycles SET status = 'benchmark_unavailable', failure_reason = ? WHERE id = ? AND status = 'benchmark_pending'`, strings.TrimSpace(command.Reason), cycleID)
		if err != nil {
			return Receipt{}, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return Receipt{}, domainError(CodeInvalidTransition, "Experiment Cycle Benchmark is not pending")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, OptimizationPaused, now, optimization.ID); err != nil {
			return Receipt{}, err
		}
		if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
			return Receipt{}, err
		}
		payload := mustJSON(map[string]any{"experiment_cycle_id": cycleID, "benchmark_run_id": runID, "benchmark_work_id": command.WorkID, "reason": strings.TrimSpace(command.Reason)})
		if err := insertEvent(ctx, tx, e.newID(), optimization.ID, optimization.Revision+1, "experiment_cycle.benchmark_unavailable", payload, now); err != nil {
			return Receipt{}, err
		}
		return Receipt{ID: e.newID(), Revision: optimization.Revision + 1, Result: payload}, nil
	}
	if len(command.Measurements) == 0 || !json.Valid(command.Measurements) || len(command.Environment) == 0 || len(command.Artifacts) == 0 || strings.TrimSpace(command.Provider) == "" || strings.TrimSpace(command.Model) == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "measured Benchmark requires measurements, environment, provider, model, and at least one artifact")
	}
	measurementDefinition, err := benchmarkintegrity.ParseFrozenMeasurementDefinition(definition)
	if err != nil {
		return Receipt{}, domainError(CodeStateCorrupt, "stored Benchmark measurement definition is invalid: "+err.Error())
	}
	subset, err := benchmarkintegrity.FrozenCaseSubset(measurementDefinition, caseSet.CaseIDs)
	if err != nil {
		return Receipt{}, domainError(CodeStateCorrupt, err.Error())
	}
	referenceSet, err := benchmarkintegrity.ParseMeasurementSet(subset, command.Measurements)
	if err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, "invalid reference measurements: "+err.Error())
	}
	for _, artifact := range command.Artifacts {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_artifacts WHERE work_id = ? AND relative_path = ?)`, command.WorkID, artifact.RelativePath).Scan(&exists); err != nil {
			return Receipt{}, err
		}
		if exists != 0 {
			return Receipt{}, domainError(CodeInvalidCommand, "Benchmark artifact path already has a durable receipt for this Work")
		}
	}
	referenceReceiptID, iterationWorkID := e.newID(), ""
	if optimization.Status != OptimizationDraining {
		iterationWorkID = e.newID()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE benchmark_runs SET status = 'measured', measurements_json = ?, environment_json = ?, provider = NULLIF(?, ''), model = NULLIF(?, ''), finished_at = ? WHERE id = ?`,
		[]byte(command.Measurements), nullableBytes(command.Environment), command.Provider, command.Model, now, runID); err != nil {
		return Receipt{}, fmt.Errorf("complete Benchmark Run: %w", err)
	}
	var cycleSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM experiment_cycles WHERE id = ?`, cycleID).Scan(&cycleSequence); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reference_receipts
		(id, experiment_cycle_id, benchmark_run_id, benchmark_work_id, optimization_id, baseline_revision_id, attempt_id,
		 iteration_round, cycle_sequence, checkpoint_sha, baseline_definition_sha256, case_snapshot_sha256, provider, model,
		 measurements_json, environment_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)`, referenceReceiptID, cycleID, runID,
		command.WorkID, optimization.ID, baselineID, attemptID, round, cycleSequence, checkpointSHA, definitionDigest, caseDigest,
		command.Provider, command.Model, []byte(command.Measurements), nullableBytes(command.Environment), now); err != nil {
		return Receipt{}, fmt.Errorf("create Reference Receipt: %w", err)
	}
	if err := persistReferenceMeasurementSet(ctx, tx, baselineID, command.WorkID, referenceReceiptID, scopeBestSHA, now, referenceSet); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraining {
		if _, err := tx.ExecContext(ctx, `INSERT INTO works
			(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, experiment_cycle_id, created_at)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`, iterationWorkID, optimization.ID, baselineID, RoleIteration, WorkPending, attemptID, round, cycleID, now); err != nil {
			return Receipt{}, fmt.Errorf("create Experiment Cycle Iteration Work: %w", err)
		}
	}
	cycleStatus, cycleReason := "iteration_active", ""
	if optimization.Status == OptimizationDraining {
		cycleStatus, cycleReason = "abandoned", "optimization_draining"
	}
	cycleResult, err := tx.ExecContext(ctx, `UPDATE experiment_cycles SET status = ?, failure_reason = NULLIF(?, ''), completed_at = CASE WHEN ? = 'abandoned' THEN ? ELSE NULL END
		WHERE id = ? AND status = 'benchmark_pending'`, cycleStatus, cycleReason, cycleStatus, now, cycleID)
	if err != nil {
		return Receipt{}, err
	}
	if changed, _ := cycleResult.RowsAffected(); changed != 1 {
		return Receipt{}, domainError(CodeInvalidTransition, "Experiment Cycle Benchmark is not pending")
	}
	for _, artifact := range command.Artifacts {
		if err := e.insertArtifact(ctx, tx, command.WorkID, referenceReceiptID, artifact, now); err != nil {
			return Receipt{}, err
		}
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraining {
		if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.start_requested", mustJSON(map[string]any{"work_id": iterationWorkID, "experiment_cycle_id": cycleID}), now); err != nil {
			return Receipt{}, err
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payloadValues := map[string]any{"experiment_cycle_id": cycleID, "benchmark_run_id": runID, "benchmark_work_id": command.WorkID, "reference_receipt_id": referenceReceiptID, "checkpoint_sha": checkpointSHA}
	if iterationWorkID != "" {
		payloadValues["iteration_work_id"] = iterationWorkID
	}
	payload := mustJSON(payloadValues)
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "experiment_cycle.reference_measured", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: referenceReceiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyStartNextExperiment(ctx context.Context, tx *sql.Tx, command StartNextExperiment) (Receipt, error) {
	if command.WorkID == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id is required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if optimization.FlowVersion != FlowVersion3 || optimization.Status != OptimizationOptimizing {
		return Receipt{}, domainError(CodeInvalidTransition, "next Experiment can start only in an optimizing flow v3 Optimization")
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	var baselineID, attemptID, cycleID, checkpointSHA string
	var round int64
	var role WorkRole
	var status WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT w.baseline_revision_id, w.attempt_id, w.iteration_round, w.role, w.status,
		w.experiment_cycle_id, r.current_checkpoint_sha FROM works w JOIN iteration_rounds r ON r.attempt_id = w.attempt_id AND r.round = w.iteration_round
		WHERE w.id = ?`, command.WorkID).Scan(&baselineID, &attemptID, &round, &role, &status, &cycleID, &checkpointSHA); err != nil {
		return Receipt{}, domainError(CodeWorkNotFound, "Iteration Work was not found")
	}
	if role != RoleIteration || status != WorkPending || cycleID == "" {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active Experiment Cycle Iteration")
	}
	var experiments int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM iteration_experiments WHERE work_id = ?`, command.WorkID).Scan(&experiments); err != nil {
		return Receipt{}, err
	}
	if experiments > 1 {
		return Receipt{}, domainError(CodeStateCorrupt, "Experiment Cycle Iteration Work contains multiple Experiments")
	}
	if experiments == 0 && strings.TrimSpace(command.Reason) == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "abandoning an unused Reference Receipt requires a reason")
	}
	now := e.timestamp()
	cycleStatus := "completed"
	if experiments == 0 {
		cycleStatus = "abandoned"
	}
	cycleResult, err := tx.ExecContext(ctx, `UPDATE experiment_cycles SET status = ?, failure_reason = NULLIF(?, ''), completed_at = ? WHERE id = ? AND status = 'iteration_active'`,
		cycleStatus, strings.TrimSpace(command.Reason), now, cycleID)
	if err != nil {
		return Receipt{}, err
	}
	if changed, _ := cycleResult.RowsAffected(); changed != 1 {
		return Receipt{}, domainError(CodeInvalidTransition, "Experiment Cycle is not active")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if err := e.startExperimentCycle(ctx, tx, optimization.ID, baselineID, attemptID, round, checkpointSHA, now); err != nil {
		return Receipt{}, err
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"completed_experiment_cycle_id": cycleID, "work_id": command.WorkID, "attempt_id": attemptID, "iteration_round": round, "checkpoint_sha": checkpointSHA})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "experiment_cycle.next_started", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: e.newID(), Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) readExperimentCycle(ctx context.Context, cycleID string) (ExperimentCycleView, error) {
	var cycle ExperimentCycleView
	var benchmarkWorkID, iterationWorkID, referenceReceiptID, failureReason sql.NullString
	err := e.db.QueryRowContext(ctx, `SELECT c.id, c.attempt_id, c.iteration_round, c.sequence, c.checkpoint_sha,
		c.baseline_definition_sha256, c.case_snapshot_sha256, c.status, c.failure_reason,
		(SELECT r.work_id FROM benchmark_runs r WHERE r.experiment_cycle_id = c.id ORDER BY r.run_number DESC LIMIT 1),
		(SELECT w.id FROM works w WHERE w.experiment_cycle_id = c.id AND w.role = 'iteration' ORDER BY w.rowid DESC LIMIT 1),
		(SELECT rr.id FROM reference_receipts rr WHERE rr.experiment_cycle_id = c.id)
		FROM experiment_cycles c WHERE c.id = ?`, cycleID).Scan(
		&cycle.ID, &cycle.AttemptID, &cycle.IterationRound, &cycle.Sequence, &cycle.CheckpointSHA,
		&cycle.BaselineDefinitionSHA, &cycle.CaseSnapshotSHA, &cycle.Status, &failureReason,
		&benchmarkWorkID, &iterationWorkID, &referenceReceiptID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ExperimentCycleView{}, domainError(CodeStateCorrupt, "Experiment Cycle was not found")
	}
	if err != nil {
		return ExperimentCycleView{}, fmt.Errorf("read Experiment Cycle: %w", err)
	}
	cycle.BenchmarkWorkID = benchmarkWorkID.String
	cycle.IterationWorkID = iterationWorkID.String
	cycle.ReferenceReceiptID = referenceReceiptID.String
	cycle.FailureReason = failureReason.String
	return cycle, nil
}

func (e *Engine) readExperimentCycles(ctx context.Context, optimizationID string) ([]ExperimentCycleView, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM experiment_cycles WHERE optimization_id = ? ORDER BY created_at, id`, optimizationID)
	if err != nil {
		return nil, fmt.Errorf("read Experiment Cycle IDs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan Experiment Cycle ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Experiment Cycle IDs: %w", err)
	}
	cycles := make([]ExperimentCycleView, 0, len(ids))
	for _, id := range ids {
		cycle, err := e.readExperimentCycle(ctx, id)
		if err != nil {
			return nil, err
		}
		cycles = append(cycles, cycle)
	}
	return cycles, nil
}

func (e *Engine) readReferenceReceiptForCycle(ctx context.Context, cycleID string) (ReferenceReceiptView, bool, error) {
	var receipt ReferenceReceiptView
	var provider, model, consumedExperimentID sql.NullString
	var measurements, environment []byte
	err := e.db.QueryRowContext(ctx, `SELECT id, experiment_cycle_id, benchmark_run_id, benchmark_work_id, checkpoint_sha,
		baseline_definition_sha256, case_snapshot_sha256, provider, model, measurements_json, environment_json, consumed_experiment_id
		FROM reference_receipts WHERE experiment_cycle_id = ?`, cycleID).Scan(
		&receipt.ID, &receipt.ExperimentCycleID, &receipt.BenchmarkRunID, &receipt.BenchmarkWorkID, &receipt.CheckpointSHA,
		&receipt.BaselineDefinitionSHA, &receipt.CaseSnapshotSHA, &provider, &model, &measurements, &environment, &consumedExperimentID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReferenceReceiptView{}, false, nil
	}
	if err != nil {
		return ReferenceReceiptView{}, false, fmt.Errorf("read Reference Receipt: %w", err)
	}
	receipt.Provider = provider.String
	receipt.Model = model.String
	receipt.Measurements = measurements
	receipt.Environment = environment
	receipt.ConsumedExperimentID = consumedExperimentID.String
	return receipt, true, nil
}

func (e *Engine) readReferenceReceipts(ctx context.Context, optimizationID string) ([]ReferenceReceiptView, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT experiment_cycle_id FROM reference_receipts WHERE optimization_id = ? ORDER BY created_at, id`, optimizationID)
	if err != nil {
		return nil, fmt.Errorf("read Reference Receipt Cycle IDs: %w", err)
	}
	defer rows.Close()
	var cycleIDs []string
	for rows.Next() {
		var cycleID string
		if err := rows.Scan(&cycleID); err != nil {
			return nil, fmt.Errorf("scan Reference Receipt Cycle ID: %w", err)
		}
		cycleIDs = append(cycleIDs, cycleID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close Reference Receipt Cycle IDs: %w", err)
	}
	receipts := make([]ReferenceReceiptView, 0, len(cycleIDs))
	for _, cycleID := range cycleIDs {
		receipt, found, err := e.readReferenceReceiptForCycle(ctx, cycleID)
		if err != nil {
			return nil, err
		}
		if found {
			receipts = append(receipts, receipt)
		}
	}
	return receipts, nil
}
