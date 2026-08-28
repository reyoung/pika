package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

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
	return ContextProjection{Session: session, View: view, TargetWork: target, GeneratorWork: generator.Work, Journal: journal}, nil
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
		&snapshot.ContextBytes, &snapshot.MessagesRelativePath, &snapshot.MessagesSHA256, &snapshot.MessagesBytes, &snapshot.MessageRecords,
	)
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
