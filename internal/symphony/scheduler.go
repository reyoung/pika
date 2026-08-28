package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const schedulerResumeMessage = "继续"

type schedulerCommandResult struct {
	SchedulerStatus SchedulerStatus `json:"scheduler_status"`
	Epoch           int64           `json:"epoch"`
	CycleID         string          `json:"cycle_id,omitempty"`
	EffectID        string          `json:"effect_id,omitempty"`
	Noop            bool            `json:"noop,omitempty"`
}

func (e *Engine) applyPauseScheduler(ctx context.Context, tx *sql.Tx, command PauseScheduler) (Receipt, error) {
	return e.applySchedulerControl(ctx, tx, command.Meta, "pause")
}

func (e *Engine) applyResumeScheduler(ctx context.Context, tx *sql.Tx, command ResumeScheduler) (Receipt, error) {
	return e.applySchedulerControl(ctx, tx, command.Meta, "resume")
}

func (e *Engine) applySchedulerControl(ctx context.Context, tx *sql.Tx, meta CommandMeta, action string) (Receipt, error) {
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status == OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "scheduler control is unavailable while the optimization is draining")
	}
	var status SchedulerStatus
	var pausedAt sql.NullString
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT scheduler_status, scheduler_paused_at, scheduler_epoch
		FROM optimizations WHERE id = ?`, optimization.ID).Scan(&status, &pausedAt, &epoch); err != nil {
		return Receipt{}, fmt.Errorf("read Scheduler state: %w", err)
	}
	want := SchedulerPaused
	if action == "resume" {
		want = SchedulerRunning
	}
	if status == want {
		result := mustJSON(schedulerCommandResult{SchedulerStatus: status, Epoch: epoch, Noop: true})
		return Receipt{ID: e.newID(), Revision: optimization.Revision, Result: result}, nil
	}
	if action != "pause" && action != "resume" {
		return Receipt{}, domainError(CodeInvalidCommand, "scheduler action must be pause or resume")
	}

	nowTime := e.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	nextRevision, nextEpoch := optimization.Revision+1, epoch+1
	if action == "resume" {
		if !pausedAt.Valid {
			return Receipt{}, domainError(CodeStateCorrupt, "paused Scheduler has no pause timestamp")
		}
		if err := shiftWaitingFollowUps(ctx, tx, pausedAt.String, nowTime); err != nil {
			return Receipt{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET scheduler_status = ?, scheduler_paused_at = NULL,
			scheduler_epoch = ?, revision = ?, updated_at = ? WHERE id = ?`, SchedulerRunning, nextEpoch, nextRevision, now, optimization.ID); err != nil {
			return Receipt{}, fmt.Errorf("resume Scheduler: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET scheduler_status = ?, scheduler_paused_at = ?,
			scheduler_epoch = ?, revision = ?, updated_at = ? WHERE id = ?`, SchedulerPaused, now, nextEpoch, nextRevision, now, optimization.ID); err != nil {
			return Receipt{}, fmt.Errorf("pause Scheduler: %w", err)
		}
	}
	cycleID, effectID, err := e.createSchedulerControlCycle(ctx, tx, optimization.ID, nextEpoch, action, now)
	if err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"cycle_id": cycleID, "action": action, "epoch": nextEpoch})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "scheduler."+action+"d", payload, now); err != nil {
		return Receipt{}, err
	}
	result := mustJSON(schedulerCommandResult{SchedulerStatus: want, Epoch: nextEpoch, CycleID: cycleID, EffectID: effectID})
	return Receipt{ID: e.newID(), Revision: nextRevision, Result: result}, nil
}

func (e *Engine) createSchedulerControlCycle(ctx context.Context, tx *sql.Tx, optimizationID string, epoch int64, action, now string) (string, string, error) {
	cycleID, effectID := e.newID(), e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO scheduler_control_cycles
		(id, optimization_id, epoch, action, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		cycleID, optimizationID, epoch, action, now); err != nil {
		return "", "", fmt.Errorf("create Scheduler control cycle: %w", err)
	}

	query := `SELECT s.id, s.work_id, s.role, s.agent_kind, s.agent_name
		FROM agent_sessions s JOIN works w ON w.id = s.work_id
		WHERE s.status IN ('starting', 'running')`
	if action == "resume" {
		query += ` AND w.status = 'pending'`
	}
	query += ` ORDER BY s.created_at, s.id`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return "", "", fmt.Errorf("read Scheduler control targets: %w", err)
	}
	var resumeSessionIDs []string
	for rows.Next() {
		var sessionID, workID, agentKind, agentName string
		var role WorkRole
		if err := rows.Scan(&sessionID, &workID, &role, &agentKind, &agentName); err != nil {
			_ = rows.Close()
			return "", "", fmt.Errorf("scan Scheduler control target: %w", err)
		}
		message := ""
		if action == "resume" {
			message = schedulerResumeMessage
			resumeSessionIDs = append(resumeSessionIDs, sessionID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_control_actions
			(id, cycle_id, agent_session_id, work_id, role, agent_kind, agent_name, action, message, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), 'pending', ?)`, e.newID(), cycleID, sessionID,
			workID, role, agentKind, agentName, action, message, now); err != nil {
			_ = rows.Close()
			return "", "", fmt.Errorf("create Session control action: %w", err)
		}
	}
	if err := rows.Close(); err != nil {
		return "", "", fmt.Errorf("close Scheduler control targets: %w", err)
	}
	if action == "resume" {
		for _, sessionID := range resumeSessionIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded', updated_at = ?
				WHERE target_agent_session_id = ? AND status = 'waiting'`, now, sessionID); err != nil {
				return "", "", fmt.Errorf("supersede waiting Follow-up on Scheduler resume: %w", err)
			}
		}
	}
	if err := insertEffect(ctx, tx, effectID, optimizationID, "scheduler.control_requested", mustJSON(map[string]string{"cycle_id": cycleID}), now); err != nil {
		return "", "", err
	}
	return cycleID, effectID, nil
}

func shiftWaitingFollowUps(ctx context.Context, tx *sql.Tx, pausedAtText string, resumeAt time.Time) error {
	pausedAt, err := time.Parse(time.RFC3339Nano, pausedAtText)
	if err != nil {
		return domainError(CodeStateCorrupt, "Scheduler pause timestamp is invalid")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, due_at, created_at, last_observed_pane_activity_at
		FROM followup_requests WHERE status = 'waiting'`)
	if err != nil {
		return fmt.Errorf("read waiting Follow-up deadlines: %w", err)
	}
	type update struct{ id, due string }
	var updates []update
	for rows.Next() {
		var id, dueText, createdText string
		var activity sql.NullString
		if err := rows.Scan(&id, &dueText, &createdText, &activity); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan waiting Follow-up deadline: %w", err)
		}
		due, dueErr := time.Parse(time.RFC3339Nano, dueText)
		created, createdErr := time.Parse(time.RFC3339Nano, createdText)
		if dueErr != nil || createdErr != nil {
			_ = rows.Close()
			return domainError(CodeStateCorrupt, "Follow-up deadline timestamp is invalid")
		}
		anchor := pausedAt
		if created.After(anchor) {
			anchor = created
		}
		if activity.Valid {
			observed, parseErr := time.Parse(time.RFC3339Nano, activity.String)
			if parseErr != nil {
				_ = rows.Close()
				return domainError(CodeStateCorrupt, "Follow-up activity timestamp is invalid")
			}
			if observed.After(anchor) {
				anchor = observed
			}
		}
		if anchor.After(resumeAt) {
			anchor = resumeAt
		}
		updates = append(updates, update{id: id, due: due.Add(resumeAt.Sub(anchor)).Format(time.RFC3339Nano)})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close waiting Follow-up deadlines: %w", err)
	}
	for _, item := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET due_at = ?, updated_at = ? WHERE id = ? AND status = 'waiting'`,
			item.due, resumeAt.Format(time.RFC3339Nano), item.id); err != nil {
			return fmt.Errorf("shift Follow-up deadline: %w", err)
		}
	}
	return nil
}

func (e *Engine) SchedulerControlCycle(ctx context.Context, cycleID string) (SchedulerControlCycleView, error) {
	if cycleID == "" {
		return SchedulerControlCycleView{}, errors.New("Scheduler control cycle ID is required")
	}
	var cycle SchedulerControlCycleView
	var completed sql.NullString
	if err := e.db.QueryRowContext(ctx, `SELECT id, epoch, action, status, created_at, completed_at
		FROM scheduler_control_cycles WHERE id = ?`, cycleID).Scan(&cycle.ID, &cycle.Epoch, &cycle.Action,
		&cycle.Status, &cycle.CreatedAt, &completed); errors.Is(err, sql.ErrNoRows) {
		return SchedulerControlCycleView{}, domainError(CodeStateCorrupt, "Scheduler control cycle was not found")
	} else if err != nil {
		return SchedulerControlCycleView{}, fmt.Errorf("read Scheduler control cycle: %w", err)
	}
	cycle.CompletedAt = completed.String
	actions, err := e.schedulerControlActions(ctx, cycleID, false)
	if err != nil {
		return SchedulerControlCycleView{}, err
	}
	cycle.Actions = actions
	return cycle, nil
}

func (e *Engine) PendingSchedulerControlActions(ctx context.Context, cycleID string) ([]SchedulerControlActionView, error) {
	return e.schedulerControlActions(ctx, cycleID, true)
}

func (e *Engine) schedulerControlActions(ctx context.Context, cycleID string, pendingOnly bool) ([]SchedulerControlActionView, error) {
	query := `SELECT id, agent_session_id, work_id, role, agent_kind, agent_name, action, COALESCE(message, ''),
		status, COALESCE(observed_agent_status, ''), COALESCE(error_message, ''), COALESCE(started_at, ''), COALESCE(completed_at, '')
		FROM session_control_actions WHERE cycle_id = ?`
	if pendingOnly {
		query += ` AND status = 'pending'`
	}
	query += ` ORDER BY created_at, id`
	rows, err := e.db.QueryContext(ctx, query, cycleID)
	if err != nil {
		return nil, fmt.Errorf("read Session control actions: %w", err)
	}
	defer rows.Close()
	var actions []SchedulerControlActionView
	for rows.Next() {
		var action SchedulerControlActionView
		if err := rows.Scan(&action.ID, &action.AgentSessionID, &action.WorkID, &action.Role, &action.AgentKind,
			&action.AgentName, &action.Action, &action.Message, &action.Status, &action.ObservedAgentStatus,
			&action.Error, &action.StartedAt, &action.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan Session control action: %w", err)
		}
		actions = append(actions, action)
	}
	return actions, rows.Err()
}

func (e *Engine) BeginSchedulerControlAction(ctx context.Context, actionID, observedStatus string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UTC()
	result, err := e.db.ExecContext(ctx, `UPDATE session_control_actions SET status = 'dispatching',
		observed_agent_status = ?, started_at = ?, suppress_activity_until = ? WHERE id = ? AND status = 'pending'`,
		observedStatus, now.Format(time.RFC3339Nano), now.Add(30*time.Second).Format(time.RFC3339Nano), actionID)
	if err != nil {
		return false, fmt.Errorf("begin Session control action: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (e *Engine) FinishSchedulerControlAction(ctx context.Context, actionID string, status SchedulerControlActionStatus, message string) error {
	if status != SchedulerActionSent && status != SchedulerActionSkipped && status != SchedulerActionFailed && status != SchedulerActionDeliveryUnknown {
		return errors.New("invalid terminal Scheduler control action status")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.db.ExecContext(ctx, `UPDATE session_control_actions SET status = ?, error_message = NULLIF(?, ''), completed_at = ?
		WHERE id = ? AND status = 'dispatching'`, status, message, e.timestamp(), actionID)
	if err != nil {
		return fmt.Errorf("finish Session control action: %w", err)
	}
	return nil
}

func (e *Engine) CompleteSchedulerControlCycle(ctx context.Context, cycleID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var remaining, failures int
	if err := e.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status IN ('pending', 'dispatching') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status IN ('failed', 'delivery_unknown') THEN 1 ELSE 0 END), 0)
		FROM session_control_actions WHERE cycle_id = ?`, cycleID).Scan(&remaining, &failures); err != nil {
		return fmt.Errorf("summarize Scheduler control cycle: %w", err)
	}
	if remaining != 0 {
		return nil
	}
	status := "complete"
	if failures != 0 {
		status = "partial"
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE scheduler_control_cycles SET status = ?, completed_at = ?
		WHERE id = ? AND status = 'pending'`, status, e.timestamp(), cycleID); err != nil {
		return fmt.Errorf("complete Scheduler control cycle: %w", err)
	}
	return nil
}

func (e *Engine) ObserveSchedulerResumePrompt(ctx context.Context, tx *sql.Tx, sessionID, message, now string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE session_control_actions SET status = 'observed', observed_at = ?, completed_at = COALESCE(completed_at, ?)
		WHERE id = (SELECT id FROM session_control_actions WHERE agent_session_id = ? AND action = 'resume' AND message = ?
			AND status IN ('dispatching', 'sent') AND observed_at IS NULL AND suppress_activity_until >= ? ORDER BY created_at DESC LIMIT 1)`,
		now, now, sessionID, message, now)
	if err != nil {
		return false, fmt.Errorf("observe Scheduler resume prompt: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (e *Engine) suppressSchedulerResumePaneActivityTx(ctx context.Context, tx *sql.Tx, sessionID, now string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_control_actions WHERE agent_session_id = ?
		AND action = 'resume' AND status IN ('dispatching', 'sent', 'observed') AND suppress_activity_until >= ?`, sessionID, now).Scan(&count); err != nil {
		return false, fmt.Errorf("read Scheduler resume activity suppression: %w", err)
	}
	return count != 0, nil
}
