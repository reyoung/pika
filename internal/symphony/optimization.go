package symphony

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (e *Engine) seedOptimization(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, bestSHA, now string) error {
	if bestSHA == "" {
		bestSHA = "unresolved"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO best_revisions
		(id, optimization_id, sequence, commit_sha, created_at) VALUES (?, ?, 0, ?, ?)`, e.newID(), optimizationID, bestSHA, now); err != nil {
		return fmt.Errorf("create initial best revision: %w", err)
	}
	var concurrency int
	if err := tx.QueryRowContext(ctx, `SELECT iteration_concurrency FROM optimizations WHERE id = ?`, optimizationID).Scan(&concurrency); err != nil {
		return fmt.Errorf("read iteration concurrency: %w", err)
	}
	for slot := 0; slot < concurrency; slot++ {
		if err := e.createIterationAttempt(ctx, tx, optimizationID, baselineID, int64(slot), 0, bestSHA, "initial", "", now); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) createIterationAttempt(ctx context.Context, tx *sql.Tx, optimizationID, baselineID string, slot, bestSequence int64, bestSHA, kind, backOffMessage, now string) error {
	attemptID, roundID, workID, effectID := e.newID(), e.newID(), e.newID(), e.newID()
	var historyLimit int64
	if err := tx.QueryRowContext(ctx, `SELECT iteration_history_limit FROM optimizations WHERE id = ?`, optimizationID).Scan(&historyLimit); err != nil {
		return fmt.Errorf("read iteration history limit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attempts
		(id, optimization_id, slot_index, status, base_best_sequence, base_sha, current_iteration_round, history_limit, created_at, updated_at)
		VALUES (?, ?, ?, 'iterating', ?, ?, 1, ?, ?, ?)`, attemptID, optimizationID, slot, bestSequence, bestSHA, historyLimit, now, now); err != nil {
		return fmt.Errorf("create attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_rounds
		(id, attempt_id, round, kind, base_sha, status, back_off_message, created_at)
		VALUES (?, ?, 1, ?, ?, 'running', ?, ?)`, roundID, attemptID, kind, bestSHA, nullable(backOffMessage), now); err != nil {
		return fmt.Errorf("create iteration round: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, 1, ?)`, workID, optimizationID, baselineID, RoleIteration, WorkPending, attemptID, now); err != nil {
		return fmt.Errorf("create iteration work: %w", err)
	}
	return insertEffect(ctx, tx, effectID, optimizationID, "work.start_requested", mustJSON(map[string]any{"work_id": workID, "attempt_id": attemptID, "iteration_round": 1}), now)
}

func (e *Engine) applyFinishIteration(ctx context.Context, tx *sql.Tx, command FinishIteration) (Receipt, error) {
	if command.WorkID == "" || command.Summary == "" || (command.Outcome != IterationCandidate && command.Outcome != IterationRejected) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id, summary, and candidate or rejected outcome are required")
	}
	if command.Outcome == IterationCandidate && command.CandidateSHA == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "candidate outcome requires candidate_sha")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationOptimizing && optimization.Status != OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "iteration can finish only while optimizing")
	}
	var attemptID, baselineID string
	var round int64
	var role WorkRole
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id, baseline_revision_id, iteration_round, role, status FROM works WHERE id = ?`, command.WorkID).Scan(&attemptID, &baselineID, &round, &role, &workStatus); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "iteration work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read iteration work: %w", err)
	}
	if role != RoleIteration || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active iteration")
	}
	now, receiptID := e.timestamp(), e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET status = ?, finished_at = ? WHERE attempt_id = ? AND round = ?`, command.Outcome, now, attemptID, round); err != nil {
		return Receipt{}, err
	}
	if command.Outcome == IterationCandidate {
		var baseSHA string
		if err := tx.QueryRowContext(ctx, `SELECT base_sha FROM attempts WHERE id = ? AND status = 'iterating'`, attemptID).Scan(&baseSHA); err != nil {
			return Receipt{}, domainError(CodeInvalidTransition, "attempt is not iterating")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'awaiting_integration', candidate_sha = ?, summary = ?, updated_at = ? WHERE id = ?`, command.CandidateSHA, command.Summary, now, attemptID); err != nil {
			return Receipt{}, err
		}
		var priorIntegrationID string
		var fifoPosition int64
		err := tx.QueryRowContext(ctx, `SELECT id, fifo_position FROM integrations WHERE attempt_id = ?
			AND status IN ('stale', 'backed_off') ORDER BY sequence DESC LIMIT 1`, attemptID).Scan(&priorIntegrationID, &fifoPosition)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(i.fifo_position), 0) + 1 FROM integrations i
				JOIN attempts a ON a.id = i.attempt_id WHERE a.optimization_id = ?`, optimization.ID).Scan(&fifoPosition); err != nil {
				return Receipt{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO integrations
				(id, attempt_id, iteration_round, status, candidate_sha, expected_best_sha, created_at, fifo_position)
				VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)`, e.newID(), attemptID, round, command.CandidateSHA, baseSHA, now, fifoPosition); err != nil {
				return Receipt{}, fmt.Errorf("queue integration: %w", err)
			}
		} else if err != nil {
			return Receipt{}, err
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'refreshed' WHERE id = ?`, priorIntegrationID); err != nil {
				return Receipt{}, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO integrations
				(id, attempt_id, iteration_round, status, candidate_sha, expected_best_sha, created_at, fifo_position)
				VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)`, e.newID(), attemptID, round, command.CandidateSHA, baseSHA, now, fifoPosition); err != nil {
				return Receipt{}, fmt.Errorf("requeue refreshed integration: %w", err)
			}
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'rejected', summary = ?, failure_reason = ?, updated_at = ? WHERE id = ? AND status = 'iterating'`, command.Summary, command.Summary, now, attemptID); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraining {
		if err := e.scheduleIntegrationHead(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
		if err := e.fillIterationSlots(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"attempt_id": attemptID, "work_id": command.WorkID, "outcome": command.Outcome, "candidate_sha": command.CandidateSHA})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "iteration.finished", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) scheduleIntegrationHead(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, now string) error {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM integrations i JOIN attempts a ON a.id = i.attempt_id
		WHERE a.optimization_id = ? AND i.status IN ('running', 'best_update_prepared')`, optimizationID).Scan(&active); err != nil || active != 0 {
		return err
	}
	var integrationID, attemptID, candidateSHA, expectedBestSHA, queueStatus string
	var round int64
	err := tx.QueryRowContext(ctx, `SELECT i.id, i.attempt_id, i.iteration_round, i.candidate_sha, i.expected_best_sha, i.status
			FROM integrations i JOIN attempts a ON a.id = i.attempt_id
			WHERE a.optimization_id = ? AND i.status IN ('queued', 'stale', 'backed_off') ORDER BY i.fifo_position, i.sequence LIMIT 1`, optimizationID).
		Scan(&integrationID, &attemptID, &round, &candidateSHA, &expectedBestSHA, &queueStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if queueStatus != "queued" {
		return nil
	}
	var bestSHA string
	var bestSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence, commit_sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1`, optimizationID).Scan(&bestSequence, &bestSHA); err != nil {
		return err
	}
	if expectedBestSHA != bestSHA {
		return e.refreshStaleAttempt(ctx, tx, optimizationID, baselineID, attemptID, bestSequence, bestSHA, "stale_best", "Best advanced before Integration", now)
	}
	workID := e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'running' WHERE id = ? AND status = 'queued'`, integrationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'integrating', updated_at = ? WHERE id = ?`, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, integration_id, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`, workID, optimizationID, baselineID, RoleIntegration, WorkPending, attemptID, round, integrationID, now); err != nil {
		return err
	}
	return insertEffect(ctx, tx, e.newID(), optimizationID, "work.start_requested", mustJSON(map[string]string{"work_id": workID}), now)
}

func (e *Engine) fillIterationSlots(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, now string) error {
	var concurrency, maxPending, active, pending, integrationBacklog, refreshPending int64
	if err := tx.QueryRowContext(ctx, `SELECT iteration_concurrency, max_pending_attempts FROM optimizations WHERE id = ?`, optimizationID).Scan(&concurrency, &maxPending); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE optimization_id = ? AND status = 'iterating'`, optimizationID).Scan(&active); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE optimization_id = ?
		AND status IN ('awaiting_integration', 'integrating', 'refresh_pending')`, optimizationID).Scan(&pending); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM integrations i JOIN attempts a ON a.id = i.attempt_id
		WHERE a.optimization_id = ? AND i.status IN ('queued', 'running', 'best_update_prepared', 'stale', 'backed_off')`, optimizationID).Scan(&integrationBacklog); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE optimization_id = ? AND status = 'refresh_pending'`, optimizationID).Scan(&refreshPending); err != nil {
		return err
	}
	targetConcurrency := concurrency
	if integrationBacklog > 0 && refreshPending == 0 && targetConcurrency > 0 {
		targetConcurrency--
	}
	var bestSequence int64
	var bestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT sequence, commit_sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1`, optimizationID).Scan(&bestSequence, &bestSHA); err != nil {
		return err
	}
	for active < targetConcurrency {
		slot, err := availableIterationSlot(ctx, tx, optimizationID, concurrency)
		if err != nil {
			return err
		}
		var refreshAttemptID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM attempts WHERE optimization_id = ? AND status = 'refresh_pending'
			ORDER BY updated_at, id LIMIT 1`, optimizationID).Scan(&refreshAttemptID)
		if err == nil {
			if err := e.startPendingRefresh(ctx, tx, optimizationID, baselineID, refreshAttemptID, slot, now); err != nil {
				return err
			}
			active++
			pending--
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if pending >= maxPending {
			break
		}
		if err := e.createIterationAttempt(ctx, tx, optimizationID, baselineID, slot, bestSequence, bestSHA, "initial", "", now); err != nil {
			return err
		}
		active++
	}
	return nil
}

func (e *Engine) refreshStaleAttempt(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, attemptID string, bestSequence int64, bestSHA, kind, message, now string) error {
	var round int64
	if err := tx.QueryRowContext(ctx, `SELECT current_iteration_round + 1 FROM attempts WHERE id = ?`, attemptID).Scan(&round); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'refresh_pending', base_best_sequence = ?, base_sha = ?, current_iteration_round = ?, updated_at = ? WHERE id = ?`, bestSequence, bestSHA, round, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'stale', finished_at = ? WHERE attempt_id = ? AND status = 'queued'`, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO iteration_rounds
		(id, attempt_id, round, kind, base_sha, status, back_off_message, created_at) VALUES (?, ?, ?, ?, ?, 'queued', ?, ?)`, e.newID(), attemptID, round, kind, bestSHA, nullable(message), now); err != nil {
		return err
	}
	return nil
}

func (e *Engine) startPendingRefresh(ctx context.Context, tx *sql.Tx, optimizationID, baselineID, attemptID string, slot int64, now string) error {
	var round int64
	if err := tx.QueryRowContext(ctx, `SELECT current_iteration_round FROM attempts WHERE id = ? AND status = 'refresh_pending'`, attemptID).Scan(&round); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'iterating', slot_index = ?, updated_at = ? WHERE id = ? AND status = 'refresh_pending'`, slot, now, attemptID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET status = 'running' WHERE attempt_id = ? AND round = ? AND status = 'queued'`, attemptID, round); err != nil {
		return err
	}
	workID := e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`, workID, optimizationID, baselineID, RoleIteration, WorkPending, attemptID, round, now); err != nil {
		return err
	}
	return insertEffect(ctx, tx, e.newID(), optimizationID, "work.start_requested", mustJSON(map[string]string{"work_id": workID}), now)
}

func availableIterationSlot(ctx context.Context, tx *sql.Tx, optimizationID string, concurrency int64) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slot_index FROM attempts WHERE optimization_id = ? AND status = 'iterating'`, optimizationID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := make(map[int64]bool)
	for rows.Next() {
		var slot int64
		if err := rows.Scan(&slot); err != nil {
			return 0, err
		}
		used[slot] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for slot := int64(0); slot < concurrency; slot++ {
		if !used[slot] {
			return slot, nil
		}
	}
	return 0, domainError(CodeStateCorrupt, "iteration capacity has no available slot")
}

func (e *Engine) applyPrepareBestUpdate(ctx context.Context, tx *sql.Tx, command PrepareBestUpdate) (Receipt, error) {
	if command.WorkID == "" || len(command.Validation) == 0 || !json.Valid(command.Validation) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and valid validation are required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	var integrationID, attemptID, candidateSHA, expectedBestSHA string
	var role WorkRole
	var status WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT w.integration_id, w.attempt_id, w.role, w.status, i.candidate_sha, i.expected_best_sha
		FROM works w JOIN integrations i ON i.id = w.integration_id WHERE w.id = ?`, command.WorkID).Scan(&integrationID, &attemptID, &role, &status, &candidateSHA, &expectedBestSHA); err != nil {
		return Receipt{}, domainError(CodeInvalidTransition, "integration work is not active")
	}
	if role != RoleIntegration || status != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active integration")
	}
	var bestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT commit_sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1`, optimization.ID).Scan(&bestSHA); err != nil {
		return Receipt{}, err
	}
	if bestSHA != expectedBestSHA {
		return Receipt{}, domainError(CodeRevisionConflict, "Best changed before integration intent")
	}
	intentID, receiptID, now := e.newID(), e.newID(), e.timestamp()
	payload := mustJSON(map[string]string{"intent_id": intentID, "expected_best_sha": bestSHA, "candidate_sha": candidateSHA, "attempt_id": attemptID})
	if _, err := tx.ExecContext(ctx, `INSERT INTO git_intents
		(id, integration_id, state, expected_best_sha, candidate_sha, payload_json, created_at)
		VALUES (?, ?, 'pending', ?, ?, ?, ?)`, intentID, integrationID, bestSHA, candidateSHA, payload, now); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'best_update_prepared', intent_id = ?, validation_json = ? WHERE id = ? AND status = 'running'`, intentID, []byte(command.Validation), integrationID); err != nil {
		return Receipt{}, err
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "integration.best_update_prepared", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyFinishIntegration(ctx context.Context, tx *sql.Tx, command FinishIntegration) (Receipt, error) {
	if command.WorkID == "" || (command.Outcome != IntegrationAccepted && command.Outcome != IntegrationRejected && command.Outcome != IntegrationStale) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and accepted, rejected, or stale outcome are required")
	}
	if len(command.Result) != 0 && !json.Valid(command.Result) {
		return Receipt{}, domainError(CodeInvalidCommand, "integration result must be valid JSON")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	var integrationID, attemptID, baselineID, integrationStatus, candidateSHA, expectedBestSHA string
	var role WorkRole
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT w.integration_id, w.attempt_id, w.baseline_revision_id, w.role, w.status,
		i.status, i.candidate_sha, i.expected_best_sha FROM works w JOIN integrations i ON i.id = w.integration_id WHERE w.id = ?`, command.WorkID).Scan(
		&integrationID, &attemptID, &baselineID, &role, &workStatus, &integrationStatus, &candidateSHA, &expectedBestSHA); err != nil {
		return Receipt{}, domainError(CodeInvalidTransition, "integration work is not active")
	}
	if role != RoleIntegration || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not active integration")
	}
	now, receiptID := e.timestamp(), e.newID()
	if command.Outcome == IntegrationAccepted {
		if integrationStatus != "best_update_prepared" || command.ObservedBestSHA != expectedBestSHA || command.AppliedSHA == "" {
			return Receipt{}, domainError(CodeInvalidTransition, "accepted integration does not satisfy the prepared Git intent")
		}
		var bestSequence int64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM best_revisions WHERE optimization_id = ?`, optimization.ID).Scan(&bestSequence); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO best_revisions
			(id, optimization_id, sequence, commit_sha, source_attempt_id, evidence_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.newID(), optimization.ID, bestSequence+1, command.AppliedSHA, attemptID, nullableBytes(command.Result), now); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE git_intents SET state = 'applied', applied_at = ? WHERE integration_id = ?`, now, integrationID); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'accepted', updated_at = ? WHERE id = ?`, now, attemptID); err != nil {
			return Receipt{}, err
		}
	} else if command.Outcome == IntegrationRejected {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'rejected', failure_reason = 'integration_rejected', updated_at = ? WHERE id = ?`, now, attemptID); err != nil {
			return Receipt{}, err
		}
	} else {
		var bestSequence int64
		var bestSHA string
		if err := tx.QueryRowContext(ctx, `SELECT sequence, commit_sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1`, optimization.ID).Scan(&bestSequence, &bestSHA); err != nil {
			return Receipt{}, err
		}
		if err := e.refreshStaleAttempt(ctx, tx, optimization.ID, baselineID, attemptID, bestSequence, bestSHA, "stale_integration", "Best changed during Integration", now); err != nil {
			return Receipt{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = ?, result_json = ?, finished_at = ? WHERE id = ?`, command.Outcome, nullableBytes(command.Result), now, integrationID); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraining {
		if err := e.scheduleIntegrationHead(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
		if err := e.fillIterationSlots(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"attempt_id": attemptID, "integration_id": integrationID, "outcome": command.Outcome})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "integration.finished", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyIntegrationBackOff(ctx context.Context, tx *sql.Tx, optimization optimizationRecord, command BackOff, baselineID, attemptID, integrationID string, workStatus WorkStatus) (Receipt, error) {
	if optimization.Status != OptimizationOptimizing || attemptID == "" || integrationID == "" || (workStatus != WorkPending && workStatus != WorkCancelled) {
		return Receipt{}, domainError(CodeInvalidTransition, "back-off source is not an active Integration")
	}
	var integrationStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM integrations WHERE id = ? AND attempt_id = ?`, integrationID, attemptID).Scan(&integrationStatus); err != nil {
		return Receipt{}, domainError(CodeInvalidTransition, "integration back-off source was not found")
	}
	if integrationStatus != "running" && integrationStatus != "best_update_prepared" {
		return Receipt{}, domainError(CodeInvalidTransition, "integration is not active")
	}
	now, receiptID := e.timestamp(), e.newID()
	if workStatus == WorkPending {
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
			return Receipt{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'backed_off', finished_at = ? WHERE id = ?`, now, integrationID); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE git_intents SET state = 'abandoned' WHERE integration_id = ? AND state = 'pending'`, integrationID); err != nil {
		return Receipt{}, err
	}
	var bestSequence int64
	var bestSHA string
	if err := tx.QueryRowContext(ctx, `SELECT sequence, commit_sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1`, optimization.ID).Scan(&bestSequence, &bestSHA); err != nil {
		return Receipt{}, err
	}
	if err := e.refreshStaleAttempt(ctx, tx, optimization.ID, baselineID, attemptID, bestSequence, bestSHA, "user_back_off", command.Message, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if err := e.scheduleIntegrationHead(ctx, tx, optimization.ID, baselineID, now); err != nil {
		return Receipt{}, err
	}
	if err := e.fillIterationSlots(ctx, tx, optimization.ID, baselineID, now); err != nil {
		return Receipt{}, err
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]string{"work_id": command.WorkID, "attempt_id": attemptID, "integration_id": integrationID, "message": command.Message})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "integration.backed_off", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyAttemptCancellation(ctx context.Context, tx *sql.Tx, optimization optimizationRecord, command CancelWork, role WorkRole, baselineID, attemptID, integrationID string) (Receipt, error) {
	if attemptID == "" || (optimization.Status != OptimizationOptimizing && optimization.Status != OptimizationDraining) {
		return Receipt{}, domainError(CodeInvalidTransition, "Attempt Work cannot be cancelled in the current state")
	}
	now, receiptID := e.timestamp(), e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCancelled, now, command.WorkID); err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'cancelled', failure_reason = 'user_cancelled', updated_at = ? WHERE id = ?`, now, attemptID); err != nil {
		return Receipt{}, err
	}
	if role == RoleIteration {
		var round int64
		if err := tx.QueryRowContext(ctx, `SELECT iteration_round FROM works WHERE id = ?`, command.WorkID).Scan(&round); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET status = 'cancelled', finished_at = ? WHERE attempt_id = ? AND round = ?`, now, attemptID, round); err != nil {
			return Receipt{}, err
		}
	} else {
		if integrationID == "" {
			return Receipt{}, domainError(CodeStateCorrupt, "Integration Work has no Integration identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'cancelled', finished_at = ? WHERE id = ?`, now, integrationID); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE git_intents SET state = 'abandoned' WHERE integration_id = ? AND state = 'pending'`, integrationID); err != nil {
			return Receipt{}, err
		}
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraining {
		if err := e.scheduleIntegrationHead(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
		if err := e.fillIterationSlots(ctx, tx, optimization.ID, baselineID, now); err != nil {
			return Receipt{}, err
		}
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]string{"work_id": command.WorkID, "attempt_id": attemptID, "role": string(role)})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "attempt.cancelled", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}
