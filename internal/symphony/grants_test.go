package symphony

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentGrantPersistsOnlyHashAndExpires(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-test", Status: AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, []string{"commit_changes", "submit_baseline_definition"}, time.Hour)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	var storedHash, catalog string
	if err := engine.db.QueryRowContext(ctx, `SELECT token_sha256, catalog_json FROM session_grants WHERE id = ?`, grant.ID).Scan(&storedHash, &catalog); err != nil {
		t.Fatalf("read stored grant: %v", err)
	}
	if strings.Contains(storedHash, grant.Token) || len(storedHash) != 64 || catalog != `["commit_changes","submit_baseline_definition"]` {
		t.Fatalf("stored grant hash=%q catalog=%q", storedHash, catalog)
	}
	resolved, err := engine.ResolveAgentGrant(ctx, grant.Token)
	if err != nil || resolved.WorkID != session.WorkID || resolved.Role != session.Role || resolved.Revoked {
		t.Fatalf("resolved grant=%+v err=%v", resolved, err)
	}
	now = now.Add(time.Hour)
	if _, err := engine.ResolveAgentGrant(ctx, grant.Token); err == nil || !strings.Contains(err.Error(), string(CodeForbidden)) {
		t.Fatalf("expired grant error = %v", err)
	}
}

func TestGrantRevocationRetainsIdentityForIdempotentTerminalReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-test", Status: AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, []string{"submit_baseline_definition"}, time.Hour)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	if err := engine.RevokeAgentGrant(ctx, session.ID); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	resolved, err := engine.ResolveAgentGrant(ctx, grant.Token)
	if err != nil || !resolved.Revoked {
		t.Fatalf("resolved revoked grant=%+v err=%v", resolved, err)
	}
}
