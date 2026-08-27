package symphony_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestAgentSessionAndPaneBindingSurviveReopenAndPaneMove(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("runtime")})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	work := view.Works[0]
	session := symphony.AgentSession{
		ID:         "session-1",
		WorkID:     work.ID,
		Generation: work.Generation,
		Role:       work.Role,
		AgentKind:  "codex",
		AgentName:  "pika-baseline-1",
		Status:     symphony.AgentSessionRunning,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure agent session: %v", err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{
		WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2", TerminalID: "term-1",
	}); err != nil {
		t.Fatalf("bind pane: %v", err)
	}
	if err := engine.ObservePaneMove(ctx, "term-1", symphony.PaneBinding{
		WorkspaceID: "w2", TabID: "w2:t1", PaneID: "w2:p1", TerminalID: "term-1",
	}); err != nil {
		t.Fatalf("observe pane move: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}

	reopened, err := symphony.Open(ctx, path, symphony.Options{})
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	current, binding, found, err := reopened.CurrentAgentSession(ctx, work.ID)
	if err != nil {
		t.Fatalf("read current agent session: %v", err)
	}
	if !found || current.ID != session.ID || current.Status != symphony.AgentSessionRunning {
		t.Fatalf("current session = %+v, found = %v", current, found)
	}
	if binding.TerminalID != "term-1" || binding.PaneID != "w2:p1" || binding.WorkspaceID != "w2" {
		t.Fatalf("pane binding = %+v", binding)
	}
}

func TestReplaceLostAgentSessionAtomicallyEnqueuesFreshStart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: prefixedCountingIDs("replacement")})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	work := view.Works[0]
	session := symphony.AgentSession{ID: "session-1", WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: "pika-test", Status: symphony.AgentSessionRunning}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2", TerminalID: "term-1"}); err != nil {
		t.Fatalf("bind pane: %v", err)
	}
	before, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil {
		t.Fatalf("pending effects before replacement: %v", err)
	}
	for _, effect := range before {
		if err := engine.MarkEffectDispatched(ctx, effect.ID); err != nil {
			t.Fatalf("ack initial effect: %v", err)
		}
	}

	effectID, err := engine.ReplaceLostAgentSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("replace lost session: %v", err)
	}
	if effectID == "" {
		t.Fatal("replacement effect ID is empty")
	}
	if _, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("current session after replacement = found %v, err %v", found, err)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatalf("pending effects: %v", err)
	}
	if len(effects) != 2 || effects[0].Type != "session.close_requested" || effects[1].ID != effectID || effects[1].Type != "work.start_requested" {
		t.Fatalf("replacement effects = %+v", effects)
	}
}

func TestAgentSessionHistoryPreservesFreshRecoverySessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := view.Works[0]
	first := symphony.AgentSession{ID: "session-1", WorkID: work.ID, Generation: 1, Role: work.Role, AgentKind: "codex", AgentName: "pika-test", Status: symphony.AgentSessionRunning}
	if err := engine.EnsureAgentSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, first.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "t1", PaneID: "p1", TerminalID: "term-1"}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, first.ID, []string{"get_context"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceLostAgentSession(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := engine.ResolveAgentGrant(ctx, grant.Token)
	if err != nil || !resolved.Revoked {
		t.Fatalf("recovery did not revoke old Session grant: grant=%+v err=%v", resolved, err)
	}
	second := symphony.AgentSession{ID: "session-2", WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: "pika-test-replacement", Status: symphony.AgentSessionRunning}
	if err := engine.EnsureAgentSession(ctx, second); err != nil {
		t.Fatal(err)
	}

	history, err := engine.AgentSessionHistory(ctx, work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].ID != first.ID || history[0].Status != symphony.AgentSessionLost || history[1].ID != second.ID || history[1].Generation != work.Generation {
		t.Fatalf("Agent Session history = %+v", history)
	}
}

func TestRecoveryLeavesTerminalWorkSessionForCommittedCloseEffect(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: prefixedCountingIDs("terminal-recovery")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	draft := view.Works[0]
	session := symphony.AgentSession{ID: "draft-session", WorkID: draft.ID, Generation: draft.Generation, Role: draft.Role, AgentKind: "codex", AgentName: "pika-draft", Status: symphony.AgentSessionRunning}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	initial, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial effects=%+v err=%v", initial, err)
	}
	if err := engine.MarkEffectDispatched(ctx, initial[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "terminal"}, WorkID: draft.ID, Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatal(err)
	}

	if err := engine.RetireActiveSessionsForRecovery(ctx); err != nil {
		t.Fatalf("retire after terminal commit: %v", err)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 2 || effects[0].Type != "work.close_requested" || effects[1].Type != "work.start_requested" {
		t.Fatalf("terminal recovery changed committed close/start effects: %+v", effects)
	}
	active, err := engine.ActiveAgentSessions(ctx)
	if err != nil || len(active) != 1 || active[0].Session.ID != session.ID {
		t.Fatalf("terminal Session should remain available to committed close effect: active=%+v err=%v", active, err)
	}
}
