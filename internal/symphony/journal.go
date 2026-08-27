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

	"github.com/reyoung/pika-go/internal/provider"
)

type ProviderEventView struct {
	Sequence          int64           `json:"sequence"`
	Provider          string          `json:"provider"`
	HookEventName     string          `json:"hook_event_name"`
	ProviderSessionID string          `json:"provider_session_id"`
	ProviderTurnID    string          `json:"provider_turn_id,omitempty"`
	Raw               json.RawMessage `json:"raw"`
}

type ConversationTurnView struct {
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ProviderTurnID   string `json:"provider_turn_id"`
	Status           string `json:"status"`
	UserMessage      string `json:"user_message,omitempty"`
	AssistantMessage string `json:"assistant_message,omitempty"`
}

type ToolEventView struct {
	ID                string          `json:"id"`
	Provider          string          `json:"provider"`
	ProviderTurnID    string          `json:"provider_turn_id"`
	ProviderToolUseID string          `json:"provider_tool_use_id"`
	ToolName          string          `json:"tool_name"`
	Input             json.RawMessage `json:"input"`
	Output            json.RawMessage `json:"output,omitempty"`
	Status            string          `json:"status"`
	ErrorMessage      string          `json:"error_message,omitempty"`
	FailureType       string          `json:"failure_type,omitempty"`
	DurationMS        int64           `json:"duration_ms,omitempty"`
	Interrupted       bool            `json:"interrupted,omitempty"`
}

type ToolSupplementView struct {
	ID             string          `json:"id"`
	Provider       string          `json:"provider"`
	ProviderTurnID string          `json:"provider_turn_id"`
	Kind           string          `json:"kind"`
	ToolName       string          `json:"tool_name,omitempty"`
	ServerName     string          `json:"server_name,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	Output         json.RawMessage `json:"output"`
	DurationMS     int64           `json:"duration_ms,omitempty"`
}

type ConversationJournalView struct {
	WorkID            string                 `json:"work_id"`
	ProviderSessionID string                 `json:"provider_session_id,omitempty"`
	Events            []ProviderEventView    `json:"events"`
	Turns             []ConversationTurnView `json:"turns"`
	Tools             []ToolEventView        `json:"tools"`
	ToolSupplements   []ToolSupplementView   `json:"tool_supplements"`
}

func (e *Engine) IngestProviderEvent(ctx context.Context, providerKind, agentSessionID string, raw json.RawMessage) error {
	return e.ingestProviderEvent(ctx, providerKind, agentSessionID, "", raw)
}

func (e *Engine) IngestProviderHookEvent(ctx context.Context, providerKind, agentSessionID, hookEventName string, raw json.RawMessage) error {
	return e.ingestProviderEvent(ctx, providerKind, agentSessionID, hookEventName, raw)
}

func (e *Engine) ingestProviderEvent(ctx context.Context, providerKind, agentSessionID, hookEventName string, raw json.RawMessage) error {
	if providerKind == "" || agentSessionID == "" || len(raw) == 0 {
		return domainError(CodeInvalidCommand, "supported provider, Pika Agent Session, and event are required")
	}
	adapter, err := e.providers.Resolve(providerKind)
	if err != nil {
		return domainError(CodeInvalidCommand, err.Error())
	}
	events, err := adapter.Normalize(provider.SessionBinding{AgentSessionID: agentSessionID, HookEventName: hookEventName}, raw)
	if err != nil {
		return domainError(CodeInvalidCommand, err.Error())
	}
	if len(events) == 0 {
		return domainError(CodeInvalidCommand, "provider Adapter produced no Journal Events")
	}
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
	var agentKind string
	var boundProvider, boundSession sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.work_id, s.role, w.status, s.agent_kind, s.provider, s.provider_session_id
		FROM agent_sessions s JOIN works w ON w.id = s.work_id WHERE s.id = ?`, agentSessionID).
		Scan(&workID, &role, &workStatus, &agentKind, &boundProvider, &boundSession); errors.Is(err, sql.ErrNoRows) {
		return domainError(CodeForbidden, "Pika Agent Session was not found")
	} else if err != nil {
		return fmt.Errorf("read provider Agent Session: %w", err)
	}
	if agentKind != providerKind {
		return domainError(CodeForbidden, "provider route does not match the Pika Agent Session kind")
	}
	for _, event := range events {
		if event.Provider != providerKind || event.ProviderSessionID == "" || event.HookEventName == "" || len(event.Raw) == 0 {
			return domainError(CodeInvalidCommand, "provider Adapter produced an invalid Journal Event")
		}
		if (boundProvider.Valid && boundProvider.String != event.Provider) || (boundSession.Valid && boundSession.String != event.ProviderSessionID) {
			return domainError(CodeForbidden, "provider identity does not match the Pika Agent Session")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET provider = ?, provider_session_id = ? WHERE id = ?
			AND (provider_session_id IS NULL OR provider_session_id = ?)`, event.Provider, event.ProviderSessionID, agentSessionID, event.ProviderSessionID); err != nil {
			return fmt.Errorf("bind provider Agent Session: %w", err)
		}
		boundProvider, boundSession = sql.NullString{String: event.Provider, Valid: true}, sql.NullString{String: event.ProviderSessionID, Valid: true}
		inserted, err := e.recordProviderEvent(ctx, tx, agentSessionID, event, now)
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		if err := e.applyJournalEvent(ctx, tx, agentSessionID, workID, role, workStatus, event, observedTime, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider hook: %w", err)
	}
	return nil
}

func (e *Engine) recordProviderEvent(ctx context.Context, tx *sql.Tx, agentSessionID string, event provider.JournalEvent, now string) (bool, error) {
	digestBytes := sha256.Sum256(event.Raw)
	digest := hex.EncodeToString(digestBytes[:])
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO provider_events
		(id, provider, agent_session_id, provider_session_id, provider_turn_id, hook_event_name, event_digest, raw_json, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.newID(), event.Provider, agentSessionID, event.ProviderSessionID,
		nullable(event.ProviderTurnID), event.HookEventName, digest, event.Raw, now)
	if err != nil {
		return false, fmt.Errorf("record provider event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count provider event: %w", err)
	}
	return inserted != 0, nil
}

func (e *Engine) applyJournalEvent(ctx context.Context, tx *sql.Tx, agentSessionID, workID string, role WorkRole, workStatus WorkStatus, event provider.JournalEvent, observedTime time.Time, now string) error {
	var turnID string
	var err error
	if event.ProviderTurnID != "" {
		turnID, err = e.ensureConversationTurn(ctx, tx, event.Provider, agentSessionID, workID, event.ProviderSessionID, event.ProviderTurnID, now)
		if err != nil {
			return err
		}
	}
	switch event.Kind {
	case provider.JournalUserMessage:
		if turnID == "" {
			return domainError(CodeInvalidCommand, "user message requires a provider Turn identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET user_message = ?,
			status = CASE WHEN stopped_at IS NULL THEN 'running' ELSE status END WHERE id = ?`, event.UserMessage, turnID); err != nil {
			return fmt.Errorf("record user prompt: %w", err)
		}
		// A provider reports the prompt injected by followup delivery as another
		// user message. BeginFollowUpDelivery moves the request to dispatching
		// before sending that prompt, so keep that state protected just like the
		// matching pane.updated notification. Genuine user input while waiting,
		// generating, or ready still supersedes the stale guidance.
		if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'superseded', updated_at = ?
			WHERE target_work_id = ? AND status IN ('waiting', 'generating', 'ready')`, now, workID); err != nil {
			return fmt.Errorf("supersede Follow-up after user prompt: %w", err)
		}
	case provider.JournalAssistantMessage:
		if turnID == "" {
			return domainError(CodeInvalidCommand, "assistant message requires a provider Turn identity")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET assistant_message = ? WHERE id = ?`, event.AssistantMessage, turnID); err != nil {
			return fmt.Errorf("record assistant message: %w", err)
		}
	case provider.JournalToolCompleted:
		if turnID == "" || event.Tool == nil || event.Tool.ID == "" || event.Tool.Name == "" || len(event.Tool.Input) == 0 {
			return domainError(CodeInvalidCommand, "completed Tool identity and input are required")
		}
		if err := e.upsertToolEvent(ctx, tx, agentSessionID, turnID, event, now); err != nil {
			return fmt.Errorf("record tool event: %w", err)
		}
	case provider.JournalToolFailed:
		if turnID == "" || event.Tool == nil || event.Tool.ID == "" || event.Tool.Name == "" || len(event.Tool.Input) == 0 {
			return domainError(CodeInvalidCommand, "failed Tool identity and input are required")
		}
		if err := e.upsertToolEvent(ctx, tx, agentSessionID, turnID, event, now); err != nil {
			return fmt.Errorf("record failed tool event: %w", err)
		}
	case provider.JournalToolSupplement:
		if turnID == "" || event.Supplement == nil || event.Supplement.Kind == "" || len(event.Supplement.Output) == 0 {
			return domainError(CodeInvalidCommand, "Tool supplement identity and output are required")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_event_supplements
			(id, provider, agent_session_id, conversation_turn_id, provider_session_id, provider_turn_id, supplement_kind,
			 tool_name, server_name, input_json, output_json, duration_ms, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.newID(), event.Provider, agentSessionID, turnID,
			event.ProviderSessionID, event.ProviderTurnID, event.Supplement.Kind, nullable(event.Supplement.Name), nullable(event.Supplement.ServerName),
			nullableBytes(event.Supplement.Input), event.Supplement.Output, nullableInt64(event.Supplement.DurationMS), now); err != nil {
			return fmt.Errorf("record Tool supplement: %w", err)
		}
	case provider.JournalTurnStopped:
		if turnID == "" {
			return domainError(CodeInvalidCommand, "Turn stop requires a provider Turn identity")
		}
		status := event.TurnStatus
		if status == "" {
			status = "stopped"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_turns SET assistant_message = COALESCE(?, assistant_message), status = ?, stopped_at = ? WHERE id = ?`, nullable(event.AssistantMessage), status, now, turnID); err != nil {
			return fmt.Errorf("record Turn stop: %w", err)
		}
		if workStatus == WorkPending {
			if err := e.armFollowUpTx(ctx, tx, workID, agentSessionID, event.ProviderTurnID, role, observedTime); err != nil {
				return err
			}
		}
	}
	if event.ProviderSessionEnded || event.Kind == provider.JournalSessionEnded {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET provider_ended_at = ? WHERE id = ?`, now, agentSessionID); err != nil {
			return fmt.Errorf("record provider session end: %w", err)
		}
	}
	return nil
}

func (e *Engine) upsertToolEvent(ctx context.Context, tx *sql.Tx, agentSessionID, turnID string, event provider.JournalEvent, now string) error {
	status := event.Tool.Status
	if status == "" {
		status = "completed"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tool_events
		(id, provider, agent_session_id, conversation_turn_id, provider_session_id, provider_turn_id, provider_tool_use_id, tool_name, input_json,
		 output_json, status, error_message, failure_type, duration_ms, is_interrupt, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, agent_session_id, provider_session_id, provider_tool_use_id) DO UPDATE SET
		tool_name = excluded.tool_name, input_json = excluded.input_json, output_json = excluded.output_json,
		status = excluded.status, error_message = excluded.error_message, failure_type = excluded.failure_type,
		duration_ms = excluded.duration_ms, is_interrupt = excluded.is_interrupt, observed_at = excluded.observed_at`,
		e.newID(), event.Provider, agentSessionID, turnID, event.ProviderSessionID, event.ProviderTurnID, event.Tool.ID, event.Tool.Name, event.Tool.Input,
		nullableBytes(event.Tool.Output), status, nullable(event.Tool.ErrorMessage), nullable(event.Tool.FailureType), nullableInt64(event.Tool.DurationMS), event.Tool.Interrupted, now)
	return err
}

func (e *Engine) ensureConversationTurn(ctx context.Context, tx *sql.Tx, providerKind, agentSessionID, workID, providerSessionID, providerTurnID, now string) (string, error) {
	var turnID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_turns WHERE provider = ? AND agent_session_id = ? AND provider_session_id = ? AND provider_turn_id = ?`,
		providerKind, agentSessionID, providerSessionID, providerTurnID).Scan(&turnID)
	if err == nil {
		return turnID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read Conversation Turn: %w", err)
	}
	turnID = e.newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_turns
		(id, provider, agent_session_id, work_id, provider_session_id, provider_turn_id, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 'running', ?)`, turnID, providerKind, agentSessionID, workID, providerSessionID, providerTurnID, now); err != nil {
		return "", fmt.Errorf("create Conversation Turn: %w", err)
	}
	return turnID, nil
}

func (e *Engine) ConversationJournal(ctx context.Context, workID string) (ConversationJournalView, error) {
	journal := ConversationJournalView{WorkID: workID}
	if err := e.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(provider_session_id), '') FROM agent_sessions WHERE work_id = ?`, workID).Scan(&journal.ProviderSessionID); err != nil {
		return ConversationJournalView{}, fmt.Errorf("read provider session: %w", err)
	}
	eventRows, err := e.db.QueryContext(ctx, `SELECT pe.sequence, pe.provider, pe.hook_event_name, pe.provider_session_id,
		pe.provider_turn_id, pe.raw_json FROM provider_events pe JOIN agent_sessions s ON s.id = pe.agent_session_id
		WHERE s.work_id = ? ORDER BY pe.sequence`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	for eventRows.Next() {
		var event ProviderEventView
		var turnID sql.NullString
		if err := eventRows.Scan(&event.Sequence, &event.Provider, &event.HookEventName, &event.ProviderSessionID, &turnID, &event.Raw); err != nil {
			_ = eventRows.Close()
			return ConversationJournalView{}, err
		}
		event.ProviderTurnID = turnID.String
		journal.Events = append(journal.Events, event)
	}
	_ = eventRows.Close()
	turnRows, err := e.db.QueryContext(ctx, `SELECT id, provider, provider_turn_id, status, user_message, assistant_message
		FROM conversation_turns WHERE work_id = ? ORDER BY started_at, id`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	for turnRows.Next() {
		var turn ConversationTurnView
		var user, assistant sql.NullString
		if err := turnRows.Scan(&turn.ID, &turn.Provider, &turn.ProviderTurnID, &turn.Status, &user, &assistant); err != nil {
			_ = turnRows.Close()
			return ConversationJournalView{}, err
		}
		turn.UserMessage, turn.AssistantMessage = user.String, assistant.String
		journal.Turns = append(journal.Turns, turn)
	}
	_ = turnRows.Close()
	toolRows, err := e.db.QueryContext(ctx, `SELECT te.id, te.provider, te.provider_turn_id, te.provider_tool_use_id, te.tool_name, te.input_json, te.output_json,
		te.status, te.error_message, te.failure_type, te.duration_ms, te.is_interrupt
		FROM tool_events te JOIN conversation_turns ct ON ct.id = te.conversation_turn_id WHERE ct.work_id = ? ORDER BY te.observed_at, te.id`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	defer toolRows.Close()
	for toolRows.Next() {
		var tool ToolEventView
		var output []byte
		var errorMessage, failureType sql.NullString
		var duration sql.NullInt64
		if err := toolRows.Scan(&tool.ID, &tool.Provider, &tool.ProviderTurnID, &tool.ProviderToolUseID, &tool.ToolName, &tool.Input, &output,
			&tool.Status, &errorMessage, &failureType, &duration, &tool.Interrupted); err != nil {
			return ConversationJournalView{}, err
		}
		tool.Output = output
		tool.ErrorMessage, tool.FailureType, tool.DurationMS = errorMessage.String, failureType.String, duration.Int64
		journal.Tools = append(journal.Tools, tool)
	}
	if err := toolRows.Err(); err != nil {
		return ConversationJournalView{}, err
	}
	supplementRows, err := e.db.QueryContext(ctx, `SELECT tes.id, tes.provider, tes.provider_turn_id, tes.supplement_kind, tes.tool_name,
		tes.server_name, tes.input_json, tes.output_json, tes.duration_ms
		FROM tool_event_supplements tes JOIN conversation_turns ct ON ct.id = tes.conversation_turn_id
		WHERE ct.work_id = ? ORDER BY tes.sequence`, workID)
	if err != nil {
		return ConversationJournalView{}, err
	}
	defer supplementRows.Close()
	for supplementRows.Next() {
		var supplement ToolSupplementView
		var toolName, serverName sql.NullString
		var input []byte
		var duration sql.NullInt64
		if err := supplementRows.Scan(&supplement.ID, &supplement.Provider, &supplement.ProviderTurnID, &supplement.Kind, &toolName,
			&serverName, &input, &supplement.Output, &duration); err != nil {
			return ConversationJournalView{}, err
		}
		supplement.ToolName, supplement.ServerName, supplement.Input, supplement.DurationMS = toolName.String, serverName.String, input, duration.Int64
		journal.ToolSupplements = append(journal.ToolSupplements, supplement)
	}
	return journal, supplementRows.Err()
}

func canonicalValue(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
