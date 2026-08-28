package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ContextProjection returns the complete committed input needed to freeze one
// Agent Session. Role-specific selection belongs here so Context Bundle callers
// do not need to understand Symphony tables or lifecycle states.
func (e *Engine) ContextProjection(ctx context.Context, sessionID string) (ContextProjection, error) {
	if sessionID == "" {
		return ContextProjection{}, errors.New("agent session ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	session, err := e.ReadAgentSession(ctx, sessionID)
	if err != nil {
		return ContextProjection{}, err
	}
	generator, err := e.RuntimeWork(ctx, session.WorkID)
	if err != nil {
		return ContextProjection{}, err
	}
	target := generator
	if session.Role == RoleFollowUp {
		target, err = e.RuntimeWork(ctx, generator.FollowUpTargetWorkID)
		if err != nil {
			return ContextProjection{}, err
		}
	}
	view, err := e.Inspect(ctx, Status{})
	if err != nil {
		return ContextProjection{}, err
	}
	journal, err := e.normalizedConversationJournal(ctx, target.Work.ID)
	if err != nil {
		return ContextProjection{}, err
	}
	projection := ContextProjection{Session: session, View: view, TargetWork: target, GeneratorWork: generator.Work, Journal: journal}
	if target.Work.Role == RoleIteration && target.IterationHistoryLimit > 0 {
		projection.AttemptHistories, err = e.recentAttemptHistories(ctx, target.OptimizationID, target.Work.AttemptID, target.IterationHistoryLimit)
		if err != nil {
			return ContextProjection{}, err
		}
	}
	return projection, nil
}

func (e *Engine) recentAttemptHistories(ctx context.Context, optimizationID, currentAttemptID string, limit int64) ([]AttemptHistoryProjection, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, slot_index, status, base_best_sequence, base_sha,
		current_iteration_round, candidate_sha, summary, failure_reason, history_limit
		FROM attempts WHERE optimization_id = ? AND id <> ? AND status IN ('accepted', 'rejected')
		ORDER BY updated_at DESC, id DESC LIMIT ?`, optimizationID, currentAttemptID, limit)
	if err != nil {
		return nil, fmt.Errorf("select recent Attempt histories: %w", err)
	}
	defer rows.Close()
	var newestFirst []AttemptView
	for rows.Next() {
		var attempt AttemptView
		var candidate, summary, failure sql.NullString
		if err := rows.Scan(&attempt.ID, &attempt.SlotIndex, &attempt.Status, &attempt.BaseBestSequence, &attempt.BaseSHA,
			&attempt.CurrentIterationRound, &candidate, &summary, &failure, &attempt.HistoryLimit); err != nil {
			return nil, fmt.Errorf("scan recent Attempt history: %w", err)
		}
		attempt.CandidateSHA, attempt.Summary, attempt.FailureReason = candidate.String, summary.String, failure.String
		newestFirst = append(newestFirst, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent Attempt histories: %w", err)
	}
	histories := make([]AttemptHistoryProjection, 0, len(newestFirst))
	for index := len(newestFirst) - 1; index >= 0; index-- {
		attempt := newestFirst[index]
		journal, err := e.attemptConversationJournal(ctx, attempt.ID)
		if err != nil {
			return nil, err
		}
		histories = append(histories, AttemptHistoryProjection{Attempt: attempt, Journal: journal})
	}
	return histories, nil
}

func (e *Engine) attemptConversationJournal(ctx context.Context, attemptID string) (ConversationJournalView, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM works WHERE attempt_id = ? AND role = ? ORDER BY iteration_round, created_at, id`, attemptID, RoleIteration)
	if err != nil {
		return ConversationJournalView{}, fmt.Errorf("read Attempt Work history: %w", err)
	}
	var workIDs []string
	for rows.Next() {
		var workID string
		if err := rows.Scan(&workID); err != nil {
			_ = rows.Close()
			return ConversationJournalView{}, fmt.Errorf("scan Attempt Work history: %w", err)
		}
		workIDs = append(workIDs, workID)
	}
	if err := rows.Close(); err != nil {
		return ConversationJournalView{}, fmt.Errorf("close Attempt Work history: %w", err)
	}
	journal := ConversationJournalView{WorkID: attemptID}
	for _, workID := range workIDs {
		part, err := e.normalizedConversationJournal(ctx, workID)
		if err != nil {
			return ConversationJournalView{}, err
		}
		journal.Turns = append(journal.Turns, part.Turns...)
		journal.Tools = append(journal.Tools, part.Tools...)
		journal.ToolSupplements = append(journal.ToolSupplements, part.ToolSupplements...)
	}
	return journal, nil
}

func (e *Engine) ReadContextSnapshot(ctx context.Context, sessionID string) (ContextSnapshot, bool, error) {
	if sessionID == "" {
		return ContextSnapshot{}, false, errors.New("agent session ID is required")
	}
	var snapshot ContextSnapshot
	err := e.db.QueryRowContext(ctx, `SELECT agent_session_id, schema_version, context_relative_path, context_sha256,
		context_bytes, messages_relative_path, messages_sha256, messages_bytes, message_records
		FROM context_snapshots WHERE agent_session_id = ?`, sessionID).Scan(
		&snapshot.AgentSessionID, &snapshot.SchemaVersion, &snapshot.ContextRelativePath, &snapshot.ContextSHA256,
		&snapshot.ContextBytes, &snapshot.MessagesRelativePath, &snapshot.MessagesSHA256, &snapshot.MessagesBytes, &snapshot.MessageRecords)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextSnapshot{}, false, nil
	}
	if err != nil {
		return ContextSnapshot{}, false, fmt.Errorf("read Context Snapshot: %w", err)
	}
	return snapshot, true, nil
}

func (e *Engine) FreezeContextSnapshot(ctx context.Context, candidate ContextSnapshot) (ContextSnapshot, error) {
	if candidate.AgentSessionID == "" || candidate.SchemaVersion < 1 || candidate.ContextRelativePath == "" ||
		candidate.ContextSHA256 == "" || candidate.ContextBytes < 1 || candidate.MessagesRelativePath == "" ||
		candidate.MessagesSHA256 == "" || candidate.MessagesBytes < 0 || candidate.MessageRecords < 0 {
		return ContextSnapshot{}, errors.New("complete Context Snapshot is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.db.ExecContext(ctx, `INSERT INTO context_snapshots
		(agent_session_id, schema_version, context_relative_path, context_sha256, context_bytes,
		 messages_relative_path, messages_sha256, messages_bytes, message_records, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(agent_session_id) DO NOTHING`,
		candidate.AgentSessionID, candidate.SchemaVersion, candidate.ContextRelativePath, candidate.ContextSHA256,
		candidate.ContextBytes, candidate.MessagesRelativePath, candidate.MessagesSHA256, candidate.MessagesBytes,
		candidate.MessageRecords, e.timestamp())
	if err != nil {
		return ContextSnapshot{}, fmt.Errorf("freeze Context Snapshot: %w", err)
	}
	stored, found, err := e.ReadContextSnapshot(ctx, candidate.AgentSessionID)
	if err != nil {
		return ContextSnapshot{}, err
	}
	if !found {
		return ContextSnapshot{}, domainError(CodeStateCorrupt, "Context Snapshot was not stored")
	}
	if stored != candidate {
		return stored, domainError(CodeStateCorrupt, "agent session Context Snapshot is already frozen differently")
	}
	return stored, nil
}
