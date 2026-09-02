package symphony

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

func (e *Engine) MintAgentGrant(ctx context.Context, sessionID string, catalog []string, ttl time.Duration) (AgentGrant, error) {
	if sessionID == "" || len(catalog) == 0 || ttl <= 0 {
		return AgentGrant{}, errors.New("session ID, non-empty catalog, and positive TTL are required")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return AgentGrant{}, fmt.Errorf("generate agent grant: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	catalogJSON := mustJSON(catalog)

	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentGrant{}, fmt.Errorf("begin agent grant: %w", err)
	}
	defer tx.Rollback()
	grant := AgentGrant{ID: e.newID(), Token: token, AgentSessionID: sessionID, Catalog: catalogJSON}
	if err := tx.QueryRowContext(ctx, `SELECT work_id, generation, role, agent_kind, agent_name FROM agent_sessions
		WHERE id = ? AND status IN ('starting', 'running')`, sessionID).Scan(&grant.WorkID, &grant.Generation, &grant.Role, &grant.AgentKind, &grant.AgentName); errors.Is(err, sql.ErrNoRows) {
		return AgentGrant{}, domainError(CodeInvalidTransition, "agent session is not current")
	} else if err != nil {
		return AgentGrant{}, fmt.Errorf("read agent session for grant: %w", err)
	}
	now := e.now().UTC()
	grant.ExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE session_grants SET revoked_at = ?
		WHERE agent_session_id = ? AND revoked_at IS NULL`, now.Format(time.RFC3339Nano), sessionID); err != nil {
		return AgentGrant{}, fmt.Errorf("revoke prior agent grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_grants
		(id, agent_session_id, token_sha256, catalog_json, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, grant.ID, sessionID, hex.EncodeToString(hash[:]), catalogJSON, grant.ExpiresAt, now.Format(time.RFC3339Nano)); err != nil {
		return AgentGrant{}, fmt.Errorf("store agent grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AgentGrant{}, fmt.Errorf("commit agent grant: %w", err)
	}
	return grant, nil
}

func (e *Engine) ResolveAgentGrant(ctx context.Context, token string) (AgentGrant, error) {
	if token == "" {
		return AgentGrant{}, domainError(CodeForbidden, "agent grant is required")
	}
	hash := sha256.Sum256([]byte(token))
	var grant AgentGrant
	var revokedAt sql.NullString
	err := e.db.QueryRowContext(ctx, `SELECT g.id, g.agent_session_id, s.work_id, s.generation, s.role, s.agent_kind, s.agent_name, s.status,
		g.catalog_json, g.expires_at, g.revoked_at
		FROM session_grants g JOIN agent_sessions s ON s.id = g.agent_session_id
		WHERE g.token_sha256 = ?`, hex.EncodeToString(hash[:])).Scan(
		&grant.ID, &grant.AgentSessionID, &grant.WorkID, &grant.Generation, &grant.Role, &grant.AgentKind, &grant.AgentName, &grant.SessionStatus,
		&grant.Catalog, &grant.ExpiresAt, &revokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentGrant{}, domainError(CodeForbidden, "agent grant is invalid")
	}
	if err != nil {
		return AgentGrant{}, fmt.Errorf("resolve agent grant: %w", err)
	}
	grant.Revoked = revokedAt.Valid
	expiresAt, err := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if err != nil {
		return AgentGrant{}, domainError(CodeStateCorrupt, "agent grant expiry is invalid")
	}
	if !e.now().UTC().Before(expiresAt) {
		return AgentGrant{}, domainError(CodeForbidden, "agent grant is expired")
	}
	return grant, nil
}

func (e *Engine) RevokeAgentGrant(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("agent session ID is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.db.ExecContext(ctx, `UPDATE session_grants SET revoked_at = ?
		WHERE agent_session_id = ? AND revoked_at IS NULL`, e.timestamp(), sessionID); err != nil {
		return fmt.Errorf("revoke agent grant: %w", err)
	}
	return nil
}
