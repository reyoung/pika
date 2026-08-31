package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReconfigureIterationAgents synchronizes the durable scheduler slot count
// with the ordered Iteration Agent list loaded from user configuration. Slots
// removed from the list are atomically cancelled before the new slot count is
// committed, so no Work can be recovered with a different backend.
func (e *Engine) ReconfigureIterationAgents(ctx context.Context, count int64) error {
	if count < 1 {
		return errors.New("at least one Iteration Agent is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Iteration Agent reconfiguration: %w", err)
	}
	defer tx.Rollback()

	var optimizationID string
	var optimizationStatus OptimizationStatus
	var revision, previous int64
	err = tx.QueryRowContext(ctx, `SELECT id, status, revision, iteration_concurrency
		FROM optimizations LIMIT 1`).Scan(&optimizationID, &optimizationStatus, &revision, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Iteration Agent configuration: %w", err)
	}
	if previous == count {
		return nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT w.id, a.id FROM attempts a JOIN works w ON w.attempt_id = a.id
		WHERE a.optimization_id = ? AND a.status = 'iterating' AND a.slot_index >= ?
		AND w.role = 'iteration' AND w.status = 'pending'`, optimizationID, count)
	if err != nil {
		return fmt.Errorf("inspect removed Iteration slots: %w", err)
	}
	var removedWorks, removedAttempts []string
	for rows.Next() {
		var workID, attemptID string
		if err := rows.Scan(&workID, &attemptID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan removed Iteration slot: %w", err)
		}
		removedWorks = append(removedWorks, workID)
		removedAttempts = append(removedAttempts, attemptID)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close removed Iteration slot rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate removed Iteration slots: %w", err)
	}
	now := e.timestamp()
	for index, workID := range removedWorks {
		attemptID := removedAttempts[index]
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = 'cancelled', finished_at = ? WHERE id = ?`, now, workID); err != nil {
			return fmt.Errorf("cancel Work in removed Iteration slot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET status = 'cancelled', finished_at = ?
			WHERE attempt_id = ? AND status IN ('queued', 'running')`, now, attemptID); err != nil {
			return fmt.Errorf("cancel Round in removed Iteration slot: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'cancelled', failure_reason = 'iteration_agent_removed', updated_at = ?
			WHERE id = ? AND status = 'iterating'`, now, attemptID); err != nil {
			return fmt.Errorf("cancel Attempt in removed Iteration slot: %w", err)
		}
		if err := insertEffect(ctx, tx, e.newID(), optimizationID, "work.close_requested", mustJSON(map[string]string{"work_id": workID}), now); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET iteration_concurrency = ?, updated_at = ? WHERE id = ?`, count, now, optimizationID); err != nil {
		return fmt.Errorf("store Iteration Agent count: %w", err)
	}
	if optimizationStatus == OptimizationOptimizing {
		var baselineID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM baseline_revisions
			WHERE optimization_id = ? AND status = 'accepted' ORDER BY number DESC LIMIT 1`, optimizationID).Scan(&baselineID); err != nil {
			return fmt.Errorf("read accepted Baseline for Iteration Agent reconfiguration: %w", err)
		}
		if err := e.fillIterationSlots(ctx, tx, optimizationID, baselineID, now); err != nil {
			return err
		}
	}
	nextRevision := revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimizationID); err != nil {
		return fmt.Errorf("advance Iteration Agent configuration revision: %w", err)
	}
	payload := mustJSON(map[string]int64{"previous_count": previous, "count": count, "cancelled_attempts": int64(len(removedAttempts))})
	if err := insertEvent(ctx, tx, e.newID(), optimizationID, nextRevision, "optimization.iteration_agents_reconfigured", payload, now); err != nil {
		return err
	}
	return tx.Commit()
}
