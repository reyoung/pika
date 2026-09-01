package symphony

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
	"github.com/reyoung/pika-go/internal/candidatepolicy"
	"github.com/reyoung/pika-go/internal/provider"
	_ "modernc.org/sqlite"
)

type Options struct {
	Now                func() time.Time
	NewID              func() string
	FollowUpInactivity time.Duration
	FollowUpPolicies   map[WorkRole]FollowUpPolicy
	ApplyCheckpoint    func(ApplyCheckpoint, string) error
	Providers          *provider.Registry
	// AllowIterationCaseMigration opens a legacy Optimizing workspace only for
	// the explicit offline Iteration Case migration command.
	AllowIterationCaseMigration bool
}

type ApplyCheckpoint string

const (
	ApplyBeforeCommit ApplyCheckpoint = "before_commit"
	ApplyAfterCommit  ApplyCheckpoint = "after_commit"
)

type Engine struct {
	db                 *sql.DB
	databasePath       string
	now                func() time.Time
	newID              func() string
	mu                 sync.Mutex
	followUpInactivity time.Duration
	followUpPolicies   map[WorkRole]FollowUpPolicy
	applyCheckpoint    func(ApplyCheckpoint, string) error
	providers          *provider.Registry
}

func Open(ctx context.Context, path string, options Options) (*Engine, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewID == nil {
		options.NewID = randomID
	}
	if options.FollowUpInactivity <= 0 {
		options.FollowUpInactivity = 5 * time.Minute
	}
	if options.Providers == nil {
		options.Providers = provider.DefaultRegistry()
	}
	if err := prepareDatabasePath(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(err error) (*Engine, error) {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping sqlite: %w", err))
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return closeOnError(fmt.Errorf("configure sqlite with %q: %w", pragma, err))
		}
	}
	if err := migrate(ctx, db, options.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return closeOnError(err)
	}
	var quickCheck string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&quickCheck); err != nil {
		return closeOnError(fmt.Errorf("validate sqlite database: %w", err))
	}
	if quickCheck != "ok" {
		return closeOnError(fmt.Errorf("validate sqlite database: %s", quickCheck))
	}
	if err := validateOnlineState(ctx, db, options.AllowIterationCaseMigration); err != nil {
		return closeOnError(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return closeOnError(fmt.Errorf("set database permissions: %w", err))
	}
	return &Engine{db: db, databasePath: path, now: options.Now, newID: options.NewID, followUpInactivity: options.FollowUpInactivity,
		followUpPolicies: normalizeFollowUpPolicies(options.FollowUpPolicies), applyCheckpoint: options.ApplyCheckpoint, providers: options.Providers}, nil
}

func (e *Engine) Close() error { return e.db.Close() }

func (e *Engine) RecordInitFailure(ctx context.Context, message string) error {
	if message == "" {
		return errors.New("init failure message is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.db.ExecContext(ctx, `INSERT INTO init_diagnostics(id, message, created_at) VALUES (?, ?, ?)`, e.newID(), message, e.timestamp()); err != nil {
		return fmt.Errorf("record init failure diagnostic: %w", err)
	}
	return nil
}

func (e *Engine) InitDiagnostics(ctx context.Context) ([]InitDiagnostic, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, message, created_at FROM init_diagnostics ORDER BY sequence`)
	if err != nil {
		return nil, fmt.Errorf("read init diagnostics: %w", err)
	}
	defer rows.Close()
	var diagnostics []InitDiagnostic
	for rows.Next() {
		var diagnostic InitDiagnostic
		if err := rows.Scan(&diagnostic.ID, &diagnostic.Message, &diagnostic.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan init diagnostic: %w", err)
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate init diagnostics: %w", err)
	}
	return diagnostics, nil
}

func (e *Engine) PendingEffects(ctx context.Context, limit int) ([]RuntimeEffect, error) {
	if limit <= 0 {
		return nil, errors.New("effect limit must be positive")
	}
	rows, err := e.db.QueryContext(ctx, `SELECT sequence, id, optimization_id, effect_type, payload_json
        FROM runtime_outbox WHERE status = 'pending' ORDER BY sequence LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read pending runtime effects: %w", err)
	}
	defer rows.Close()
	var effects []RuntimeEffect
	for rows.Next() {
		var effect RuntimeEffect
		var payload []byte
		if err := rows.Scan(&effect.Sequence, &effect.ID, &effect.OptimizationID, &effect.Type, &payload); err != nil {
			return nil, fmt.Errorf("scan pending runtime effect: %w", err)
		}
		effect.Payload = payload
		effects = append(effects, effect)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending runtime effects: %w", err)
	}
	return effects, nil
}

func (e *Engine) ClaimPendingEffects(ctx context.Context, limit int) ([]RuntimeEffect, error) {
	if limit <= 0 {
		return nil, errors.New("effect limit must be positive")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin runtime effect claim: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT sequence, id, optimization_id, effect_type, payload_json
		FROM runtime_outbox WHERE status = 'pending'
		AND (effect_type NOT IN ('work.start_requested', 'followup.deliver_requested')
			OR EXISTS (SELECT 1 FROM optimizations o WHERE o.id = runtime_outbox.optimization_id AND o.scheduler_status = 'running'))
		ORDER BY CASE WHEN effect_type = 'scheduler.control_requested' THEN 0 ELSE 1 END, sequence LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read claimable runtime effects: %w", err)
	}
	var effects []RuntimeEffect
	for rows.Next() {
		var effect RuntimeEffect
		var payload []byte
		if err := rows.Scan(&effect.Sequence, &effect.ID, &effect.OptimizationID, &effect.Type, &payload); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan claimable runtime effect: %w", err)
		}
		effect.Payload = payload
		effects = append(effects, effect)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close claimable runtime effects: %w", err)
	}
	for _, effect := range effects {
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_outbox
			SET status = 'dispatching', attempts = attempts + 1, claimed_at = ?
			WHERE id = ? AND status = 'pending'`, e.timestamp(), effect.ID); err != nil {
			return nil, fmt.Errorf("claim runtime effect %s: %w", effect.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit runtime effect claim: %w", err)
	}
	return effects, nil
}

func (e *Engine) ClaimRuntimeEffect(ctx context.Context, id string) (RuntimeEffect, bool, error) {
	if id == "" {
		return RuntimeEffect{}, false, errors.New("runtime effect ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return RuntimeEffect{}, false, fmt.Errorf("begin exact runtime effect claim: %w", err)
	}
	defer tx.Rollback()
	var effect RuntimeEffect
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT sequence, id, optimization_id, effect_type, payload_json
		FROM runtime_outbox WHERE id = ? AND status = 'pending'
		AND (effect_type NOT IN ('work.start_requested', 'followup.deliver_requested')
			OR EXISTS (SELECT 1 FROM optimizations o WHERE o.id = runtime_outbox.optimization_id AND o.scheduler_status = 'running'))`, id).Scan(
		&effect.Sequence, &effect.ID, &effect.OptimizationID, &effect.Type, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeEffect{}, false, nil
	}
	if err != nil {
		return RuntimeEffect{}, false, fmt.Errorf("read exact runtime effect: %w", err)
	}
	effect.Payload = payload
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_outbox SET status = 'dispatching', attempts = attempts + 1, claimed_at = ?
		WHERE id = ? AND status = 'pending'`, e.timestamp(), id); err != nil {
		return RuntimeEffect{}, false, fmt.Errorf("claim exact runtime effect: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RuntimeEffect{}, false, fmt.Errorf("commit exact runtime effect claim: %w", err)
	}
	return effect, true, nil
}

func (e *Engine) RecoverUncertainEffects(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.db.ExecContext(ctx, `UPDATE session_control_actions
		SET status = 'delivery_unknown', error_message = 'daemon stopped while control delivery was in progress', completed_at = ?
		WHERE status = 'dispatching'`, e.timestamp()); err != nil {
		return fmt.Errorf("recover uncertain scheduler control actions: %w", err)
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE runtime_outbox SET status = 'pending', claimed_at = NULL WHERE status = 'dispatching'`); err != nil {
		return fmt.Errorf("recover uncertain runtime effects: %w", err)
	}
	return nil
}

func (e *Engine) MarkEffectDispatched(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("effect ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result, err := e.db.ExecContext(ctx, `UPDATE runtime_outbox SET status = 'dispatched' WHERE id = ? AND status = 'dispatching'`, id)
	if err != nil {
		return fmt.Errorf("acknowledge runtime effect: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count acknowledged runtime effect: %w", err)
	}
	if count == 0 {
		var status string
		if err := e.db.QueryRowContext(ctx, `SELECT status FROM runtime_outbox WHERE id = ?`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return domainError(CodeStateCorrupt, "runtime effect was not found")
		} else if err != nil {
			return fmt.Errorf("read runtime effect acknowledgement: %w", err)
		}
		if status != "dispatched" {
			return domainError(CodeInvalidTransition, "runtime effect was not claimed")
		}
	}
	return nil
}

func (e *Engine) EnsureAgentSession(ctx context.Context, session AgentSession) error {
	if session.ID == "" || session.WorkID == "" || session.Generation < 1 || session.Role == "" || session.AgentKind == "" || session.AgentName == "" {
		return errors.New("complete agent session identity is required")
	}
	if session.Status != AgentSessionStarting && session.Status != AgentSessionRunning {
		return errors.New("new agent session must be starting or running")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.db.ExecContext(ctx, `INSERT INTO agent_sessions
		(id, work_id, generation, role, agent_kind, agent_name, provider_version, provider_capabilities_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)
		ON CONFLICT(id) DO NOTHING`, session.ID, session.WorkID, session.Generation, session.Role, session.AgentKind, session.AgentName,
		session.ProviderVersion, []byte(session.ProviderCapabilities), session.Status, e.timestamp())
	if err != nil {
		return fmt.Errorf("ensure agent session: %w", err)
	}
	var stored AgentSession
	var providerVersion sql.NullString
	if err := e.db.QueryRowContext(ctx, `SELECT id, work_id, generation, role, agent_kind, agent_name,
		COALESCE(provider_capabilities_json, X''), provider_version, status FROM agent_sessions WHERE id = ?`, session.ID).Scan(
		&stored.ID, &stored.WorkID, &stored.Generation, &stored.Role, &stored.AgentKind, &stored.AgentName,
		&stored.ProviderCapabilities, &providerVersion, &stored.Status,
	); err != nil {
		return fmt.Errorf("read ensured agent session: %w", err)
	}
	stored.ProviderVersion = providerVersion.String
	if stored.ID != session.ID || stored.WorkID != session.WorkID || stored.Generation != session.Generation || stored.Role != session.Role || stored.AgentKind != session.AgentKind || stored.AgentName != session.AgentName {
		return domainError(CodeStateCorrupt, "agent session ID is already bound to different identity")
	}
	if session.ProviderVersion != "" && stored.ProviderVersion != session.ProviderVersion ||
		len(session.ProviderCapabilities) != 0 && string(stored.ProviderCapabilities) != string(session.ProviderCapabilities) {
		return domainError(CodeStateCorrupt, "agent session ID is already bound to different provider capabilities")
	}
	if stored.Status != session.Status && !(session.Status == AgentSessionStarting && stored.Status == AgentSessionRunning) {
		return domainError(CodeStateCorrupt, "agent session ID is already bound to incompatible status")
	}
	return nil
}

func (e *Engine) BindPane(ctx context.Context, sessionID string, binding PaneBinding) error {
	if sessionID == "" || binding.WorkspaceID == "" || binding.TabID == "" || binding.PaneID == "" || binding.TerminalID == "" {
		return errors.New("complete pane binding identity is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pane binding transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE pane_bindings SET current = 0 WHERE terminal_id = ? AND agent_session_id != ?`, binding.TerminalID, sessionID); err != nil {
		return fmt.Errorf("retire previous terminal binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pane_bindings
        (id, agent_session_id, workspace_id, tab_id, pane_id, terminal_id, current, last_observed_at)
        VALUES (?, ?, ?, ?, ?, ?, 1, ?)
        ON CONFLICT(agent_session_id) DO UPDATE SET
            workspace_id = excluded.workspace_id,
            tab_id = excluded.tab_id,
            pane_id = excluded.pane_id,
            terminal_id = excluded.terminal_id,
            current = 1,
            last_observed_at = excluded.last_observed_at`,
		e.newID(), sessionID, binding.WorkspaceID, binding.TabID, binding.PaneID, binding.TerminalID, e.timestamp()); err != nil {
		return fmt.Errorf("bind agent session to pane: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET status = ? WHERE id = ? AND status = ?`, AgentSessionRunning, sessionID, AgentSessionStarting); err != nil {
		return fmt.Errorf("mark bound agent session running: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pane binding: %w", err)
	}
	return nil
}

func (e *Engine) ObservePaneMove(ctx context.Context, terminalID string, binding PaneBinding) error {
	if terminalID == "" || terminalID != binding.TerminalID {
		return errors.New("matching terminal identity is required for pane move")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result, err := e.db.ExecContext(ctx, `UPDATE pane_bindings SET workspace_id = ?, tab_id = ?, pane_id = ?, last_observed_at = ?
        WHERE terminal_id = ? AND current = 1`, binding.WorkspaceID, binding.TabID, binding.PaneID, e.timestamp(), terminalID)
	if err != nil {
		return fmt.Errorf("observe pane move: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count moved pane bindings: %w", err)
	}
	if count == 0 {
		return domainError(CodeStateCorrupt, "moved terminal has no current pane binding")
	}
	return nil
}

func (e *Engine) CurrentAgentSession(ctx context.Context, workID string) (AgentSession, PaneBinding, bool, error) {
	var session AgentSession
	var binding PaneBinding
	var workspaceID, tabID, paneID, terminalID sql.NullString
	err := e.db.QueryRowContext(ctx, `SELECT s.id, s.work_id, s.generation, s.role, s.agent_kind, s.agent_name,
		COALESCE(s.provider_version, ''), COALESCE(s.provider_capabilities_json, X''), s.status,
        b.workspace_id, b.tab_id, b.pane_id, b.terminal_id
        FROM agent_sessions s
        LEFT JOIN pane_bindings b ON b.agent_session_id = s.id AND b.current = 1
        WHERE s.work_id = ? AND s.status IN ('starting', 'running')
        ORDER BY s.created_at DESC LIMIT 1`, workID).Scan(
		&session.ID, &session.WorkID, &session.Generation, &session.Role, &session.AgentKind, &session.AgentName,
		&session.ProviderVersion, &session.ProviderCapabilities, &session.Status,
		&workspaceID, &tabID, &paneID, &terminalID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSession{}, PaneBinding{}, false, nil
	}
	if err != nil {
		return AgentSession{}, PaneBinding{}, false, fmt.Errorf("read current agent session: %w", err)
	}
	binding = PaneBinding{WorkspaceID: workspaceID.String, TabID: tabID.String, PaneID: paneID.String, TerminalID: terminalID.String}
	return session, binding, true, nil
}

func (e *Engine) ReadAgentSession(ctx context.Context, sessionID string) (AgentSession, error) {
	if sessionID == "" {
		return AgentSession{}, errors.New("agent session ID is required")
	}
	var session AgentSession
	err := e.db.QueryRowContext(ctx, `SELECT id, work_id, generation, role, agent_kind, agent_name,
		COALESCE(provider_version, ''), COALESCE(provider_capabilities_json, X''), status
		FROM agent_sessions WHERE id = ?`, sessionID).Scan(
		&session.ID, &session.WorkID, &session.Generation, &session.Role, &session.AgentKind, &session.AgentName,
		&session.ProviderVersion, &session.ProviderCapabilities, &session.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSession{}, domainError(CodeStateCorrupt, "agent session was not found")
	}
	if err != nil {
		return AgentSession{}, fmt.Errorf("read agent session: %w", err)
	}
	return session, nil
}

// AgentSessionBinding returns the durable pane identity for a Session,
// including bindings retired after the Session exited. A binding is the
// authoritative marker that the runtime successfully launched and observed
// the Session; a starting record alone is only scheduling intent.
func (e *Engine) AgentSessionBinding(ctx context.Context, sessionID string) (PaneBinding, bool, error) {
	if sessionID == "" {
		return PaneBinding{}, false, errors.New("agent session ID is required")
	}
	var binding PaneBinding
	err := e.db.QueryRowContext(ctx, `SELECT workspace_id, tab_id, pane_id, terminal_id
		FROM pane_bindings WHERE agent_session_id = ?`, sessionID).Scan(
		&binding.WorkspaceID, &binding.TabID, &binding.PaneID, &binding.TerminalID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PaneBinding{}, false, nil
	}
	if err != nil {
		return PaneBinding{}, false, fmt.Errorf("read agent session pane binding: %w", err)
	}
	return binding, true, nil
}

// AgentSessionHistory returns every Pika Agent Session recorded for one Work.
// Recovery diagnostics use it instead of inferring history from the
// currently active pane, which may already have completed by observation time.
func (e *Engine) AgentSessionHistory(ctx context.Context, workID string) ([]AgentSession, error) {
	if workID == "" {
		return nil, errors.New("work ID is required")
	}
	rows, err := e.db.QueryContext(ctx, `SELECT id, work_id, generation, role, agent_kind, agent_name,
		COALESCE(provider_version, ''), COALESCE(provider_capabilities_json, X''), status
		FROM agent_sessions WHERE work_id = ? ORDER BY generation, created_at, id`, workID)
	if err != nil {
		return nil, fmt.Errorf("read Agent Session history: %w", err)
	}
	defer rows.Close()
	var history []AgentSession
	for rows.Next() {
		var session AgentSession
		if err := rows.Scan(&session.ID, &session.WorkID, &session.Generation, &session.Role, &session.AgentKind, &session.AgentName,
			&session.ProviderVersion, &session.ProviderCapabilities, &session.Status); err != nil {
			return nil, fmt.Errorf("scan Agent Session history: %w", err)
		}
		history = append(history, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Agent Session history: %w", err)
	}
	return history, nil
}

func (e *Engine) RuntimeWork(ctx context.Context, workID string) (RuntimeWork, error) {
	var runtimeWork RuntimeWork
	var attemptID, integrationID, parentWorkID, followUpRequestID, baseSHA, candidateSHA, iterationKind, backOffMessage, currentCheckpoint sql.NullString
	var expectedBestSHA, integrationStatus, gitIntentID, gitIntentState sql.NullString
	var predecessorBaselineID, baselineRepositorySHA sql.NullString
	var iterationRound, fifoPosition, historyLimit, iterationSlotIndex sql.NullInt64
	var baselineDefinition []byte
	err := e.db.QueryRowContext(ctx, `SELECT w.id, w.baseline_revision_id, w.role, w.status, w.generation,
		w.attempt_id, w.iteration_round, w.integration_id, w.parent_work_id, w.followup_request_id,
		o.id, o.status, o.revision, o.flow_version, o.repository, b.number, b.status, b.definition_json, b.repository_sha, b.predecessor_id,
		a.base_sha, a.candidate_sha, a.slot_index, a.history_limit, r.kind, r.back_off_message, r.current_checkpoint_sha,
		i.expected_best_sha, i.fifo_position, i.status, g.id, g.state
		FROM works w JOIN optimizations o ON o.id = w.optimization_id
		JOIN baseline_revisions b ON b.id = w.baseline_revision_id
		LEFT JOIN attempts a ON a.id = w.attempt_id
		LEFT JOIN iteration_rounds r ON r.attempt_id = w.attempt_id AND r.round = w.iteration_round
		LEFT JOIN integrations i ON i.id = w.integration_id
		LEFT JOIN git_intents g ON g.integration_id = i.id
		WHERE w.id = ?`, workID).Scan(
		&runtimeWork.Work.ID, &runtimeWork.Work.BaselineRevisionID, &runtimeWork.Work.Role,
		&runtimeWork.Work.Status, &runtimeWork.Work.Generation, &attemptID, &iterationRound, &integrationID, &parentWorkID, &followUpRequestID,
		&runtimeWork.OptimizationID, &runtimeWork.OptimizationStatus, &runtimeWork.OptimizationRevision, &runtimeWork.FlowVersion, &runtimeWork.Repository,
		&runtimeWork.BaselineNumber, &runtimeWork.BaselineStatus, &baselineDefinition, &baselineRepositorySHA, &predecessorBaselineID,
		&baseSHA, &candidateSHA, &iterationSlotIndex, &historyLimit, &iterationKind, &backOffMessage, &currentCheckpoint,
		&expectedBestSHA, &fifoPosition, &integrationStatus, &gitIntentID, &gitIntentState,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeWork{}, domainError(CodeWorkNotFound, "work was not found")
	}
	if err != nil {
		return RuntimeWork{}, fmt.Errorf("read runtime work: %w", err)
	}
	runtimeWork.OptimizationRepository = runtimeWork.Repository
	runtimeWork.Work.AttemptID = attemptID.String
	runtimeWork.Work.IterationRound = iterationRound.Int64
	runtimeWork.Work.IntegrationID = integrationID.String
	runtimeWork.Work.ParentWorkID = parentWorkID.String
	runtimeWork.Work.FollowUpRequestID = followUpRequestID.String
	runtimeWork.BaseSHA = baseSHA.String
	runtimeWork.CandidateSHA = candidateSHA.String
	runtimeWork.IterationSlotIndex = iterationSlotIndex.Int64
	runtimeWork.IterationHistoryLimit = historyLimit.Int64
	runtimeWork.IterationKind = iterationKind.String
	runtimeWork.BackOffMessage = backOffMessage.String
	runtimeWork.CurrentCheckpointSHA = currentCheckpoint.String
	runtimeWork.ExpectedBestSHA = expectedBestSHA.String
	runtimeWork.IntegrationFIFOPosition = fifoPosition.Int64
	runtimeWork.IntegrationStatus = integrationStatus.String
	runtimeWork.GitIntentID = gitIntentID.String
	runtimeWork.GitIntentState = gitIntentState.String
	runtimeWork.PredecessorBaselineID = predecessorBaselineID.String
	runtimeWork.BaselineRepositorySHA = baselineRepositorySHA.String
	if len(baselineDefinition) != 0 {
		digest := sha256.Sum256(baselineDefinition)
		runtimeWork.BaselineDefinitionSHA256 = hex.EncodeToString(digest[:])
		policy, present, err := candidatepolicy.Parse(baselineDefinition)
		if err != nil {
			return RuntimeWork{}, fmt.Errorf("parse stored candidate change policy: %w", err)
		}
		if present {
			runtimeWork.CandidateChangePolicy = policy
		}
	}
	if runtimeWork.Work.Role == RoleBaselineDraft && runtimeWork.PredecessorBaselineID != "" {
		var failureKind, failureReason, requestedChanges sql.NullString
		var evidence []byte
		if err := e.db.QueryRowContext(ctx, `SELECT v.failure_kind, v.reason, v.requested_changes, v.evidence_json
			FROM baseline_verifications v WHERE v.baseline_revision_id = ?`, runtimeWork.PredecessorBaselineID).Scan(
			&failureKind, &failureReason, &requestedChanges, &evidence,
		); err != nil {
			return RuntimeWork{}, fmt.Errorf("read predecessor Baseline verification: %w", err)
		}
		runtimeWork.PredecessorFailureKind = failureKind.String
		runtimeWork.PredecessorFailureReason = failureReason.String
		runtimeWork.PredecessorRequestedChanges = requestedChanges.String
		runtimeWork.PredecessorVerificationEvidence = evidence
	}
	if runtimeWork.FlowVersion == FlowVersion2 {
		snapshot, snapshotErr := e.readSkillSnapshot(ctx, runtimeWork.OptimizationID)
		if snapshotErr != nil {
			return RuntimeWork{}, snapshotErr
		}
		runtimeWork.SkillSnapshot = &snapshot
		var diagnosis DiagnosisView
		var report []byte
		diagnosisErr := e.db.QueryRowContext(ctx, `SELECT id, baseline_revision_id, work_id, status, report_json
			FROM diagnoses WHERE baseline_revision_id = ?`, runtimeWork.Work.BaselineRevisionID).Scan(
			&diagnosis.ID, &diagnosis.BaselineRevisionID, &diagnosis.WorkID, &diagnosis.Status, &report)
		if diagnosisErr == nil {
			diagnosis.Report = report
			if len(report) != 0 {
				_, _, hypotheses, countErr := diagnosisReportCounts(report)
				if countErr != nil {
					return RuntimeWork{}, domainError(CodeStateCorrupt, "stored Diagnosis report is invalid JSON")
				}
				diagnosis.HypothesisCount = hypotheses
			}
			runtimeWork.Diagnosis = &diagnosis
		} else if !errors.Is(diagnosisErr, sql.ErrNoRows) {
			return RuntimeWork{}, fmt.Errorf("read runtime Diagnosis: %w", diagnosisErr)
		}
	}
	if runtimeWork.Work.Role == RoleIteration || runtimeWork.Work.Role == RoleIntegration || runtimeWork.Work.Role == RoleDiagnosis {
		if err := e.db.QueryRowContext(ctx, `SELECT sequence, commit_sha FROM best_revisions
			WHERE optimization_id = (SELECT optimization_id FROM works WHERE id = ?)
			ORDER BY sequence DESC LIMIT 1`, workID).Scan(&runtimeWork.BestSequence, &runtimeWork.BestSHA); err != nil {
			return RuntimeWork{}, fmt.Errorf("read runtime Best: %w", err)
		}
		var caseSet IterationCaseSetView
		var err error
		if runtimeWork.Work.Role == RoleIteration {
			caseSet, err = roundIterationCaseSet(ctx, e.db, runtimeWork.Work.AttemptID, runtimeWork.Work.IterationRound)
		} else {
			caseSet, err = currentIterationCaseSet(ctx, e.db, runtimeWork.OptimizationID)
		}
		if err != nil {
			return RuntimeWork{}, err
		}
		runtimeWork.IterationCaseSet = &caseSet
	}
	if (runtimeWork.Work.Role == RoleIteration || runtimeWork.Work.Role == RoleIntegration) && runtimeWork.FlowVersion == FlowVersion2 {
		experiments, experimentsErr := e.readIterationExperiments(ctx, runtimeWork.Work.AttemptID, runtimeWork.Work.IterationRound)
		if experimentsErr != nil {
			return RuntimeWork{}, experimentsErr
		}
		runtimeWork.IterationExperiments = experiments
	}
	if runtimeWork.Work.Role == RoleFollowUp {
		if err := e.db.QueryRowContext(ctx, `SELECT target_work_id, target_role, request_sequence, due_at,
			target_max_messages, generator_max_attempts, generator_attempts
			FROM followup_requests WHERE id = ?`, runtimeWork.Work.FollowUpRequestID).Scan(&runtimeWork.FollowUpTargetWorkID,
			&runtimeWork.FollowUpTargetRole, &runtimeWork.FollowUpSequence, &runtimeWork.FollowUpDueAt,
			&runtimeWork.FollowUpMaxMessages, &runtimeWork.FollowUpGeneratorMax, &runtimeWork.FollowUpGeneratorTry); err != nil {
			return RuntimeWork{}, fmt.Errorf("read Follow-up runtime context: %w", err)
		}
		target, err := e.RuntimeWork(ctx, runtimeWork.FollowUpTargetWorkID)
		if err != nil {
			return RuntimeWork{}, err
		}
		runtimeWork.Repository = target.Repository
		runtimeWork.OptimizationRepository = target.OptimizationRepository
		runtimeWork.BaseSHA, runtimeWork.CandidateSHA = target.BaseSHA, target.CandidateSHA
		runtimeWork.IterationKind, runtimeWork.BackOffMessage = target.IterationKind, target.BackOffMessage
		runtimeWork.IterationHistoryLimit = target.IterationHistoryLimit
		runtimeWork.IterationCaseSet = target.IterationCaseSet
		runtimeWork.FlowVersion, runtimeWork.SkillSnapshot, runtimeWork.Diagnosis = target.FlowVersion, target.SkillSnapshot, target.Diagnosis
		runtimeWork.IterationExperiments, runtimeWork.CurrentCheckpointSHA = target.IterationExperiments, target.CurrentCheckpointSHA
		runtimeWork.BestSHA, runtimeWork.BestSequence = target.BestSHA, target.BestSequence
		runtimeWork.ExpectedBestSHA = target.ExpectedBestSHA
		runtimeWork.IntegrationFIFOPosition, runtimeWork.IntegrationStatus = target.IntegrationFIFOPosition, target.IntegrationStatus
		runtimeWork.GitIntentID, runtimeWork.GitIntentState = target.GitIntentID, target.GitIntentState
	}
	return runtimeWork, nil
}

func (e *Engine) EvidenceArtifacts(ctx context.Context, workID string) ([]EvidenceArtifact, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id, work_id, receipt_id, relative_path, byte_size, content_sha256, contract_version
		FROM evidence_artifacts WHERE work_id = ? ORDER BY created_at, id`, workID)
	if err != nil {
		return nil, fmt.Errorf("read evidence artifacts: %w", err)
	}
	defer rows.Close()
	var artifacts []EvidenceArtifact
	for rows.Next() {
		var artifact EvidenceArtifact
		if err := rows.Scan(&artifact.ID, &artifact.WorkID, &artifact.ReceiptID, &artifact.RelativePath, &artifact.ByteSize, &artifact.ContentSHA256, &artifact.ContractVersion); err != nil {
			return nil, fmt.Errorf("scan evidence artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evidence artifacts: %w", err)
	}
	return artifacts, nil
}

func (e *Engine) GitIntent(ctx context.Context, intentID string) (GitIntentView, error) {
	var intent GitIntentView
	if err := e.db.QueryRowContext(ctx, `SELECT id, integration_id, state, expected_best_sha, candidate_sha
		FROM git_intents WHERE id = ?`, intentID).Scan(&intent.ID, &intent.IntegrationID, &intent.State,
		&intent.ExpectedBestSHA, &intent.CandidateSHA); errors.Is(err, sql.ErrNoRows) {
		return GitIntentView{}, domainError(CodeInvalidCommand, "Git intent was not found")
	} else if err != nil {
		return GitIntentView{}, fmt.Errorf("read Git intent: %w", err)
	}
	return intent, nil
}

func (e *Engine) ActiveAgentSessions(ctx context.Context) ([]ActiveAgentSession, error) {
	return queryActiveAgentSessions(ctx, e.db)
}

// ReadActiveAgentSessions opens an existing Symphony database without
// migrations or filesystem writes. Maintenance-aware open uses it before a
// reviewed daemon is allowed to cold-open and migrate the database.
func ReadActiveAgentSessions(ctx context.Context, path string) ([]ActiveAgentSession, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return queryActiveAgentSessions(ctx, db)
}

type activeAgentSessionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryActiveAgentSessions(ctx context.Context, queryer activeAgentSessionQueryer) ([]ActiveAgentSession, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT s.id, s.work_id, s.generation, s.role, s.agent_kind, s.agent_name,
		COALESCE(s.provider_version, ''), COALESCE(s.provider_capabilities_json, X''), s.status,
        b.workspace_id, b.tab_id, b.pane_id, b.terminal_id
        FROM agent_sessions s
        LEFT JOIN pane_bindings b ON b.agent_session_id = s.id AND b.current = 1
        WHERE s.status IN ('starting', 'running') ORDER BY s.created_at`)
	if err != nil {
		return nil, fmt.Errorf("read active agent sessions: %w", err)
	}
	defer rows.Close()
	var records []ActiveAgentSession
	for rows.Next() {
		var record ActiveAgentSession
		var workspaceID, tabID, paneID, terminalID sql.NullString
		if err := rows.Scan(
			&record.Session.ID, &record.Session.WorkID, &record.Session.Generation, &record.Session.Role,
			&record.Session.AgentKind, &record.Session.AgentName, &record.Session.ProviderVersion,
			&record.Session.ProviderCapabilities, &record.Session.Status,
			&workspaceID, &tabID, &paneID, &terminalID,
		); err != nil {
			return nil, fmt.Errorf("scan active agent session: %w", err)
		}
		record.Binding = PaneBinding{WorkspaceID: workspaceID.String, TabID: tabID.String, PaneID: paneID.String, TerminalID: terminalID.String}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active agent sessions: %w", err)
	}
	return records, nil
}

// DrainReady reports whether a requested graceful shutdown has no remaining
// Work, runtime effects, or child Agent Sessions that Pika must wait for. It
// never changes Work or Session state and therefore cannot make shutdown
// progress by killing or cancelling a child.
func (e *Engine) DrainReady(ctx context.Context) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var status OptimizationStatus
	err := e.db.QueryRowContext(ctx, `SELECT status FROM optimizations LIMIT 1`).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read drain lifecycle: %w", err)
	}
	if status != OptimizationDraining {
		return false, nil
	}
	var pendingWorks, runtimeEffects, activeSessions int64
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works WHERE status = 'pending'`).Scan(&pendingWorks); err != nil {
		return false, fmt.Errorf("count drain Work: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_outbox WHERE status IN ('pending', 'dispatching')`).Scan(&runtimeEffects); err != nil {
		return false, fmt.Errorf("count drain runtime effects: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_sessions WHERE status IN ('starting', 'running')`).Scan(&activeSessions); err != nil {
		return false, fmt.Errorf("count drain Agent Sessions: %w", err)
	}
	return pendingWorks == 0 && runtimeEffects == 0 && activeSessions == 0, nil
}

func (e *Engine) MarkAgentSessionEnded(ctx context.Context, sessionID string, status AgentSessionStatus) error {
	if status != AgentSessionExited && status != AgentSessionLost {
		return errors.New("ended agent session must be exited or lost")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent session end: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET status = ?, ended_at = ? WHERE id = ? AND status IN ('starting', 'running')`, status, e.timestamp(), sessionID)
	if err != nil {
		return fmt.Errorf("end agent session: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count ended agent session: %w", err)
	}
	if count == 0 {
		return domainError(CodeInvalidTransition, "agent session is not current")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pane_bindings SET current = 0, last_observed_at = ? WHERE agent_session_id = ? AND current = 1`, e.timestamp(), sessionID); err != nil {
		return fmt.Errorf("retire ended pane binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_grants SET revoked_at = ? WHERE agent_session_id = ? AND revoked_at IS NULL`, e.timestamp(), sessionID); err != nil {
		return fmt.Errorf("revoke ended agent grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent session end: %w", err)
	}
	return nil
}

func (e *Engine) ReplaceLostAgentSession(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", errors.New("agent session ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin lost agent replacement: %w", err)
	}
	defer tx.Rollback()
	var workID, optimizationID, agentName string
	var status WorkStatus
	var role WorkRole
	var followUpRequestID, paneID, terminalID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.work_id, w.optimization_id, w.status, w.role, w.followup_request_id, s.agent_name,
		(SELECT p.pane_id FROM pane_bindings p WHERE p.agent_session_id = s.id AND p.current = 1),
		(SELECT p.terminal_id FROM pane_bindings p WHERE p.agent_session_id = s.id AND p.current = 1)
		FROM agent_sessions s JOIN works w ON w.id = s.work_id WHERE s.id = ?`, sessionID).Scan(
		&workID, &optimizationID, &status, &role, &followUpRequestID, &agentName, &paneID, &terminalID,
	); errors.Is(err, sql.ErrNoRows) {
		return "", domainError(CodeStateCorrupt, "agent session was not found")
	} else if err != nil {
		return "", fmt.Errorf("read lost agent session: %w", err)
	}
	if status != WorkPending {
		return "", domainError(CodeWorkTerminal, "work is terminal")
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_sessions SET status = ?, ended_at = ?
		WHERE id = ? AND status IN ('starting', 'running')`, AgentSessionLost, e.timestamp(), sessionID)
	if err != nil {
		return "", fmt.Errorf("mark agent session lost: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("count lost agent session: %w", err)
	}
	if count == 0 {
		return "", domainError(CodeInvalidTransition, "agent session is not current")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pane_bindings SET current = 0, last_observed_at = ?
		WHERE agent_session_id = ? AND current = 1`, e.timestamp(), sessionID); err != nil {
		return "", fmt.Errorf("retire lost pane binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_grants SET revoked_at = ?
		WHERE agent_session_id = ? AND revoked_at IS NULL`, e.timestamp(), sessionID); err != nil {
		return "", fmt.Errorf("revoke lost agent grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_outbox SET status = 'dispatched'
		WHERE id = ? AND status IN ('pending', 'dispatching')`, sessionID); err != nil {
		return "", fmt.Errorf("retire uncertain start effect: %w", err)
	}
	if err := insertEffect(ctx, tx, e.newID(), optimizationID, "session.close_requested", mustJSON(map[string]string{
		"agent_session_id": sessionID, "agent_name": agentName, "pane_id": paneID.String, "terminal_id": terminalID.String,
	}), e.timestamp()); err != nil {
		return "", err
	}
	if role == RoleFollowUp {
		var requestStatus, targetWorkID string
		var targetRole WorkRole
		var generatorAttempts, generatorMaxAttempts int64
		if !followUpRequestID.Valid {
			return "", domainError(CodeStateCorrupt, "Follow-up generator has no request")
		}
		if err := tx.QueryRowContext(ctx, `SELECT status, target_work_id, target_role, generator_attempts, generator_max_attempts
			FROM followup_requests WHERE id = ?`, followUpRequestID.String).Scan(&requestStatus, &targetWorkID, &targetRole, &generatorAttempts, &generatorMaxAttempts); err != nil {
			return "", fmt.Errorf("read lost Follow-up generator request: %w", err)
		}
		if requestStatus != "generating" {
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return "", nil
		}
		if generatorAttempts >= generatorMaxAttempts {
			now := e.timestamp()
			if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ? AND status = ?`, WorkCancelled, now, workID, WorkPending); err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET status = 'generator_exhausted', updated_at = ? WHERE id = ? AND status = 'generating'`, now, followUpRequestID.String); err != nil {
				return "", err
			}
			if err := e.exhaustFollowUpTargetTx(ctx, tx, targetWorkID, targetRole, "generator_attempt_budget_exhausted"); err != nil {
				return "", err
			}
			if err := tx.Commit(); err != nil {
				return "", fmt.Errorf("commit Follow-up generator exhaustion: %w", err)
			}
			return "", nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE followup_requests SET generator_attempts = generator_attempts + 1, updated_at = ? WHERE id = ?`, e.timestamp(), followUpRequestID.String); err != nil {
			return "", fmt.Errorf("advance Follow-up generator attempt: %w", err)
		}
	}
	effectID := e.newID()
	if err := insertEffect(ctx, tx, effectID, optimizationID, "work.start_requested", mustJSON(map[string]string{"work_id": workID, "reason": "lost_session_replacement"}), e.timestamp()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit lost agent replacement: %w", err)
	}
	return effectID, nil
}

// RetireActiveSessionsForRecovery turns every pending Work Session left active
// by a prior daemon into an ordered close-then-fresh-start sequence. A Session
// whose Work is already terminal is deliberately left active: its committed
// work.close_requested effect must still resolve that exact pane before any
// committed successor start is dispatched. It is safe to call this method
// again after a partial recovery because replaced Sessions are no longer active.
func (e *Engine) RetireActiveSessionsForRecovery(ctx context.Context) error {
	records, err := e.ActiveAgentSessions(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		work, err := e.RuntimeWork(ctx, record.Session.WorkID)
		if err != nil {
			return fmt.Errorf("read Work for Agent Session %s recovery: %w", record.Session.ID, err)
		}
		if work.Work.Status != WorkPending {
			continue
		}
		if _, err := e.ReplaceLostAgentSession(ctx, record.Session.ID); err != nil {
			return fmt.Errorf("retire Agent Session %s for daemon recovery: %w", record.Session.ID, err)
		}
	}
	return nil
}

func (e *Engine) Apply(ctx context.Context, command Command) (Receipt, error) {
	if command == nil {
		return Receipt{}, domainError(CodeInvalidCommand, "command is required")
	}
	meta := command.commandMeta()
	if meta.RequestID == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "request_id is required")
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, err.Error())
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, fmt.Errorf("begin command transaction: %w", err)
	}
	defer tx.Rollback()
	if receipt, found, err := loadReceipt(ctx, tx, meta.RequestID, digest); err != nil {
		return Receipt{}, err
	} else if found {
		return receipt, nil
	}

	var receipt Receipt
	switch typed := command.(type) {
	case Init:
		receipt, err = e.applyInit(ctx, tx, typed)
	case SubmitBaselineDefinition:
		receipt, err = e.applySubmitBaselineDefinition(ctx, tx, typed)
	case FinishBaselineVerification:
		receipt, err = e.applyFinishBaselineVerification(ctx, tx, typed)
	case FinishDiagnosis:
		receipt, err = e.applyFinishDiagnosis(ctx, tx, typed)
	case RecordIterationExperiment:
		receipt, err = e.applyRecordIterationExperiment(ctx, tx, typed)
	case BackOff:
		receipt, err = e.applyBackOff(ctx, tx, typed)
	case CancelWork:
		receipt, err = e.applyCancelWork(ctx, tx, typed)
	case RequestShutdown:
		receipt, err = e.applyRequestShutdown(ctx, tx, typed)
	case PauseScheduler:
		receipt, err = e.applyPauseScheduler(ctx, tx, typed)
	case ResumeScheduler:
		receipt, err = e.applyResumeScheduler(ctx, tx, typed)
	case StartBaselineDraft:
		receipt, err = e.applyStartBaselineDraft(ctx, tx, typed)
	case FinishIteration:
		receipt, err = e.applyFinishIteration(ctx, tx, typed)
	case PrepareBestUpdate:
		receipt, err = e.applyPrepareBestUpdate(ctx, tx, typed)
	case FinishIntegration:
		receipt, err = e.applyFinishIntegration(ctx, tx, typed)
	case SubmitFollowUpMessage:
		receipt, err = e.applySubmitFollowUpMessage(ctx, tx, typed)
	default:
		err = domainError(CodeInvalidCommand, fmt.Sprintf("unsupported command %q", command.commandName()))
	}
	if err != nil {
		return Receipt{}, err
	}
	receipt.RequestID = meta.RequestID
	receipt.Command = command.commandName()
	if _, err := tx.ExecContext(ctx, `INSERT INTO operation_receipts
        (request_id, request_digest, receipt_id, command_type, revision, result_json, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, meta.RequestID, digest, receipt.ID, receipt.Command, receipt.Revision, []byte(receipt.Result), e.timestamp()); err != nil {
		return Receipt{}, fmt.Errorf("store operation receipt: %w", err)
	}
	if e.applyCheckpoint != nil {
		if err := e.applyCheckpoint(ApplyBeforeCommit, command.commandName()); err != nil {
			return Receipt{}, fmt.Errorf("command checkpoint %s: %w", ApplyBeforeCommit, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, fmt.Errorf("commit command transaction: %w", err)
	}
	if e.applyCheckpoint != nil {
		if err := e.applyCheckpoint(ApplyAfterCommit, command.commandName()); err != nil {
			return Receipt{}, fmt.Errorf("command checkpoint %s: %w", ApplyAfterCommit, err)
		}
	}
	return receipt, nil
}

func (e *Engine) Replay(ctx context.Context, command Command) (Receipt, bool, error) {
	if command == nil || command.commandMeta().RequestID == "" {
		return Receipt{}, false, domainError(CodeInvalidCommand, "command and request_id are required")
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Receipt{}, false, domainError(CodeInvalidCommand, err.Error())
	}
	var storedDigest string
	var receipt Receipt
	var result []byte
	err = e.db.QueryRowContext(ctx, `SELECT request_digest, receipt_id, command_type, revision, result_json
		FROM operation_receipts WHERE request_id = ?`, command.commandMeta().RequestID).Scan(
		&storedDigest, &receipt.ID, &receipt.Command, &receipt.Revision, &result,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read operation receipt replay: %w", err)
	}
	if storedDigest != digest {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used with different input")
	}
	receipt.RequestID = command.commandMeta().RequestID
	receipt.Result = result
	receipt.Replayed = true
	return receipt, true, nil
}

func (e *Engine) applySubmitBaselineDefinition(ctx context.Context, tx *sql.Tx, command SubmitBaselineDefinition) (Receipt, error) {
	if command.WorkID == "" || len(command.Definition) == 0 || !json.Valid(command.Definition) {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id and a valid JSON definition are required")
	}
	if command.RepositorySHA != "" {
		decoded, err := hex.DecodeString(command.RepositorySHA)
		if err != nil || len(decoded) != 20 {
			return Receipt{}, domainError(CodeInvalidCommand, "repository_sha must be a 40-character Git SHA")
		}
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationDraftingBaseline && optimization.Status != OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline definition can be submitted only while drafting")
	}
	var baselineID string
	var role WorkRole
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT baseline_revision_id, role, status FROM works WHERE id = ? AND optimization_id = ?`, command.WorkID, optimization.ID).Scan(&baselineID, &role, &workStatus); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline draft work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read baseline draft work: %w", err)
	}
	if role != RoleBaselineDraft || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active baseline draft")
	}
	var baselineStatus BaselineStatus
	var measurementContractVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT status, measurement_contract_version FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&baselineStatus, &measurementContractVersion); err != nil {
		return Receipt{}, fmt.Errorf("read baseline revision: %w", err)
	}
	if baselineStatus != BaselineDrafting {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline revision is not drafting")
	}
	if err := candidatepolicy.ValidateNewDefinition(command.Definition); err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, "invalid candidate change policy: "+err.Error())
	}
	if err := benchmarkintegrity.ValidateDefinition(command.Definition); err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, "invalid benchmark integrity contract: "+err.Error())
	}
	var measurementDefinition benchmarkintegrity.MeasurementDefinition
	if measurementContractVersion.Valid {
		if measurementContractVersion.Int64 != benchmarkintegrity.MeasurementSchemaVersion {
			return Receipt{}, domainError(CodeStateCorrupt, "unsupported Baseline measurement contract version")
		}
		measurementDefinition, err = benchmarkintegrity.ParseFrozenMeasurementDefinition(command.Definition)
		if err != nil {
			return Receipt{}, domainError(CodeInvalidCommand, "invalid benchmark measurement contract: "+err.Error())
		}
		if optimization.FlowVersion == FlowVersion2 {
			if err := benchmarkintegrity.RequireIterationGate(measurementDefinition); err != nil {
				return Receipt{}, domainError(CodeInvalidCommand, "invalid benchmark measurement contract: "+err.Error())
			}
		}
	}

	receiptID := e.newID()
	now := e.timestamp()
	nextRevision := optimization.Revision + 1
	digest := sha256.Sum256(command.Definition)
	if optimization.Status == OptimizationDraining {
		eventID := e.newID()
		closeEffectID := e.newID()
		if _, err := tx.ExecContext(ctx, `UPDATE baseline_revisions SET status = ?, definition_json = ?, definition_digest = ?, repository_sha = ?, submitted_at = ? WHERE id = ?`,
			BaselineSubmitted, []byte(command.Definition), hex.EncodeToString(digest[:]), nullable(command.RepositorySHA), now, baselineID); err != nil {
			return Receipt{}, fmt.Errorf("submit draining baseline revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
			return Receipt{}, fmt.Errorf("complete draining baseline draft work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET revision = ?, updated_at = ? WHERE id = ?`, nextRevision, now, optimization.ID); err != nil {
			return Receipt{}, fmt.Errorf("advance draining optimization revision: %w", err)
		}
		payload := mustJSON(map[string]string{"baseline_revision_id": baselineID, "draft_work_id": command.WorkID})
		if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "baseline.submitted_while_draining", payload, now); err != nil {
			return Receipt{}, err
		}
		if err := insertEffect(ctx, tx, closeEffectID, optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
			return Receipt{}, err
		}
		if command.Artifact != nil {
			if err := e.insertArtifact(ctx, tx, command.WorkID, receiptID, *command.Artifact, now); err != nil {
				return Receipt{}, err
			}
		}
		return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
	}
	verificationID := e.newID()
	verificationWorkID := e.newID()
	eventID := e.newID()
	closeEffectID := e.newID()
	startEffectID := e.newID()
	if _, err := tx.ExecContext(ctx, `UPDATE baseline_revisions SET status = ?, definition_json = ?, definition_digest = ?, repository_sha = ?, submitted_at = ? WHERE id = ?`,
		BaselineVerifying, []byte(command.Definition), hex.EncodeToString(digest[:]), nullable(command.RepositorySHA), now, baselineID); err != nil {
		return Receipt{}, fmt.Errorf("submit baseline revision: %w", err)
	}
	if measurementContractVersion.Valid {
		if err := persistMeasurementDefinition(ctx, tx, baselineID, measurementDefinition); err != nil {
			return Receipt{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, fmt.Errorf("complete baseline draft work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO baseline_verifications
        (id, baseline_revision_id, status, created_at) VALUES (?, ?, 'pending', ?)`, verificationID, baselineID, now); err != nil {
		return Receipt{}, fmt.Errorf("create baseline verification: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
        (id, optimization_id, baseline_revision_id, role, status, generation, created_at)
        VALUES (?, ?, ?, ?, ?, 1, ?)`, verificationWorkID, optimization.ID, baselineID, RoleBaselineVerification, WorkPending, now); err != nil {
		return Receipt{}, fmt.Errorf("create baseline verification work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`,
		OptimizationVerifyingBaseline, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("advance optimization to verification: %w", err)
	}
	result, _ := json.Marshal(map[string]string{"baseline_revision_id": baselineID, "verification_id": verificationID, "work_id": verificationWorkID})
	payload, _ := json.Marshal(map[string]string{"baseline_revision_id": baselineID, "draft_work_id": command.WorkID, "verification_work_id": verificationWorkID})
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "baseline.submitted", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, closeEffectID, optimization.ID, "work.close_requested", []byte(`{"work_id":"`+command.WorkID+`"}`), now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, startEffectID, optimization.ID, "work.start_requested", []byte(`{"work_id":"`+verificationWorkID+`"}`), now); err != nil {
		return Receipt{}, err
	}
	if command.Artifact != nil {
		if err := e.insertArtifact(ctx, tx, command.WorkID, receiptID, *command.Artifact, now); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: result}, nil
}

func (e *Engine) applyFinishBaselineVerification(ctx context.Context, tx *sql.Tx, command FinishBaselineVerification) (Receipt, error) {
	if command.WorkID == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id is required")
	}
	if command.Decision != VerificationAccepted && command.Decision != VerificationRejected {
		return Receipt{}, domainError(CodeInvalidCommand, "decision must be accepted or rejected")
	}
	if len(command.Evidence) != 0 && !json.Valid(command.Evidence) {
		return Receipt{}, domainError(CodeInvalidCommand, "evidence must be valid JSON")
	}
	if command.Decision == VerificationRejected && (command.FailureKind == "" || command.Reason == "" || command.RequestedChanges == "") {
		return Receipt{}, domainError(CodeInvalidCommand, "rejected verification requires failure_kind, reason, and requested_changes")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	draining := optimization.Status == OptimizationDraining
	if optimization.Status != OptimizationVerifyingBaseline && !draining {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline verification can finish only while verifying")
	}
	var baselineID string
	var role WorkRole
	var workStatus WorkStatus
	if err := tx.QueryRowContext(ctx, `SELECT baseline_revision_id, role, status FROM works WHERE id = ? AND optimization_id = ?`, command.WorkID, optimization.ID).Scan(&baselineID, &role, &workStatus); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline verification work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read baseline verification work: %w", err)
	}
	if role != RoleBaselineVerification || workStatus != WorkPending {
		return Receipt{}, domainError(CodeInvalidTransition, "work is not an active baseline verification")
	}
	var baselineNumber int64
	var baselineStatus BaselineStatus
	var baselineRepositorySHA sql.NullString
	var measurementContractVersion sql.NullInt64
	var baselineDefinition []byte
	if err := tx.QueryRowContext(ctx, `SELECT number, status, repository_sha, definition_json, measurement_contract_version FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&baselineNumber, &baselineStatus, &baselineRepositorySHA, &baselineDefinition, &measurementContractVersion); err != nil {
		return Receipt{}, fmt.Errorf("read baseline revision: %w", err)
	}
	if baselineStatus != BaselineVerifying {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline revision is not verifying")
	}
	if command.Decision == VerificationAccepted && baselineRepositorySHA.Valid && command.InitialBestSHA != baselineRepositorySHA.String {
		return Receipt{}, domainError(CodeInvalidCommand, "initial Best SHA must equal the frozen Baseline repository snapshot")
	}
	if command.Decision == VerificationAccepted {
		if err := benchmarkintegrity.ValidateEvidence(baselineDefinition, command.Evidence); err != nil {
			return Receipt{}, domainError(CodeInvalidCommand, "invalid benchmark integrity evidence: "+err.Error())
		}
	}
	var baselineMeasurements benchmarkintegrity.MeasurementSet
	if command.Decision == VerificationAccepted && measurementContractVersion.Valid {
		measurementDefinition, parseErr := benchmarkintegrity.ParseFrozenMeasurementDefinition(baselineDefinition)
		if parseErr != nil {
			return Receipt{}, domainError(CodeStateCorrupt, "stored benchmark measurement contract is invalid: "+parseErr.Error())
		}
		baselineMeasurements, parseErr = benchmarkintegrity.ParseBaselineMeasurements(measurementDefinition, command.Evidence)
		if parseErr != nil {
			return Receipt{}, domainError(CodeInvalidCommand, "invalid baseline measurements: "+parseErr.Error())
		}
	}

	receiptID := e.newID()
	eventID := e.newID()
	closeEffectID := e.newID()
	now := e.timestamp()
	nextRevision := optimization.Revision + 1
	accepted := command.Decision == VerificationAccepted
	if _, err := tx.ExecContext(ctx, `UPDATE baseline_verifications SET
        status = ?, accepted = ?, failure_kind = ?, reason = ?, requested_changes = ?, evidence_json = ?, finished_at = ?
        WHERE baseline_revision_id = ?`, command.Decision, accepted, nullable(command.FailureKind), nullable(command.Reason), nullable(command.RequestedChanges), nullableBytes(command.Evidence), now, baselineID); err != nil {
		return Receipt{}, fmt.Errorf("finish baseline verification: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
		return Receipt{}, fmt.Errorf("complete baseline verification work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE baseline_revisions SET status = ?, completed_at = ? WHERE id = ?`, command.Decision, now, baselineID); err != nil {
		return Receipt{}, fmt.Errorf("complete baseline revision: %w", err)
	}
	if accepted && measurementContractVersion.Valid {
		if err := persistBaselineMeasurementSet(ctx, tx, baselineID, command.WorkID, now, baselineMeasurements); err != nil {
			return Receipt{}, err
		}
	}

	resultValues := map[string]string{"baseline_revision_id": baselineID, "decision": string(command.Decision)}
	eventValues := map[string]string{"baseline_revision_id": baselineID, "verification_work_id": command.WorkID, "decision": string(command.Decision)}
	nextStatus := OptimizationOptimizing
	if draining {
		nextStatus = OptimizationDraining
	}
	if !accepted && !draining {
		successorID := e.newID()
		successorWorkID := e.newID()
		startEffectID := e.newID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO baseline_revisions
            (id, optimization_id, number, status, predecessor_id, created_at, measurement_contract_version) VALUES (?, ?, ?, ?, ?, ?, 1)`,
			successorID, optimization.ID, baselineNumber+1, BaselineDrafting, baselineID, now); err != nil {
			return Receipt{}, fmt.Errorf("create successor baseline revision: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO works
            (id, optimization_id, baseline_revision_id, role, status, generation, created_at)
            VALUES (?, ?, ?, ?, ?, 1, ?)`, successorWorkID, optimization.ID, successorID, RoleBaselineDraft, WorkPending, now); err != nil {
			return Receipt{}, fmt.Errorf("create successor baseline draft work: %w", err)
		}
		if err := insertEffect(ctx, tx, startEffectID, optimization.ID, "work.start_requested", mustJSON(map[string]string{"work_id": successorWorkID}), now); err != nil {
			return Receipt{}, err
		}
		resultValues["successor_baseline_revision_id"] = successorID
		resultValues["successor_work_id"] = successorWorkID
		eventValues["successor_baseline_revision_id"] = successorID
		eventValues["successor_work_id"] = successorWorkID
		nextStatus = OptimizationDraftingBaseline
	}
	if accepted && !draining {
		if err := e.seedOptimization(ctx, tx, optimization.ID, baselineID, command.InitialBestSHA, now); err != nil {
			return Receipt{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, nextStatus, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("advance optimization after verification: %w", err)
	}
	result := mustJSON(resultValues)
	payload := mustJSON(eventValues)
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "baseline.verification_finished", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, closeEffectID, optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
		return Receipt{}, err
	}
	for _, artifact := range command.Artifacts {
		if err := e.insertArtifact(ctx, tx, command.WorkID, receiptID, artifact, now); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: result}, nil
}

func (e *Engine) applyBackOff(ctx context.Context, tx *sql.Tx, command BackOff) (Receipt, error) {
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
	var baselineID string
	var role WorkRole
	var workStatus WorkStatus
	var attemptID, integrationID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT baseline_revision_id, role, status, attempt_id, integration_id
		FROM works WHERE id = ? AND optimization_id = ?`, command.WorkID, optimization.ID).
		Scan(&baselineID, &role, &workStatus, &attemptID, &integrationID); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read back-off work: %w", err)
	}
	if role == RoleIntegration {
		return e.applyIntegrationBackOff(ctx, tx, optimization, command, baselineID, attemptID.String, integrationID.String, workStatus)
	}
	if optimization.Status != OptimizationVerifyingBaseline && optimization.Status != OptimizationPaused {
		return Receipt{}, domainError(CodeInvalidTransition, "back-off is allowed only from baseline verification")
	}
	if role != RoleBaselineVerification || (workStatus != WorkPending && workStatus != WorkCancelled) {
		return Receipt{}, domainError(CodeInvalidTransition, "back-off source is not current baseline verification")
	}
	var baselineNumber int64
	var baselineStatus BaselineStatus
	if err := tx.QueryRowContext(ctx, `SELECT number, status FROM baseline_revisions WHERE id = ?`, baselineID).Scan(&baselineNumber, &baselineStatus); err != nil {
		return Receipt{}, fmt.Errorf("read back-off baseline: %w", err)
	}
	if baselineStatus != BaselineVerifying {
		return Receipt{}, domainError(CodeInvalidTransition, "back-off baseline is not verifying")
	}

	receiptID := e.newID()
	backOffID := e.newID()
	successorID := e.newID()
	successorWorkID := e.newID()
	eventID := e.newID()
	startEffectID := e.newID()
	now := e.timestamp()
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE baseline_verifications SET status = 'superseded', failure_kind = 'user_back_off', reason = ?, requested_changes = ?, finished_at = ? WHERE baseline_revision_id = ?`,
		command.Message, command.Message, now, baselineID); err != nil {
		return Receipt{}, fmt.Errorf("supersede baseline verification: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE baseline_revisions SET status = ?, completed_at = ? WHERE id = ?`, BaselineRejected, now, baselineID); err != nil {
		return Receipt{}, fmt.Errorf("reject backed-off baseline revision: %w", err)
	}
	if workStatus == WorkPending {
		if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCompleted, now, command.WorkID); err != nil {
			return Receipt{}, fmt.Errorf("complete backed-off verification work: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO baseline_revisions
        (id, optimization_id, number, status, predecessor_id, created_at, measurement_contract_version) VALUES (?, ?, ?, ?, ?, ?, 1)`,
		successorID, optimization.ID, baselineNumber+1, BaselineDrafting, baselineID, now); err != nil {
		return Receipt{}, fmt.Errorf("create back-off successor baseline: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
        (id, optimization_id, baseline_revision_id, role, status, generation, created_at)
        VALUES (?, ?, ?, ?, ?, 1, ?)`, successorWorkID, optimization.ID, successorID, RoleBaselineDraft, WorkPending, now); err != nil {
		return Receipt{}, fmt.Errorf("create back-off successor work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO back_offs
        (id, optimization_id, source_work_id, successor_baseline_revision_id, message, created_at)
        VALUES (?, ?, ?, ?, ?, ?)`, backOffID, optimization.ID, command.WorkID, successorID, command.Message, now); err != nil {
		return Receipt{}, fmt.Errorf("record back-off: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationDraftingBaseline, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("advance optimization after back-off: %w", err)
	}
	payload := mustJSON(map[string]string{"source_work_id": command.WorkID, "successor_baseline_revision_id": successorID, "successor_work_id": successorWorkID, "message": command.Message})
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "baseline.backed_off", payload, now); err != nil {
		return Receipt{}, err
	}
	if workStatus == WorkPending {
		closeEffectID := e.newID()
		if err := insertEffect(ctx, tx, closeEffectID, optimization.ID, "work.close_requested", mustJSON(map[string]string{"work_id": command.WorkID}), now); err != nil {
			return Receipt{}, err
		}
	}
	if err := insertEffect(ctx, tx, startEffectID, optimization.ID, "work.start_requested", mustJSON(map[string]string{"work_id": successorWorkID}), now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyCancelWork(ctx context.Context, tx *sql.Tx, command CancelWork) (Receipt, error) {
	if command.WorkID == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "work_id is required")
	}
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	var status WorkStatus
	var role WorkRole
	var baselineID string
	var attemptID, integrationID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, role, baseline_revision_id, attempt_id, integration_id
		FROM works WHERE id = ? AND optimization_id = ?`, command.WorkID, optimization.ID).
		Scan(&status, &role, &baselineID, &attemptID, &integrationID); errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, domainError(CodeWorkNotFound, "work was not found")
	} else if err != nil {
		return Receipt{}, fmt.Errorf("read work to cancel: %w", err)
	}
	if status != WorkPending {
		return Receipt{}, domainError(CodeWorkTerminal, "work is already terminal")
	}
	if role == RoleIteration || role == RoleIntegration {
		return e.applyAttemptCancellation(ctx, tx, optimization, command, role, baselineID, attemptID.String, integrationID.String)
	}
	receiptID := e.newID()
	eventID := e.newID()
	effectID := e.newID()
	now := e.timestamp()
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `UPDATE works SET status = ?, finished_at = ? WHERE id = ?`, WorkCancelled, now, command.WorkID); err != nil {
		return Receipt{}, fmt.Errorf("cancel work: %w", err)
	}
	if role == RoleDiagnosis {
		if _, err := tx.ExecContext(ctx, `UPDATE diagnoses SET status = ?, finished_at = ? WHERE work_id = ? AND status = ?`, DiagnosisCancelled, now, command.WorkID, DiagnosisPending); err != nil {
			return Receipt{}, fmt.Errorf("cancel Diagnosis: %w", err)
		}
	}
	nextStatus := OptimizationPaused
	if optimization.Status == OptimizationDraining {
		nextStatus = OptimizationDraining
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, nextStatus, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("pause optimization after cancellation: %w", err)
	}
	payload := mustJSON(map[string]string{"work_id": command.WorkID})
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "work.cancelled", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, effectID, optimization.ID, "work.close_requested", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyRequestShutdown(ctx context.Context, tx *sql.Tx, command RequestShutdown) (Receipt, error) {
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status == OptimizationDraining {
		return Receipt{}, domainError(CodeInvalidTransition, "optimization is already draining")
	}
	var schedulerStatus SchedulerStatus
	var schedulerPausedAt sql.NullString
	var schedulerEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT scheduler_status, scheduler_paused_at, scheduler_epoch
		FROM optimizations WHERE id = ?`, optimization.ID).Scan(&schedulerStatus, &schedulerPausedAt, &schedulerEpoch); err != nil {
		return Receipt{}, fmt.Errorf("read Scheduler state before shutdown: %w", err)
	}
	receiptID := e.newID()
	eventID := e.newID()
	nowTime := e.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	nextRevision := optimization.Revision + 1
	payload := map[string]any{"previous_status": string(optimization.Status), "scheduler_was_paused": schedulerStatus == SchedulerPaused}
	if schedulerStatus == SchedulerPaused {
		if !schedulerPausedAt.Valid {
			return Receipt{}, domainError(CodeStateCorrupt, "paused Scheduler has no pause timestamp")
		}
		if err := shiftWaitingFollowUps(ctx, tx, schedulerPausedAt.String, nowTime); err != nil {
			return Receipt{}, err
		}
		nextEpoch := schedulerEpoch + 1
		if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, scheduler_status = ?, scheduler_paused_at = NULL,
			scheduler_epoch = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationDraining, SchedulerRunning,
			nextEpoch, nextRevision, now, optimization.ID); err != nil {
			return Receipt{}, fmt.Errorf("atomically resume Scheduler for shutdown drain: %w", err)
		}
		cycleID, effectID, err := e.createSchedulerControlCycle(ctx, tx, optimization.ID, nextEpoch, "resume", now)
		if err != nil {
			return Receipt{}, err
		}
		payload["resume_cycle_id"] = cycleID
		payload["resume_effect_id"] = effectID
	} else if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationDraining, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("request optimization shutdown: %w", err)
	}
	payloadJSON := mustJSON(payload)
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "optimization.shutdown_requested", payloadJSON, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payloadJSON}, nil
}

func (e *Engine) applyStartBaselineDraft(ctx context.Context, tx *sql.Tx, command StartBaselineDraft) (Receipt, error) {
	optimization, err := readOptimization(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	if err := checkExpectedRevision(command.Meta.ExpectedRevision, optimization.Revision); err != nil {
		return Receipt{}, err
	}
	if optimization.Status != OptimizationPaused {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline draft recovery is allowed only while paused")
	}
	var baselineID string
	var baselineStatus BaselineStatus
	if err := tx.QueryRowContext(ctx, `SELECT id, status FROM baseline_revisions WHERE optimization_id = ? ORDER BY number DESC LIMIT 1`, optimization.ID).Scan(&baselineID, &baselineStatus); err != nil {
		return Receipt{}, fmt.Errorf("read recovery baseline: %w", err)
	}
	if baselineStatus != BaselineDrafting {
		return Receipt{}, domainError(CodeInvalidTransition, "paused successor is not a baseline draft")
	}
	var activeCount int
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(generation), 0) FROM works
        WHERE baseline_revision_id = ? AND role = ?`, baselineID, RoleBaselineDraft).Scan(&activeCount, &generation); err != nil {
		return Receipt{}, fmt.Errorf("inspect baseline draft recovery works: %w", err)
	}
	if activeCount == 0 {
		return Receipt{}, domainError(CodeStateCorrupt, "paused baseline has no prior draft work")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM works
        WHERE baseline_revision_id = ? AND role = ? AND status = ?`, baselineID, RoleBaselineDraft, WorkPending).Scan(&activeCount); err != nil {
		return Receipt{}, fmt.Errorf("inspect active baseline draft work: %w", err)
	}
	if activeCount != 0 {
		return Receipt{}, domainError(CodeInvalidTransition, "baseline draft already has active work")
	}

	receiptID := e.newID()
	workID := e.newID()
	eventID := e.newID()
	effectID := e.newID()
	now := e.timestamp()
	nextRevision := optimization.Revision + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
        (id, optimization_id, baseline_revision_id, role, status, generation, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, workID, optimization.ID, baselineID, RoleBaselineDraft, WorkPending, generation+1, now); err != nil {
		return Receipt{}, fmt.Errorf("create recovery baseline draft work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE optimizations SET status = ?, revision = ?, updated_at = ? WHERE id = ?`, OptimizationDraftingBaseline, nextRevision, now, optimization.ID); err != nil {
		return Receipt{}, fmt.Errorf("resume baseline drafting: %w", err)
	}
	payload := mustJSON(map[string]any{"baseline_revision_id": baselineID, "work_id": workID, "generation": generation + 1})
	if err := insertEvent(ctx, tx, eventID, optimization.ID, nextRevision, "baseline.draft_restarted", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, effectID, optimization.ID, "work.start_requested", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: nextRevision, Result: payload}, nil
}

func (e *Engine) applyInit(ctx context.Context, tx *sql.Tx, command Init) (Receipt, error) {
	if command.OptimizationID == "" || command.Repository == "" {
		return Receipt{}, domainError(CodeInvalidCommand, "optimization_id and repository are required")
	}
	if command.IterationConcurrency < 0 || command.MaxPendingAttempts < 0 || command.IterationHistoryLimit < 0 {
		return Receipt{}, domainError(CodeInvalidCommand, "scheduler limits must be positive")
	}
	if command.IterationConcurrency == 0 {
		command.IterationConcurrency = 1
	}
	if command.MaxPendingAttempts == 0 {
		command.MaxPendingAttempts = 8
	}
	// Zero explicitly disables history. Direct Engine callers that predate this
	// field receive the default unless they mark the value as configured.
	if !command.IterationHistoryLimitSet {
		command.IterationHistoryLimit = 20
	}
	flowVersion, err := normalizedFlowVersion(command.FlowVersion)
	if err != nil {
		return Receipt{}, domainError(CodeInvalidCommand, err.Error())
	}
	if flowVersion == FlowVersion1 && command.SkillSnapshot != nil {
		return Receipt{}, domainError(CodeInvalidCommand, "flow v1 cannot receive a skill snapshot")
	}
	if flowVersion == FlowVersion2 {
		if err := validateSkillSnapshotInput(command.SkillSnapshot); err != nil {
			return Receipt{}, domainError(CodeInvalidCommand, err.Error())
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM optimizations").Scan(&count); err != nil {
		return Receipt{}, fmt.Errorf("inspect optimization singleton: %w", err)
	}
	if count != 0 {
		return Receipt{}, domainError(CodeInvalidTransition, "optimization is already initialized")
	}

	receiptID := e.newID()
	baselineID := e.newID()
	workID := e.newID()
	eventID := e.newID()
	effectID := e.newID()
	now := e.timestamp()
	if _, err := tx.ExecContext(ctx, `INSERT INTO optimizations
		(id, status, revision, repository, iteration_concurrency, max_pending_attempts, iteration_history_limit, flow_version, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`, command.OptimizationID, OptimizationDraftingBaseline, command.Repository,
		command.IterationConcurrency, command.MaxPendingAttempts, command.IterationHistoryLimit, flowVersion, now, now); err != nil {
		return Receipt{}, fmt.Errorf("create optimization: %w", err)
	}
	if flowVersion == FlowVersion2 {
		snapshot := command.SkillSnapshot
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_snapshots
			(optimization_id, schema_version, snapshot_id, root_path, manifest_json, manifest_sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, command.OptimizationID, snapshot.SchemaVersion, snapshot.SnapshotID,
			snapshot.RootPath, []byte(snapshot.Manifest), snapshot.ManifestSHA256, now); err != nil {
			return Receipt{}, fmt.Errorf("persist frozen skill snapshot: %w", err)
		}
		for ordinal, entry := range snapshot.Entries {
			if _, err := tx.ExecContext(ctx, `INSERT INTO skill_snapshot_entries
				(optimization_id, ordinal, name, repository, branch, commit_sha, relative_path, content_sha256)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, command.OptimizationID, ordinal, entry.Name, entry.Repository,
				entry.Branch, entry.CommitSHA, entry.RelativePath, entry.ContentSHA256); err != nil {
				return Receipt{}, fmt.Errorf("persist frozen skill snapshot entry: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO baseline_revisions
		(id, optimization_id, number, status, created_at, measurement_contract_version) VALUES (?, ?, 1, ?, ?, 1)`, baselineID, command.OptimizationID, BaselineDrafting, now); err != nil {
		return Receipt{}, fmt.Errorf("create baseline revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO works
        (id, optimization_id, baseline_revision_id, role, status, generation, created_at)
        VALUES (?, ?, ?, ?, ?, 1, ?)`, workID, command.OptimizationID, baselineID, RoleBaselineDraft, WorkPending, now); err != nil {
		return Receipt{}, fmt.Errorf("create baseline draft work: %w", err)
	}
	result, _ := json.Marshal(map[string]any{"optimization_id": command.OptimizationID, "baseline_revision_id": baselineID, "work_id": workID, "flow_version": flowVersion})
	payloadValues := map[string]string{"baseline_revision_id": baselineID, "work_id": workID}
	if command.CallerPaneID != "" {
		payloadValues["preferred_pane_id"] = command.CallerPaneID
	}
	payload, _ := json.Marshal(payloadValues)
	if err := insertEvent(ctx, tx, eventID, command.OptimizationID, 1, "optimization.initialized", payload, now); err != nil {
		return Receipt{}, err
	}
	if err := insertEffect(ctx, tx, effectID, command.OptimizationID, "work.start_requested", payload, now); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: receiptID, Revision: 1, Result: result}, nil
}

func (e *Engine) Inspect(ctx context.Context, query Query) (View, error) {
	if query == nil || query.queryName() != "status" {
		return View{}, domainError(CodeInvalidCommand, "unsupported query")
	}
	var view View
	var schedulerPausedAt sql.NullString
	var iterationCaseSetVersion sql.NullInt64
	if err := e.db.QueryRowContext(ctx, `SELECT id, status, revision, repository, iteration_concurrency, max_pending_attempts,
		iteration_history_limit, flow_version, scheduler_status, scheduler_paused_at, scheduler_epoch, iteration_case_set_version FROM optimizations LIMIT 1`).Scan(
		&view.Optimization.ID, &view.Optimization.Status, &view.Optimization.Revision, &view.Optimization.Repository,
		&view.Optimization.IterationConcurrency, &view.Optimization.MaxPendingAttempts, &view.Optimization.IterationHistoryLimit, &view.Optimization.FlowVersion,
		&view.Scheduler.Status, &schedulerPausedAt, &view.Scheduler.Epoch, &iterationCaseSetVersion,
	); errors.Is(err, sql.ErrNoRows) {
		return View{}, domainError(CodeNotInitialized, "optimization is not initialized")
	} else if err != nil {
		return View{}, fmt.Errorf("read optimization: %w", err)
	}
	view.Scheduler.PausedAt = schedulerPausedAt.String
	if view.Optimization.FlowVersion == FlowVersion2 {
		snapshot, err := e.readSkillSnapshot(ctx, view.Optimization.ID)
		if err != nil {
			return View{}, err
		}
		view.SkillSnapshot = &snapshot
	}
	if iterationCaseSetVersion.Valid {
		caseSet, err := currentIterationCaseSet(ctx, e.db, view.Optimization.ID)
		if err != nil {
			return View{}, err
		}
		view.IterationCaseSet = &caseSet
	}
	var latestCycleID string
	if err := e.db.QueryRowContext(ctx, `SELECT id FROM scheduler_control_cycles
		WHERE optimization_id = ? ORDER BY epoch DESC LIMIT 1`, view.Optimization.ID).Scan(&latestCycleID); err == nil {
		latest, cycleErr := e.SchedulerControlCycle(ctx, latestCycleID)
		if cycleErr != nil {
			return View{}, cycleErr
		}
		view.Scheduler.Latest = &latest
	} else if !errors.Is(err, sql.ErrNoRows) {
		return View{}, fmt.Errorf("read latest Scheduler control cycle: %w", err)
	}
	baselineRows, err := e.db.QueryContext(ctx, `SELECT b.id, b.number, b.status, b.definition_json, b.repository_sha, b.predecessor_id, b.measurement_contract_version,
		v.failure_kind, v.reason, v.requested_changes, v.evidence_json
		FROM baseline_revisions b LEFT JOIN baseline_verifications v ON v.baseline_revision_id = b.id
		WHERE b.optimization_id = ? ORDER BY b.number`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read baselines: %w", err)
	}
	for baselineRows.Next() {
		var baseline BaselineView
		var definition []byte
		var repositorySHA, predecessor, failureKind, failureReason, requestedChanges sql.NullString
		var measurementContractVersion sql.NullInt64
		var evidence []byte
		if err := baselineRows.Scan(&baseline.ID, &baseline.Number, &baseline.Status, &definition, &repositorySHA, &predecessor, &measurementContractVersion,
			&failureKind, &failureReason, &requestedChanges, &evidence); err != nil {
			_ = baselineRows.Close()
			return View{}, fmt.Errorf("scan baseline: %w", err)
		}
		baseline.Definition = definition
		baseline.RepositorySHA = repositorySHA.String
		baseline.PredecessorID = predecessor.String
		baseline.FailureKind = failureKind.String
		baseline.FailureReason = failureReason.String
		baseline.RequestedChanges = requestedChanges.String
		baseline.VerificationEvidence = evidence
		baseline.MeasurementContractVersion = measurementContractVersion.Int64
		view.Baselines = append(view.Baselines, baseline)
	}
	if err := baselineRows.Close(); err != nil {
		return View{}, fmt.Errorf("close baseline rows: %w", err)
	}
	if len(view.Baselines) == 0 {
		return View{}, domainError(CodeStateCorrupt, "optimization has no baseline revision")
	}
	currentBaseline := view.Baselines[len(view.Baselines)-1]
	view.Baseline = &currentBaseline
	rows, err := e.db.QueryContext(ctx, `SELECT id, baseline_revision_id, role, status, generation, attempt_id, iteration_round, integration_id, parent_work_id, followup_request_id
        FROM works WHERE optimization_id = ? ORDER BY rowid`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read works: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var work WorkView
		var attemptID, integrationID, parentWorkID, followUpRequestID sql.NullString
		var iterationRound sql.NullInt64
		if err := rows.Scan(&work.ID, &work.BaselineRevisionID, &work.Role, &work.Status, &work.Generation, &attemptID, &iterationRound, &integrationID, &parentWorkID, &followUpRequestID); err != nil {
			return View{}, fmt.Errorf("scan work: %w", err)
		}
		work.AttemptID, work.IterationRound, work.IntegrationID = attemptID.String, iterationRound.Int64, integrationID.String
		work.ParentWorkID, work.FollowUpRequestID = parentWorkID.String, followUpRequestID.String
		view.Works = append(view.Works, work)
	}
	if err := rows.Err(); err != nil {
		return View{}, fmt.Errorf("iterate works: %w", err)
	}
	sessionRows, err := e.db.QueryContext(ctx, `SELECT s.id, s.work_id, s.generation, s.role, s.agent_kind, s.agent_name,
		COALESCE(s.provider_version, ''), COALESCE(s.provider_capabilities_json, X''), s.status
		FROM agent_sessions s JOIN works w ON w.id = s.work_id
		WHERE w.optimization_id = ? ORDER BY s.created_at, s.id`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read Agent Sessions: %w", err)
	}
	for sessionRows.Next() {
		var session AgentSession
		if err := sessionRows.Scan(&session.ID, &session.WorkID, &session.Generation, &session.Role, &session.AgentKind,
			&session.AgentName, &session.ProviderVersion, &session.ProviderCapabilities, &session.Status); err != nil {
			_ = sessionRows.Close()
			return View{}, fmt.Errorf("scan Agent Session: %w", err)
		}
		view.AgentSessions = append(view.AgentSessions, session)
	}
	if err := sessionRows.Close(); err != nil {
		return View{}, fmt.Errorf("close Agent Session rows: %w", err)
	}
	bestRows, err := e.db.QueryContext(ctx, `SELECT id, sequence, commit_sha, source_attempt_id, evidence_json FROM best_revisions
		WHERE optimization_id = ? ORDER BY sequence`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read Best spine: %w", err)
	}
	for bestRows.Next() {
		var best BestView
		var sourceAttempt sql.NullString
		var evidence []byte
		if err := bestRows.Scan(&best.ID, &best.Sequence, &best.CommitSHA, &sourceAttempt, &evidence); err != nil {
			_ = bestRows.Close()
			return View{}, fmt.Errorf("scan Best spine: %w", err)
		}
		best.SourceAttemptID, best.Evidence = sourceAttempt.String, evidence
		view.Bests = append(view.Bests, best)
	}
	if err := bestRows.Close(); err != nil {
		return View{}, fmt.Errorf("close Best spine: %w", err)
	}
	if len(view.Bests) != 0 {
		best := view.Bests[len(view.Bests)-1]
		view.Best = &best
	}
	attemptRows, err := e.db.QueryContext(ctx, `SELECT id, slot_index, status, base_best_sequence, base_sha,
		current_iteration_round, candidate_sha, summary, failure_reason, history_limit FROM attempts WHERE optimization_id = ? ORDER BY created_at, id`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read attempts: %w", err)
	}
	for attemptRows.Next() {
		var attempt AttemptView
		var candidateSHA, summary, failureReason sql.NullString
		if err := attemptRows.Scan(&attempt.ID, &attempt.SlotIndex, &attempt.Status, &attempt.BaseBestSequence, &attempt.BaseSHA,
			&attempt.CurrentIterationRound, &candidateSHA, &summary, &failureReason, &attempt.HistoryLimit); err != nil {
			_ = attemptRows.Close()
			return View{}, fmt.Errorf("scan attempt: %w", err)
		}
		attempt.CandidateSHA, attempt.Summary, attempt.FailureReason = candidateSHA.String, summary.String, failureReason.String
		view.Attempts = append(view.Attempts, attempt)
	}
	if err := attemptRows.Close(); err != nil {
		return View{}, fmt.Errorf("close attempt rows: %w", err)
	}
	integrationRows, err := e.db.QueryContext(ctx, `SELECT sequence, fifo_position, id, attempt_id, iteration_round, status,
		candidate_sha, expected_best_sha, candidate_experiment_id, intent_id, regression_cases_json, validation_json, result_json FROM integrations ORDER BY sequence`)
	if err != nil {
		return View{}, fmt.Errorf("read integrations: %w", err)
	}
	for integrationRows.Next() {
		var integration IntegrationView
		var intentID sql.NullString
		var candidateExperimentID sql.NullString
		var regressionCases, validation, result []byte
		if err := integrationRows.Scan(&integration.Sequence, &integration.FIFOPosition, &integration.ID, &integration.AttemptID,
			&integration.IterationRound, &integration.Status, &integration.CandidateSHA,
			&integration.ExpectedBestSHA, &candidateExperimentID, &intentID, &regressionCases, &validation, &result); err != nil {
			_ = integrationRows.Close()
			return View{}, fmt.Errorf("scan integration: %w", err)
		}
		integration.CandidateExperimentID = candidateExperimentID.String
		integration.IntentID = intentID.String
		if len(regressionCases) != 0 {
			if err := json.Unmarshal(regressionCases, &integration.RegressionCases); err != nil {
				_ = integrationRows.Close()
				return View{}, domainError(CodeStateCorrupt, "Integration has invalid Regression Case history")
			}
		}
		integration.Validation, integration.Result = validation, result
		view.Integrations = append(view.Integrations, integration)
	}
	if err := integrationRows.Close(); err != nil {
		return View{}, fmt.Errorf("close integration rows: %w", err)
	}
	roundRows, err := e.db.QueryContext(ctx, `SELECT r.attempt_id, r.round, r.kind, r.base_sha, r.current_checkpoint_sha, r.status, r.back_off_message,
		r.iteration_case_set_version, r.evidence_json
		FROM iteration_rounds r JOIN attempts a ON a.id = r.attempt_id
		WHERE a.optimization_id = ? ORDER BY r.created_at, r.id`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read iteration rounds: %w", err)
	}
	for roundRows.Next() {
		var round IterationRoundView
		var message, currentCheckpoint sql.NullString
		var version sql.NullInt64
		var evidence []byte
		if err := roundRows.Scan(&round.AttemptID, &round.Round, &round.Kind, &round.BaseSHA, &currentCheckpoint, &round.Status, &message, &version, &evidence); err != nil {
			_ = roundRows.Close()
			return View{}, fmt.Errorf("scan iteration round: %w", err)
		}
		round.BackOffMessage = message.String
		round.CurrentCheckpointSHA = currentCheckpoint.String
		round.IterationCaseSetVersion = version.Int64
		round.Evidence = evidence
		view.IterationRounds = append(view.IterationRounds, round)
	}
	if err := roundRows.Close(); err != nil {
		return View{}, fmt.Errorf("close iteration round rows: %w", err)
	}
	diagnosisRows, err := e.db.QueryContext(ctx, `SELECT id, baseline_revision_id, work_id, status, report_json
		FROM diagnoses WHERE optimization_id = ? ORDER BY created_at, id`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read Diagnoses: %w", err)
	}
	for diagnosisRows.Next() {
		var diagnosis DiagnosisView
		var report []byte
		if err := diagnosisRows.Scan(&diagnosis.ID, &diagnosis.BaselineRevisionID, &diagnosis.WorkID, &diagnosis.Status, &report); err != nil {
			_ = diagnosisRows.Close()
			return View{}, fmt.Errorf("scan Diagnosis: %w", err)
		}
		diagnosis.Report = report
		view.Diagnoses = append(view.Diagnoses, diagnosis)
	}
	if err := diagnosisRows.Close(); err != nil {
		return View{}, fmt.Errorf("close Diagnosis rows: %w", err)
	}
	experimentRows, err := e.db.QueryContext(ctx, `SELECT id, attempt_id, iteration_round, sequence, outcome, parent_checkpoint_sha, checkpoint_sha, scope_best_sha, receipt_id, experiment_json
		FROM iteration_experiments WHERE optimization_id = ? ORDER BY attempt_id, iteration_round, sequence`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read Iteration Experiments: %w", err)
	}
	for experimentRows.Next() {
		var experiment IterationExperimentView
		var checkpoint sql.NullString
		if err := experimentRows.Scan(&experiment.ID, &experiment.AttemptID, &experiment.IterationRound, &experiment.Sequence, &experiment.Outcome, &experiment.ParentCheckpointSHA, &checkpoint, &experiment.ScopeBestSHA, &experiment.ReceiptID, &experiment.Experiment); err != nil {
			_ = experimentRows.Close()
			return View{}, fmt.Errorf("scan Iteration Experiment: %w", err)
		}
		experiment.CheckpointSHA = checkpoint.String
		view.IterationExperiments = append(view.IterationExperiments, experiment)
	}
	if err := experimentRows.Close(); err != nil {
		return View{}, fmt.Errorf("close Iteration Experiment rows: %w", err)
	}
	if err := summarizeKnowledge(&view); err != nil {
		return View{}, err
	}
	backOffRows, err := e.db.QueryContext(ctx, `SELECT id, source_work_id, successor_baseline_revision_id, message
        FROM back_offs WHERE optimization_id = ? ORDER BY rowid`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read back-offs: %w", err)
	}
	for backOffRows.Next() {
		var backOff BackOffView
		if err := backOffRows.Scan(&backOff.ID, &backOff.SourceWorkID, &backOff.SuccessorBaselineRevisionID, &backOff.Message); err != nil {
			_ = backOffRows.Close()
			return View{}, fmt.Errorf("scan back-off: %w", err)
		}
		view.BackOffs = append(view.BackOffs, backOff)
	}
	if err := backOffRows.Close(); err != nil {
		return View{}, fmt.Errorf("close back-off rows: %w", err)
	}
	followUpRows, err := e.db.QueryContext(ctx, `SELECT f.id, f.target_work_id, f.target_agent_session_id,
		f.target_provider_turn_id, f.target_role, f.request_sequence, f.status, f.inactivity_timeout_ms,
		f.due_at, f.last_observed_pane_activity_at, f.activity_source, f.generator_work_id, f.message, f.delivery_id,
		f.target_max_messages, f.generator_max_attempts, f.generator_attempts
		FROM followup_requests f JOIN works w ON w.id = f.target_work_id WHERE w.optimization_id = ? ORDER BY f.sequence`, view.Optimization.ID)
	if err != nil {
		return View{}, fmt.Errorf("read Follow-up Requests: %w", err)
	}
	for followUpRows.Next() {
		var followUp FollowUpView
		var activityAt, source, generatorWorkID, message, deliveryID sql.NullString
		if err := followUpRows.Scan(&followUp.ID, &followUp.TargetWorkID, &followUp.TargetAgentSessionID,
			&followUp.TargetProviderTurnID, &followUp.TargetRole, &followUp.RequestSequence, &followUp.Status,
			&followUp.InactivityTimeoutMS, &followUp.DueAt, &activityAt, &source, &generatorWorkID, &message, &deliveryID,
			&followUp.TargetMaxMessages, &followUp.GeneratorMaxAttempts, &followUp.GeneratorAttempts); err != nil {
			_ = followUpRows.Close()
			return View{}, fmt.Errorf("scan Follow-up Request: %w", err)
		}
		followUp.LastObservedPaneActivityAt, followUp.ActivitySource = activityAt.String, source.String
		followUp.GeneratorWorkID, followUp.Message, followUp.DeliveryID = generatorWorkID.String, message.String, deliveryID.String
		view.FollowUps = append(view.FollowUps, followUp)
	}
	if err := followUpRows.Close(); err != nil {
		return View{}, fmt.Errorf("close Follow-up rows: %w", err)
	}
	view.PaneActivityNotice = paneActivityNotice
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM domain_events WHERE optimization_id = ?`, view.Optimization.ID).Scan(&view.DomainEventCount); err != nil {
		return View{}, fmt.Errorf("count domain events: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_outbox WHERE optimization_id = ? AND status = 'pending'`, view.Optimization.ID).Scan(&view.PendingEffectCount); err != nil {
		return View{}, fmt.Errorf("count pending effects: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&view.Storage.PageCount); err != nil {
		return View{}, fmt.Errorf("read SQLite page count: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&view.Storage.PageSize); err != nil {
		return View{}, fmt.Errorf("read SQLite page size: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(raw_json)), 0) FROM provider_events`).Scan(&view.Storage.ProviderEventBytes); err != nil {
		return View{}, fmt.Errorf("measure provider event storage: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(input_json) + COALESCE(length(output_json), 0)), 0) FROM tool_events`).Scan(&view.Storage.ToolPayloadBytes); err != nil {
		return View{}, fmt.Errorf("measure tool payload storage: %w", err)
	}
	if info, err := os.Stat(e.databasePath); err == nil {
		view.Storage.DatabaseBytes = info.Size()
	} else {
		return View{}, fmt.Errorf("measure SQLite database: %w", err)
	}
	if info, err := os.Stat(e.databasePath + "-wal"); err == nil {
		view.Storage.WALBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return View{}, fmt.Errorf("measure SQLite WAL: %w", err)
	}
	return view, nil
}

func prepareDatabasePath(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect database directory: %w", err)
	}
	return nil
}

func validateOnlineState(ctx context.Context, db *sql.DB, allowIterationCaseMigration bool) error {
	var optimizationCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations`).Scan(&optimizationCount); err != nil {
		return fmt.Errorf("validate optimization singleton: %w", err)
	}
	if optimizationCount > 1 {
		return domainError(CodeStateCorrupt, "database contains more than one optimization")
	}
	var invalidCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations
        WHERE status NOT IN ('drafting_baseline', 'verifying_baseline', 'optimizing', 'paused', 'draining') OR revision < 1
        OR flow_version NOT IN (1, 2)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate optimization state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "optimization has an invalid status or revision")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations o
		WHERE o.iteration_case_set_version IS NULL AND EXISTS (SELECT 1 FROM attempts a WHERE a.optimization_id = o.id)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Iteration Case Set migration state: %w", err)
	}
	if invalidCount != 0 && !allowIterationCaseMigration {
		return fmt.Errorf("workspace requires explicit offline migration: stop the daemon and run pika-go workspace migrate-iteration-cases --workspace PATH --case-id ID ...")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations o WHERE
		(o.iteration_case_set_version IS NOT NULL AND o.iteration_case_set_version < 1)
		OR (o.iteration_case_set_version IS NOT NULL AND NOT EXISTS (SELECT 1 FROM iteration_cases c WHERE c.optimization_id = o.id))
		OR EXISTS (SELECT 1 FROM iteration_cases c WHERE c.optimization_id = o.id AND c.added_in_version > o.iteration_case_set_version)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Iteration Case Set: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Optimization has an invalid Iteration Case Set")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations o WHERE EXISTS (
		SELECT 1 FROM iteration_cases c WHERE c.optimization_id = o.id
		GROUP BY c.optimization_id HAVING MIN(c.ordinal) != 0 OR MAX(c.ordinal) + 1 != COUNT(*))`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Iteration Case ordering: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Iteration Case Set ordering is not contiguous")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations
		WHERE scheduler_status NOT IN ('running', 'paused') OR scheduler_epoch < 0
		OR (scheduler_status = 'paused' AND scheduler_paused_at IS NULL)
		OR (scheduler_status = 'running' AND scheduler_paused_at IS NOT NULL)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Scheduler state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Scheduler has an invalid status, epoch, or pause timestamp")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_control_cycles
		WHERE epoch < 1 OR action NOT IN ('pause', 'resume') OR status NOT IN ('pending', 'complete', 'partial')`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Scheduler control cycles: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Scheduler control cycle has an invalid action, status, or epoch")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_control_actions
		WHERE action NOT IN ('pause', 'resume')
		OR status NOT IN ('pending', 'dispatching', 'sent', 'skipped', 'failed', 'delivery_unknown', 'observed')
		OR (action = 'resume' AND COALESCE(message, '') != '继续') OR (action = 'pause' AND message IS NOT NULL)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Session control actions: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Session control action has an invalid action, status, or message")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline_revisions
        WHERE status NOT IN ('drafting', 'submitted', 'verifying', 'accepted', 'rejected') OR number < 1`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate baseline state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "baseline revision has an invalid status or number")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works
		WHERE role NOT IN ('baseline_draft', 'baseline_verification', 'diagnosis', 'iteration', 'integration', 'follow_up') OR status NOT IN ('pending', 'completed', 'cancelled') OR generation < 1`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate work state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "work has an invalid role, status, or generation")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts
		WHERE status NOT IN ('iterating', 'awaiting_integration', 'integrating', 'refresh_pending', 'accepted', 'rejected', 'cancelled')
		OR current_iteration_round < 1 OR base_best_sequence < 0`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Attempt state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Attempt has an invalid status or revision")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM iteration_rounds
		WHERE status NOT IN ('queued', 'running', 'candidate', 'rejected', 'cancelled') OR round < 1`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Iteration Round state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Iteration Round has an invalid status or round")
	}
	if !allowIterationCaseMigration {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM iteration_rounds r
			WHERE r.iteration_case_set_version IS NULL
			OR NOT EXISTS (SELECT 1 FROM iteration_round_cases c WHERE c.attempt_id = r.attempt_id AND c.round = r.round)`).Scan(&invalidCount); err != nil {
			return fmt.Errorf("validate Iteration Case Snapshots: %w", err)
		}
		if invalidCount != 0 {
			return domainError(CodeStateCorrupt, "Iteration Round has no frozen Iteration Case Snapshot")
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM iteration_rounds r
			JOIN attempts a ON a.id = r.attempt_id JOIN optimizations o ON o.id = a.optimization_id
			WHERE r.iteration_case_set_version < 0 OR r.iteration_case_set_version > o.iteration_case_set_version
			OR (r.iteration_case_set_version > 0 AND (
				(SELECT COUNT(*) FROM iteration_round_cases rc WHERE rc.attempt_id = r.attempt_id AND rc.round = r.round)
				!= (SELECT COUNT(*) FROM iteration_cases c WHERE c.optimization_id = o.id AND c.added_in_version <= r.iteration_case_set_version)
				OR EXISTS (SELECT 1 FROM iteration_round_cases rc LEFT JOIN iteration_cases c
					ON c.optimization_id = o.id AND c.case_id = rc.case_id
					WHERE rc.attempt_id = r.attempt_id AND rc.round = r.round
					AND (c.case_id IS NULL OR c.added_in_version > r.iteration_case_set_version))))`).Scan(&invalidCount); err != nil {
			return fmt.Errorf("validate frozen Iteration Case Snapshot membership: %w", err)
		}
		if invalidCount != 0 {
			return domainError(CodeStateCorrupt, "Iteration Round has an inconsistent Iteration Case Snapshot")
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integrations
		WHERE status NOT IN ('queued', 'running', 'best_update_prepared', 'accepted', 'rejected', 'stale', 'backed_off', 'refreshed', 'cancelled')
		OR iteration_round < 1`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Integration state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Integration has an invalid status or round")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM followup_requests
		WHERE status NOT IN ('waiting', 'generating', 'ready', 'dispatching', 'delivery_unknown', 'delivered', 'superseded', 'cancelled', 'target_exhausted', 'generator_exhausted')
		OR request_sequence < 1 OR inactivity_timeout_ms < 1 OR target_max_messages < 1
		OR generator_max_attempts < 1 OR generator_attempts < 0 OR generator_attempts > generator_max_attempts
		OR (status IN ('generating', 'ready', 'dispatching', 'delivery_unknown', 'delivered', 'generator_exhausted') AND generator_work_id IS NULL)`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Follow-up state: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Follow-up Request has an invalid status, identity, or budget")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works w
		LEFT JOIN attempts a ON a.id = w.attempt_id
		LEFT JOIN integrations i ON i.id = w.integration_id
		WHERE (w.role IN ('baseline_draft', 'baseline_verification') AND (w.attempt_id IS NOT NULL OR w.iteration_round IS NOT NULL OR w.integration_id IS NOT NULL))
		   OR (w.role = 'iteration' AND (a.id IS NULL OR w.iteration_round IS NULL OR w.integration_id IS NOT NULL))
		   OR (w.role = 'integration' AND (a.id IS NULL OR i.id IS NULL OR w.iteration_round IS NULL OR i.attempt_id != w.attempt_id))
		   OR (w.role = 'follow_up' AND (w.parent_work_id IS NULL OR w.followup_request_id IS NULL OR w.attempt_id IS NOT NULL OR w.iteration_round IS NOT NULL OR w.integration_id IS NOT NULL))
		   OR (w.role != 'follow_up' AND (w.parent_work_id IS NOT NULL OR w.followup_request_id IS NOT NULL))`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Work aggregate identity: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Work has inconsistent Attempt or Integration identity")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integrations WHERE status IN ('running', 'best_update_prepared')`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Integration concurrency: %w", err)
	}
	if invalidCount > 1 {
		return domainError(CodeStateCorrupt, "more than one Integration is active")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM optimizations o
		WHERE (SELECT COUNT(*) FROM attempts a WHERE a.optimization_id = o.id AND a.status = 'iterating') > o.iteration_concurrency`).Scan(&invalidCount); err != nil {
		return fmt.Errorf("validate Iteration concurrency: %w", err)
	}
	if invalidCount != 0 {
		return domainError(CodeStateCorrupt, "Iteration concurrency is exceeded")
	}
	if optimizationCount == 1 {
		var baselineCount int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline_revisions`).Scan(&baselineCount); err != nil {
			return fmt.Errorf("validate baseline presence: %w", err)
		}
		if baselineCount == 0 {
			return domainError(CodeStateCorrupt, "initialized optimization has no baseline revision")
		}
	}
	foreignKeyRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("validate foreign keys: %w", err)
	}
	defer foreignKeyRows.Close()
	if foreignKeyRows.Next() {
		return domainError(CodeStateCorrupt, "database contains a foreign-key violation")
	}
	if err := foreignKeyRows.Err(); err != nil {
		return fmt.Errorf("validate foreign keys: %w", err)
	}
	return nil
}

func commandDigest(command Command) (string, error) {
	body, err := json.Marshal(command)
	if err != nil {
		return "", fmt.Errorf("encode command: %w", err)
	}
	var canonical any
	if err := json.Unmarshal(body, &canonical); err != nil {
		return "", fmt.Errorf("canonicalize command: %w", err)
	}
	if object, ok := canonical.(map[string]any); ok {
		if meta, ok := object["meta"].(map[string]any); ok {
			delete(meta, "request_id")
		}
	}
	body, err = json.Marshal(struct {
		Type string `json:"type"`
		Body any    `json:"body"`
	}{command.commandName(), canonical})
	if err != nil {
		return "", fmt.Errorf("encode canonical command: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func loadReceipt(ctx context.Context, tx *sql.Tx, requestID, digest string) (Receipt, bool, error) {
	var receipt Receipt
	var storedDigest string
	var result []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest, receipt_id, command_type, revision, result_json
        FROM operation_receipts WHERE request_id = ?`, requestID).Scan(&storedDigest, &receipt.ID, &receipt.Command, &receipt.Revision, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read operation receipt: %w", err)
	}
	if storedDigest != digest {
		return Receipt{}, false, domainError(CodeIdempotencyConflict, "request_id was already used for a different command body")
	}
	receipt.RequestID = requestID
	receipt.Result = result
	receipt.Replayed = true
	return receipt, true, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, id, optimizationID string, revision int64, eventType string, payload []byte, now string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO domain_events
        (id, optimization_id, revision, event_type, payload_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, optimizationID, revision, eventType, payload, now); err != nil {
		return fmt.Errorf("append domain event: %w", err)
	}
	return nil
}

func insertEffect(ctx context.Context, tx *sql.Tx, id, optimizationID, effectType string, payload []byte, now string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_outbox
        (id, optimization_id, effect_type, payload_json, status, created_at) VALUES (?, ?, ?, ?, 'pending', ?)`,
		id, optimizationID, effectType, payload, now); err != nil {
		return fmt.Errorf("append runtime effect: %w", err)
	}
	return nil
}

func (e *Engine) insertArtifact(ctx context.Context, tx *sql.Tx, workID, receiptID string, artifact ArtifactInput, now string) error {
	if artifact.RelativePath == "" || artifact.ByteSize < 0 || len(artifact.ContentSHA256) != 64 || artifact.ContractVersion < 1 {
		return domainError(CodeInvalidCommand, "artifact metadata is incomplete")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evidence_artifacts
		(id, work_id, receipt_id, relative_path, byte_size, content_sha256, contract_version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.newID(), workID, receiptID, artifact.RelativePath,
		artifact.ByteSize, artifact.ContentSHA256, artifact.ContractVersion, now); err != nil {
		return fmt.Errorf("record evidence artifact: %w", err)
	}
	return nil
}

func (e *Engine) timestamp() string { return e.now().UTC().Format(time.RFC3339Nano) }

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate random ID: %v", err))
	}
	return hex.EncodeToString(value[:])
}

func domainError(code ErrorCode, message string) error {
	return &DomainError{Code: code, Message: message}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode internal JSON: %v", err))
	}
	return encoded
}

type optimizationRecord struct {
	ID          string
	Status      OptimizationStatus
	Revision    int64
	FlowVersion FlowVersion
}

func (e *Engine) readSkillSnapshot(ctx context.Context, optimizationID string) (SkillSnapshotView, error) {
	expectedSources := expectedSkillSourcesForCurrentProcess()
	var snapshot SkillSnapshotView
	err := e.db.QueryRowContext(ctx, `SELECT schema_version, snapshot_id, root_path, manifest_sha256
		FROM skill_snapshots WHERE optimization_id = ?`, optimizationID).Scan(
		&snapshot.SchemaVersion, &snapshot.SnapshotID, &snapshot.RootPath, &snapshot.ManifestSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return SkillSnapshotView{}, domainError(CodeStateCorrupt, "flow v2 Optimization has no frozen skill snapshot")
	}
	if err != nil {
		return SkillSnapshotView{}, fmt.Errorf("read frozen skill snapshot: %w", err)
	}
	rows, err := e.db.QueryContext(ctx, `SELECT name, repository, branch, commit_sha, relative_path, content_sha256
		FROM skill_snapshot_entries WHERE optimization_id = ? ORDER BY ordinal`, optimizationID)
	if err != nil {
		return SkillSnapshotView{}, fmt.Errorf("read frozen skill snapshot entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry SkillSnapshotEntry
		if err := rows.Scan(&entry.Name, &entry.Repository, &entry.Branch, &entry.CommitSHA, &entry.RelativePath, &entry.ContentSHA256); err != nil {
			return SkillSnapshotView{}, fmt.Errorf("scan frozen skill snapshot entry: %w", err)
		}
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return SkillSnapshotView{}, fmt.Errorf("iterate frozen skill snapshot entries: %w", err)
	}
	if snapshot.SchemaVersion != SkillSnapshotSchemaV1 || !isSHA256(snapshot.SnapshotID) || !filepath.IsAbs(snapshot.RootPath) ||
		!isSHA256(snapshot.ManifestSHA256) || len(snapshot.Entries) != len(expectedSources) {
		return SkillSnapshotView{}, domainError(CodeStateCorrupt, "frozen skill snapshot has invalid shape")
	}
	for index, expected := range expectedSources {
		entry := snapshot.Entries[index]
		if entry.Name != expected.name || entry.Repository != expected.repository || entry.Branch != expected.branch || entry.RelativePath != expected.path ||
			!isGitSHA(entry.CommitSHA) || !isSHA256(entry.ContentSHA256) {
			return SkillSnapshotView{}, domainError(CodeStateCorrupt, "frozen skill snapshot has invalid entries")
		}
	}
	return snapshot, nil
}

func readOptimization(ctx context.Context, tx *sql.Tx) (optimizationRecord, error) {
	var optimization optimizationRecord
	err := tx.QueryRowContext(ctx, `SELECT id, status, revision, flow_version FROM optimizations LIMIT 1`).Scan(&optimization.ID, &optimization.Status, &optimization.Revision, &optimization.FlowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return optimizationRecord{}, domainError(CodeNotInitialized, "optimization is not initialized")
	}
	if err != nil {
		return optimizationRecord{}, fmt.Errorf("read optimization: %w", err)
	}
	return optimization, nil
}

func checkExpectedRevision(expected *int64, actual int64) error {
	if expected != nil && *expected != actual {
		return domainError(CodeRevisionConflict, fmt.Sprintf("expected revision %d, current revision is %d", *expected, actual))
	}
	return nil
}
