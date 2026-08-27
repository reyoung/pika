package symphony

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ProviderEventView struct {
	Sequence          int64           `json:"sequence"`
	HookEventName     string          `json:"hook_event_name"`
	ProviderSessionID string          `json:"provider_session_id"`
	ProviderTurnID    string          `json:"provider_turn_id,omitempty"`
	Raw               json.RawMessage `json:"raw"`
}

type ConversationTurnView struct {
	ID               string `json:"id"`
	ProviderTurnID   string `json:"provider_turn_id"`
	Status           string `json:"status"`
	UserMessage      string `json:"user_message,omitempty"`
	AssistantMessage string `json:"assistant_message,omitempty"`
}

type ToolEventView struct {
	ID                string          `json:"id"`
	ProviderTurnID    string          `json:"provider_turn_id"`
	ProviderToolUseID string          `json:"provider_tool_use_id"`
	ToolName          string          `json:"tool_name"`
	Input             json.RawMessage `json:"input"`
	Output            json.RawMessage `json:"output,omitempty"`
}

type ConversationJournalView struct {
	WorkID            string                 `json:"work_id"`
	ProviderSessionID string                 `json:"provider_session_id,omitempty"`
	Events            []ProviderEventView    `json:"events"`
	Turns             []ConversationTurnView `json:"turns"`
	Tools             []ToolEventView        `json:"tools"`
}

type codexHookEnvelope struct {
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id"`
	HookEventName        string          `json:"hook_event_name"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
}

func (e *Engine) IngestProviderEvent(ctx context.Context, provider, agentSessionID string, raw json.RawMessage) error {
	if provider != "codex" || agentSessionID == "" || len(raw) == 0 {
		return domainError(CodeInvalidCommand, "supported provider, Pika Agent Session, and event are required")
	}
	var envelope codexHookEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.SessionID == "" || envelope.HookEventName == "" {
		return domainError(CodeInvalidCommand, "provider hook envelope is invalid")
	}
	var canonical any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return domainError(CodeInvalidCommand, "provider hook envelope must be JSON")
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		return fmt.Errorf("canonicalize provider hook: %w", err)
	}
	digestBytes := sha256.Sum256(canonicalJSON)
	digest := hex.EncodeToString(digestBytes[:])
	observedTime := e.now().UTC()
	now := observedTime.Format(time.RFC3339Nano)

	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider hook: %w", err)
	}
	defer tx.Rollback()
	var workID string
	var role WorkRole
	var workStatus WorkStatus
	var boundProvider, boundSession sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.work_id, s.role, w.status, s.provider, s.provider_session_id
		FROM agent_sessions s JOIN works w ON w.id = s.work_id WHERE s.id = ?`, agentSessionID).
		Scan(&workID, &role, &workStatus, &boundProvider, &boundSession); errors.Is(err, sql.ErrNoRows) {
		return domainError(CodeForbidden, "Pika Agent Session was not found")
	} else if err != nil {
		return fmt.Errorf("read provider Agent Session: %w", err)
	}
	if (boundProvider.Valid && boundProvider.String != provider) || (boundSession.Valid && boundSession.String != envelope.SessionID) {
		return domainError(CodeForbidden, "provider identity does not match the Pika Agent Session")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET provider = ?, provider_session_id = ? WHERE id = ?
		AND (provider_session_id IS NULL OR provider_session_id = ?)`, provider, envelope.SessionID, agentSessionID, envelope.SessionID); err != nil {
		return fmt.Errorf("bind provider Agent Session: %w", err)
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO provider_events
		(id, provider, agent_session_id, provider_session_id, provider_turn_id, hook_event_name, event_digest, raw_json, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.newID(), provider, agentSessionID, envelope.SessionID,
		nullable(envelope.TurnID), envelope.HookEventName, digest, canonicalJSON, now)
	if err != nil {
		return fmt.Errorf("record provider event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count provider event: %w", err)
	}
	if inserted == 0 {
		return tx.Commit()
	}
	if envelope.TurnID != "" {
		turnID, err := e.ensureConversationTurn(ctx, tx, agentSessionID, workID, envelope.SessionID, envelope.TurnID, now)
		if err != nil {
			return err
		}
		switch envelope.HookEventName {
		case "UserPromptSubmit":
			if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET user_message = ?, status = 'running' WHERE id = ?`, envelope.Prompt, turnID); err != nil {
				return fmt.Errorf("record user prompt: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded', updated_at = ?
				WHERE target_work_id = ? AND status IN ('waiting', 'generating', 'ready', 'dispatching')`, now, workID); err != nil {
				return fmt.Errorf("supersede Follow-up after user prompt: %w", err)
			}
		case "PostToolUse":
			if envelope.ToolUseID == "" || envelope.ToolName == "" || len(envelope.ToolInput) == 0 {
				return domainError(CodeInvalidCommand, "PostToolUse identity and input are required")
			}
			input, err := canonicalValue(envelope.ToolInput)
			if err != nil {
				return domainError(CodeInvalidCommand, "tool input must be valid JSON")
			}
			var output []byte
			if len(envelope.ToolResponse) != 0 && string(envelope.ToolResponse) != "null" {
				output, err = canonicalValue(envelope.ToolResponse)
				if err != nil {
					return domainError(CodeInvalidCommand, "tool response must be valid JSON")
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tool_events
				(id, conversation_turn_id, provider_session_id, provider_turn_id, provider_tool_use_id, tool_name, input_json, output_json, observed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(provider_session_id, provider_tool_use_id) DO UPDATE SET
				tool_name = excluded.tool_name, input_json = excluded.input_json, output_json = excluded.output_json,
				observed_at = excluded.observed_at`, e.newID(), turnID, envelope.SessionID, envelope.TurnID,
				envelope.ToolUseID, envelope.ToolName, input, nullableBytes(output), now); err != nil {
				return fmt.Errorf("record tool event: %w", err)
			}
		case "Stop":
			if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET assistant_message = ?, status = 'stopped', stopped_at = ? WHERE id = ?`, nullable(envelope.LastAssistantMessage), now, turnID); err != nil {
				return fmt.Errorf("record Turn stop: %w", err)
			}
			if workStatus == WorkPending {
				if err := e.armFollowUpTx(ctx, tx, workID, agentSessionID, envelope.TurnID, role, observedTime); err != nil {
					return err
				}
			}
		}
	}
	if envelope.HookEventName == "SessionEnd" {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET provider_ended_at = ? WHERE id = ?`, now, agentSessionID); err != nil {
			return fmt.Errorf("record provider session end: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider hook: %w", err)
	}
	return nil
}

func (e *Engine) ensureConversationTurn(ctx context.Context, tx *sql.Tx, agentSessionID, workID, providerSessionID, providerTurnID, now string) (string, error) {
	var turnID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_turns WHERE provider_session_id = ? AND provider_turn_id = ?`, providerSessionID, providerTurnID).Scan(&turnID)
	if err == nil {
		return turnID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read Conversation Turn: %w", err)
	}
	turnID = e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_turns
		(id, agent_session_id, work_id, provider_session_id, provider_turn_id, status, started_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?)`, turnID, agentSessionID, workID, providerSessionID, providerTurnID, now); err != nil {
		return "", fmt.Errorf("create Conversation Turn: %w", err)
	}
	return turnID, nil
}

func (e *Engine) ConversationJournal(ctx context.Context, workID string) (ConversationJournalView, error) {
	journal := ConversationJournalView{WorkID: workID}
	if err := e.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(provider_session_id), '') FROM agent_sessions WHERE work_id = ?`, workID).Scan(&journal.ProviderSessionID); err != nil {
		return ConversationJournalView{}, fmt.Errorf("read provider session: %w", err)
	}
	eventRows, err := e.db.QueryContext(ctx, `SELECT pe.sequence, pe.hook_event_name, pe.provider_session_id,
		pe.provider_turn_id, pe.raw_json FROM provider_events pe JOIN agent_sessions s ON s.id = pe.agent_session_id
		WHERE s.work_id = ? ORDER BY pe.sequence`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	for eventRows.Next() {
		var event ProviderEventView
		var turnID sql.NullString
		if err := eventRows.Scan(&event.Sequence, &event.HookEventName, &event.ProviderSessionID, &turnID, &event.Raw); err != nil {
			_ = eventRows.Close()
			return ConversationJournalView{}, err
		}
		event.ProviderTurnID = turnID.String
		journal.Events = append(journal.Events, event)
	}
	_ = eventRows.Close()
	turnRows, err := e.db.QueryContext(ctx, `SELECT id, provider_turn_id, status, user_message, assistant_message
		FROM conversation_turns WHERE work_id = ? ORDER BY started_at, id`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	for turnRows.Next() {
		var turn ConversationTurnView
		var user, assistant sql.NullString
		if err := turnRows.Scan(&turn.ID, &turn.ProviderTurnID, &turn.Status, &user, &assistant); err != nil {
			_ = turnRows.Close()
			return ConversationJournalView{}, err
		}
		turn.UserMessage, turn.AssistantMessage = user.String, assistant.String
		journal.Turns = append(journal.Turns, turn)
	}
	_ = turnRows.Close()
	toolRows, err := e.db.QueryContext(ctx, `SELECT te.id, te.provider_turn_id, te.provider_tool_use_id, te.tool_name, te.input_json, te.output_json
		FROM tool_events te JOIN conversation_turns ct ON ct.id = te.conversation_turn_id WHERE ct.work_id = ? ORDER BY te.observed_at, te.id`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	defer toolRows.Close()
	for toolRows.Next() {
		var tool ToolEventView
		var output []byte
		if err := toolRows.Scan(&tool.ID, &tool.ProviderTurnID, &tool.ProviderToolUseID, &tool.ToolName, &tool.Input, &output); err != nil {
			return ConversationJournalView{}, err
		}
		tool.Output = output
		journal.Tools = append(journal.Tools, tool)
	}
	return journal, toolRows.Err()
}

func canonicalValue(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
