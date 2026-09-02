package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const paneActivityNotice = "Follow-up inactivity uses provider activity and best-effort Herdr pane.updated observations; settled-idle (idle or done) pane observations do not reset the deadline."

var defaultFollowUpPolicies = map[WorkRole]FollowUpPolicy{
	RoleBaselineVerification: {MaxMessages: 8, GeneratorMaxAttempts: 3},
	RoleDiagnosis:            {MaxMessages: 5, GeneratorMaxAttempts: 3},
	RoleIteration:            {MaxMessages: 5, GeneratorMaxAttempts: 3},
	RoleIntegration:          {MaxMessages: 8, GeneratorMaxAttempts: 3},
}

func normalizeFollowUpPolicies(configured map[WorkRole]FollowUpPolicy) map[WorkRole]FollowUpPolicy {
	result := make(map[WorkRole]FollowUpPolicy, len(defaultFollowUpPolicies))
	for role, fallback := range defaultFollowUpPolicies {
		policy := configured[role]
		if policy.MaxMessages <= 0 {
			policy.MaxMessages = fallback.MaxMessages
		}
		if policy.GeneratorMaxAttempts <= 0 {
			policy.GeneratorMaxAttempts = fallback.GeneratorMaxAttempts
		}
		result[role] = policy
	}
	return result
}

func (e *Engine) SetFollowUpInactivity(duration time.Duration) error {
	if duration <= 0 {
		return errors.New("Follow-up inactivity must be positive")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.followUpInactivity = duration
	return nil
}

func (e *Engine) SetFollowUpPolicies(policies map[WorkRole]FollowUpPolicy) error {
	for role, policy := range policies {
		if !followUpEligible(role) || policy.MaxMessages <= 0 || policy.GeneratorMaxAttempts <= 0 {
			return fmt.Errorf("invalid Follow-up policy for role %s", role)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.followUpPolicies = normalizeFollowUpPolicies(policies)
	return nil
}

func followUpEligible(role WorkRole) bool {
	descriptor, ok := DescribeRole(role)
	return ok && descriptor.FollowUpEligible
}

func (e *Engine) armFollowUpTx(ctx context.Context, tx *sql.Tx, workID, agentSessionID, providerTurnID string, role WorkRole, observed time.Time) error {
	observed = observed.UTC()
	return e.armFollowUpAtTx(ctx, tx, workID, agentSessionID, providerTurnID, role, observed, observed.Add(e.followUpInactivity), "")
}

func (e *Engine) armFollowUpAtTx(ctx context.Context, tx *sql.Tx, workID, agentSessionID, providerTurnID string, role WorkRole, createdAt, due time.Time, activitySource string) error {
	if !followUpEligible(role) || workID == "" || agentSessionID == "" || providerTurnID == "" {
		return nil
	}
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM works WHERE id = ?`, workID).Scan(&workStatus); err != nil {
		return fmt.Errorf("read Follow-up target Work: %w", err)
	}
	if workStatus != WorkPending {
		return nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM followup_requests WHERE target_work_id = ?
		AND status IN ('waiting', 'generating', 'ready', 'dispatching', 'delivery_unknown')`, workID).Scan(&active); err != nil {
		return fmt.Errorf("read active Follow-up Request: %w", err)
	}
	if active != 0 {
		return nil
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(request_sequence), 0) + 1 FROM followup_requests WHERE target_work_id = ?`, workID).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate Follow-up sequence: %w", err)
	}
	now := createdAt.UTC()
	due = due.UTC()
	policy := e.followUpPolicies[role]
	var delivered int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM followup_requests WHERE target_work_id = ?
		AND status IN ('delivered', 'delivery_unknown')`, workID).Scan(&delivered); err != nil {
		return fmt.Errorf("count delivered Follow-ups: %w", err)
	}
	if delivered >= policy.MaxMessages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO followup_requests
			(id, target_work_id, target_agent_session_id, target_provider_turn_id, target_role, request_sequence,
			 status, inactivity_timeout_ms, due_at, activity_source, target_max_messages, generator_max_attempts, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'target_exhausted', ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`, e.newID(), workID, agentSessionID,
			providerTurnID, role, sequence, e.followUpInactivity.Milliseconds(), now.Format(time.RFC3339Nano),
			activitySource, policy.MaxMessages, policy.GeneratorMaxAttempts, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record Follow-up target exhaustion: %w", err)
		}
		return e.exhaustFollowUpTargetTx(ctx, tx, workID, role, "target_message_budget_exhausted")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO followup_requests
		(id, target_work_id, target_agent_session_id, target_provider_turn_id, target_role, request_sequence,
		 status, inactivity_timeout_ms, due_at, activity_source, target_max_messages, generator_max_attempts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'waiting', ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`, e.newID(), workID, agentSessionID, providerTurnID, role,
		sequence, e.followUpInactivity.Milliseconds(), due.Format(time.RFC3339Nano), activitySource, policy.MaxMessages, policy.GeneratorMaxAttempts,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("arm Follow-up Request: %w", err)
	}
	return nil
}

// ObserveAgentStatus consumes Herdr's current Agent state. An idle or done Agent is
// only Follow-up eligible when its current provider Turn is still running and
// provider activity has gone quiet for the configured timeout. Pane
// observations from a settled-idle Agent never reset a waiting Follow-up deadline.
func (e *Engine) ObserveAgentStatus(ctx context.Context, paneID, agentStatus string) error {
	if paneID == "" {
		return errors.New("pane ID is required")
	}
	settledIdle := strings.EqualFold(agentStatus, "idle") || strings.EqualFold(agentStatus, "done")
	if !settledIdle {
		return e.ObservePaneActivity(ctx, paneID)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin idle Agent observation: %w", err)
	}
	defer tx.Rollback()
	var workID, sessionID string
	var role WorkRole
	var workStatus WorkStatus
	err = tx.QueryRowContext(ctx, `SELECT s.work_id, s.id, s.role, w.status
		FROM pane_bindings b JOIN agent_sessions s ON s.id = b.agent_session_id
		JOIN works w ON w.id = s.work_id
		WHERE b.pane_id = ? AND b.current = 1 AND s.status IN ('starting', 'running')`, paneID).
		Scan(&workID, &sessionID, &role, &workStatus)
	if errors.Is(err, sql.ErrNoRows) || !followUpEligible(role) || workStatus != WorkPending {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("resolve idle Agent target: %w", err)
	}
	var providerSessionID, providerTurnID string
	err = tx.QueryRowContext(ctx, `SELECT provider_session_id, provider_turn_id
		FROM conversation_turns WHERE agent_session_id = ? AND status = 'running'
		ORDER BY started_at DESC, id DESC LIMIT 1`, sessionID).Scan(&providerSessionID, &providerTurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("read idle Agent provider Turn: %w", err)
	}
	var lastActivityText sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MAX(observed_at) FROM provider_events
		WHERE agent_session_id = ? AND provider_session_id = ? AND provider_turn_id = ?`,
		sessionID, providerSessionID, providerTurnID).Scan(&lastActivityText); err != nil {
		return fmt.Errorf("read idle Agent provider activity: %w", err)
	}
	if !lastActivityText.Valid {
		return tx.Commit()
	}
	lastActivity, err := time.Parse(time.RFC3339Nano, lastActivityText.String)
	if err != nil {
		return domainError(CodeStateCorrupt, "provider activity timestamp is invalid")
	}
	var latestDeliveryText sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MAX(delivered_at) FROM followup_requests
		WHERE target_work_id = ? AND delivered_at IS NOT NULL`, workID).Scan(&latestDeliveryText); err != nil {
		return fmt.Errorf("read latest Follow-up delivery: %w", err)
	}
	anchor := lastActivity
	if latestDeliveryText.Valid {
		latestDelivery, parseErr := time.Parse(time.RFC3339Nano, latestDeliveryText.String)
		if parseErr != nil {
			return domainError(CodeStateCorrupt, "Follow-up delivery timestamp is invalid")
		}
		if latestDelivery.After(anchor) {
			anchor = latestDelivery
		}
	}
	now := e.now().UTC()
	if now.Before(anchor.Add(e.followUpInactivity)) {
		return tx.Commit()
	}
	if err := e.armFollowUpAtTx(ctx, tx, workID, sessionID, providerTurnID, role, now, anchor.Add(e.followUpInactivity), "provider_activity_timeout"); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) exhaustFollowUpTargetTx(ctx context.Context, tx *sql.Tx, workID string, role WorkRole, reason string) error {
	var optimizationID, baselineID string
	var attemptID, integrationID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT optimization_id, baseline_revision_id, attempt_id, integration_id FROM works WHERE id = ?`, workID).
		Scan(&optimizationID, &baselineID, &attemptID, &integrationID); err != nil {
		return fmt.Errorf("read exhausted Follow-up target: %w", err)
	}
	now := e.timestamp()
	var revision int64
	var optimizationStatus OptimizationStatus
	if err := tx.QueryRowContext(ctx, `SELECT revision, status FROM optimizations WHERE id = ?`, optimizationID).Scan(&revision, &optimizationStatus); err != nil {
		return err
	}
	revision++
	switch role {
	case RoleBaselineVerification:
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, WorkCancelled, now, workID, WorkPending); err != nil {
			return err
		}
		if optimizationStatus == OptimizationDraining {
			if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, revision, now, optimizationID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationPaused, revision, now, optimizationID); err != nil {
				return err
			}
		}
	case RoleDiagnosis:
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, WorkCancelled, now, workID, WorkPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE diagnoses SET status = ?, finished_at = ? WHERE work_id = ? AND status = ?`, DiagnosisCancelled, now, workID, DiagnosisPending); err != nil {
			return err
		}
		if optimizationStatus == OptimizationDraining {
			if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, revision, now, optimizationID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationPaused, revision, now, optimizationID); err != nil {
			return err
		}
	case RoleIteration:
		if !attemptID.Valid {
			return domainError(CodeStateCorrupt, "exhausted Iteration has no Attempt")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, WorkCompleted, now, workID, WorkPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE iteration_rounds SET status = 'rejected', finished_at = ? WHERE attempt_id = ? AND round = (SELECT iteration_round FROM works WHERE id = ?)`, now, attemptID.String, workID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'rejected', summary = ?, failure_reason = 'followup_exhausted', updated_at = ? WHERE id = ?`, reason, now, attemptID.String); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE experiment_cycles
			SET status = CASE WHEN EXISTS (SELECT 1 FROM iteration_experiments e WHERE e.work_id = ?) THEN 'completed' ELSE 'abandoned' END,
			failure_reason = 'followup_exhausted', completed_at = ?
			WHERE id = (SELECT experiment_cycle_id FROM works WHERE id = ?) AND status = 'iteration_active'`, workID, now, workID); err != nil {
			return fmt.Errorf("close exhausted flow v3 Experiment Cycle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, revision, now, optimizationID); err != nil {
			return err
		}
		if optimizationStatus != OptimizationDraining {
			if err := e.fillIterationSlots(ctx, tx, optimizationID, baselineID, now); err != nil {
				return err
			}
		}
	case RoleIntegration:
		if !attemptID.Valid || !integrationID.Valid {
			return domainError(CodeStateCorrupt, "exhausted Integration has incomplete identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, WorkCompleted, now, workID, WorkPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE integrations SET status = 'rejected', result_json = ?, finished_at = ? WHERE id = ?`, mustJSON(map[string]string{"reason": reason}), now, integrationID.String); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = 'rejected', failure_reason = 'followup_exhausted', updated_at = ? WHERE id = ?`, now, attemptID.String); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, revision, now, optimizationID); err != nil {
			return err
		}
		if optimizationStatus != OptimizationDraining {
			if err := e.scheduleIntegrationHead(ctx, tx, optimizationID, baselineID, now); err != nil {
				return err
			}
			if err := e.fillIterationSlots(ctx, tx, optimizationID, baselineID, now); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("Follow-up exhaustion for role %s is not supported", role)
	}
	if err := insertEvent(ctx, tx, e.newID(), optimizationID, revision, "followup.target_exhausted", mustJSON(map[string]string{"work_id": workID, "role": string(role), "reason": reason}), now); err != nil {
		return err
	}
	return insertEffect(ctx, tx, e.newID(), optimizationID, "work.close_requested", mustJSON(map[string]string{"work_id": workID}), now)
}

// ObservePaneActivity records Herdr's approximate pane.updated signal. It never
// claims that a human supplied input.
func (e *Engine) ObservePaneActivity(ctx context.Context, paneID string) error {
	if paneID == "" {
		return errors.New("pane ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pane activity: %w", err)
	}
	defer tx.Rollback()
	var workID, sessionID string
	var role WorkRole
	var workStatus WorkStatus
	err = tx.QueryRowContext(ctx, `SELECT s.work_id, s.id, s.role, w.status
		FROM pane_bindings b JOIN agent_sessions s ON s.id = b.agent_session_id
		JOIN works w ON w.id = s.work_id
		WHERE b.pane_id = ? AND b.current = 1 AND s.status IN ('starting', 'running')`, paneID).
		Scan(&workID, &sessionID, &role, &workStatus)
	if errors.Is(err, sql.ErrNoRows) || !followUpEligible(role) || workStatus != WorkPending {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("resolve pane activity target: %w", err)
	}
	nowTime := e.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	if suppress, err := e.suppressSchedulerResumePaneActivityTx(ctx, tx, sessionID, now); err != nil {
		return err
	} else if suppress {
		return tx.Commit()
	}
	due := nowTime.Add(e.followUpInactivity).Format(time.RFC3339Nano)
	var requestID, status string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM followup_requests WHERE target_work_id = ?
		AND status IN ('waiting', 'generating', 'ready', 'dispatching', 'delivery_unknown') ORDER BY sequence DESC LIMIT 1`, workID).
		Scan(&requestID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("read pane activity Follow-up Request: %w", err)
	}
	if status == "waiting" {
		if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET due_at = ?, last_observed_pane_activity_at = ?,
			activity_source = 'pane.updated', updated_at = ? WHERE id = ?`, due, now, now, requestID); err != nil {
			return fmt.Errorf("reset Follow-up deadline: %w", err)
		}
		return tx.Commit()
	}
	// Prompting the target through Herdr itself emits pane.updated while the
	// request is dispatching. That update is delivery output, not intervening
	// human activity, and must not supersede the message that caused it.
	if status == "dispatching" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded',
		last_observed_pane_activity_at = ?, activity_source = 'pane.updated', updated_at = ? WHERE id = ?`, now, now, requestID); err != nil {
		return fmt.Errorf("supersede Follow-up generation: %w", err)
	}
	var providerTurnID string
	err = tx.QueryRowContext(ctx, `SELECT provider_turn_id FROM conversation_turns WHERE agent_session_id = ? AND status = 'stopped'
		ORDER BY stopped_at DESC, id DESC LIMIT 1`, sessionID).Scan(&providerTurnID)
	if err == nil {
		if err := e.armFollowUpTx(ctx, tx, workID, sessionID, providerTurnID, role, nowTime); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read stopped Turn after pane activity: %w", err)
	}
	return tx.Commit()
}

// PromoteDueFollowUps starts at most one generator Work globally. The caller may
// invoke it repeatedly or after restart; all decisions are durable.
func (e *Engine) PromoteDueFollowUps(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin due Follow-up promotion: %w", err)
	}
	defer tx.Rollback()
	var schedulerStatus SchedulerStatus
	err = tx.QueryRowContext(ctx, `SELECT scheduler_status FROM optimizations LIMIT 1`).Scan(&schedulerStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("read Scheduler state for Follow-up promotion: %w", err)
	}
	if schedulerStatus == SchedulerPaused {
		return false, tx.Commit()
	}
	var activeGenerators int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM works WHERE role = ? AND status = ?`, RoleFollowUp, WorkPending).Scan(&activeGenerators); err != nil {
		return false, fmt.Errorf("count Follow-up generators: %w", err)
	}
	if activeGenerators != 0 {
		return false, tx.Commit()
	}
	var requestID, targetWorkID, targetSessionID, baselineID, optimizationID string
	var targetRole WorkRole
	var requestSequence int64
	err = tx.QueryRowContext(ctx, `SELECT f.id, f.target_work_id, f.target_agent_session_id, f.target_role,
		f.request_sequence, w.baseline_revision_id, w.optimization_id
		FROM followup_requests f JOIN works w ON w.id = f.target_work_id
		WHERE f.status = 'waiting' AND f.due_at <= ? ORDER BY f.due_at, f.sequence LIMIT 1`, e.timestamp()).
		Scan(&requestID, &targetWorkID, &targetSessionID, &targetRole, &requestSequence, &baselineID, &optimizationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("read due Follow-up Request: %w", err)
	}
	var targetStatus WorkStatus
	var currentSessionCount int
	if err := tx.QueryRowContext(ctx, `SELECT status FROM works WHERE id = ?`, targetWorkID).Scan(&targetStatus); err != nil {
		return false, fmt.Errorf("revalidate Follow-up target: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_sessions WHERE id = ? AND work_id = ? AND status IN ('starting', 'running')`, targetSessionID, targetWorkID).Scan(&currentSessionCount); err != nil {
		return false, fmt.Errorf("revalidate Follow-up target session: %w", err)
	}
	if targetStatus != WorkPending || currentSessionCount != 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'cancelled', updated_at = ? WHERE id = ?`, e.timestamp(), requestID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	generatorWorkID := e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id, optimization_id, baseline_revision_id, role, status, generation, parent_work_id, followup_request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, generatorWorkID, optimizationID, baselineID, RoleFollowUp, WorkPending,
		requestSequence, targetWorkID, requestID, e.timestamp()); err != nil {
		return false, fmt.Errorf("create Follow-up generator Work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'generating', generator_work_id = ?, updated_at = ? WHERE id = ? AND status = 'waiting'`, generatorWorkID, e.timestamp(), requestID); err != nil {
		return false, fmt.Errorf("start Follow-up generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET generator_attempts = 1 WHERE id = ?`, requestID); err != nil {
		return false, fmt.Errorf("record Follow-up generator attempt: %w", err)
	}
	if err := insertEffect(ctx, tx, e.newID(), optimizationID, "work.start_requested", mustJSON(map[string]string{"work_id": generatorWorkID, "reason": "follow_up_due"}), e.timestamp()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit Follow-up promotion: %w", err)
	}
	return true, nil
}

func (e *Engine) applySubmitFollowUpMessage(ctx context.Context, tx *sql.Tx, command SubmitFollowUpMessage) (Receipt, error) {
	if command.WorkID == "" || command.Message == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and message are required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	var requestID, targetWorkID, targetSessionID, requestStatus string
	var workStatus, targetStatus WorkStatus
	var role WorkRole
	err = tx.QueryRowContext(ctx, `SELECT f.id, f.target_work_id, f.target_agent_session_id, f.status,
		gw.status, gw.role, tw.status FROM works gw JOIN followup_requests f ON f.id = gw.followup_request_id
		JOIN works tw ON tw.id = f.target_work_id WHERE gw.id = ?`, command.WorkID).
		Scan(&requestID, &targetWorkID, &targetSessionID, &requestStatus, &workStatus, &role, &targetStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeInvalidTransition, "Follow-up generator Work was not found")
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("read Follow-up generator Work: %w", err)
	}
	if role != RoleFollowUp || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "Work is not an active Follow-up generator")
	}
	now := e.timestamp()
	deliver := requestStatus == "generating" && targetStatus == WorkPending
	if deliver {
		var current int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_sessions WHERE id = ? AND work_id = ? AND status IN ('starting', 'running')`, targetSessionID, targetWorkID).Scan(&current); err != nil {
			return Receipt{}, err
		}
		deliver = current == 1
	}
	newRequestStatus := "superseded"
	if deliver {
		newRequestStatus = "ready"
	}
	deliveryID := e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = ?, message = ?, delivery_id = ?, updated_at = ? WHERE id = ?`, newRequestStatus, command.Message, deliveryID, now, requestID); err != nil {
		return Receipt{}, fmt.Errorf("store generated Follow-up: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, fmt.Errorf("complete Follow-up generator Work: %w", err)
	}
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, err
	}
	payload := mustJSON(map[string]any{"followup_request_id": requestID, "generator_work_id": command.WorkID, "target_work_id": targetWorkID, "delivery_id": deliveryID, "deliver": deliver})
	if err := insertEvent(ctx, tx, e.newID(), optimization.ID, nextRevision, "followup.message_submitted", payload, now); err != nil {
		return Receipt{}, err
	}
	if deliver {
		if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "followup.deliver_requested", mustJSON(map[string]string{"request_id": requestID, "target_work_id": targetWorkID, "message": command.Message, "delivery_id": deliveryID}), now); err != nil {
			return Receipt{}, err
		}
	}
	if err := insertEffect(ctx, tx, e.newID(), optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: e.newID(), Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) BeginFollowUpDelivery(ctx context.Context, requestID, deliveryID string) (string, string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	var targetWorkID, targetSessionID, status, storedDelivery string
	var targetStatus WorkStatus
	err = tx.QueryRowContext(ctx, `SELECT f.target_work_id, f.target_agent_session_id, f.status, f.delivery_id, w.status
		FROM followup_requests f JOIN works w ON w.id = f.target_work_id WHERE f.id = ?`, requestID).
		Scan(&targetWorkID, &targetSessionID, &status, &storedDelivery, &targetStatus)
	if err != nil {
		return "", "", false, err
	}
	if status != "ready" || storedDelivery != deliveryID || targetStatus != WorkPending {
		if status == "ready" {
			_, _ = tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded', updated_at = ? WHERE id = ?`, e.timestamp(), requestID)
		}
		return "", "", false, tx.Commit()
	}
	var paneID string
	err = tx.QueryRowContext(ctx, `SELECT b.pane_id FROM agent_sessions s JOIN pane_bindings b ON b.agent_session_id = s.id
		WHERE s.id = ? AND s.work_id = ? AND s.status IN ('starting', 'running') AND b.current = 1`, targetSessionID, targetWorkID).Scan(&paneID)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded', updated_at = ? WHERE id = ?`, e.timestamp(), requestID)
		return "", "", false, tx.Commit()
	}
	if err != nil {
		return "", "", false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'dispatching', updated_at = ? WHERE id = ? AND status = 'ready'`, e.timestamp(), requestID); err != nil {
		return "", "", false, err
	}
	return targetWorkID, paneID, true, tx.Commit()
}

func (e *Engine) FinishFollowUpDelivery(ctx context.Context, requestID string, delivered bool) error {
	status := "delivery_unknown"
	if delivered {
		status = "delivered"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.db.ExecContext(ctx, `UPDATE followup_requests SET status = ?, delivered_at = CASE WHEN ? THEN ? ELSE delivered_at END,
		updated_at = ? WHERE id = ? AND status = 'dispatching'`, status, delivered, e.timestamp(), e.timestamp(), requestID)
	return err
}
