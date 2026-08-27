package symphony

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (e *Engine) FreezeInstruction(ctx context.Context, candidate InstructionSnapshot) (InstructionSnapshot, error) {
	if candidate.AgentSessionID == "" || candidate.LogicalName == "" || candidate.SourcePath == "" || candidate.ContentSHA256 == "" || len(candidate.SystemPrompt) == 0 || candidate.ActivationSHA256 == "" {
		return InstructionSnapshot{}, errors.New("complete instruction snapshot is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.db.ExecContext(ctx, `INSERT INTO instruction_snapshots
		(agent_session_id, logical_name, source_path, content_sha256, content, system_prompt, activation_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(agent_session_id) DO NOTHING`,
		candidate.AgentSessionID, candidate.LogicalName, candidate.SourcePath, candidate.ContentSHA256,
		candidate.Content, candidate.SystemPrompt, candidate.ActivationSHA256, e.timestamp())
	if err != nil {
		return InstructionSnapshot{}, fmt.Errorf("freeze instruction: %w", err)
	}
	// Version 10 added the complete rendered System Prompt. Backfill only legacy
	// snapshots that could not have frozen this value; never replace a non-empty
	// prompt for an existing session.
	if _, err := e.db.ExecContext(ctx, `UPDATE instruction_snapshots SET system_prompt = ?, activation_sha256 = ?
		WHERE agent_session_id = ? AND length(system_prompt) = 0`, candidate.SystemPrompt, candidate.ActivationSHA256, candidate.AgentSessionID); err != nil {
		return InstructionSnapshot{}, fmt.Errorf("backfill frozen System Prompt: %w", err)
	}
	var stored InstructionSnapshot
	err = e.db.QueryRowContext(ctx, `SELECT agent_session_id, logical_name, source_path, content_sha256, content, system_prompt, activation_sha256
		FROM instruction_snapshots WHERE agent_session_id = ?`, candidate.AgentSessionID).Scan(
		&stored.AgentSessionID, &stored.LogicalName, &stored.SourcePath, &stored.ContentSHA256, &stored.Content, &stored.SystemPrompt, &stored.ActivationSHA256,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return InstructionSnapshot{}, domainError(CodeStateCorrupt, "instruction snapshot was not stored")
	}
	if err != nil {
		return InstructionSnapshot{}, fmt.Errorf("read frozen instruction: %w", err)
	}
	if stored.AgentSessionID != candidate.AgentSessionID || stored.LogicalName != candidate.LogicalName || stored.SourcePath != candidate.SourcePath {
		return stored, domainError(CodeStateCorrupt, "agent session instruction identity is already frozen differently")
	}
	return stored, nil
}
