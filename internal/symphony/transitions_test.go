package symphony_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestRejectedBaselineTransitionsDoNotMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, context.Context) (*symphony.Engine, symphony.View)
		apply   func(symphony.View) symphony.Command
		code    symphony.ErrorCode
	}{
		{
			name:    "submit definition twice",
			prepare: submittedState,
			apply: func(view symphony.View) symphony.Command {
				return symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "again"}, WorkID: view.Works[0].ID, Definition: json.RawMessage(`{"target":"other"}`)}
			},
			code: symphony.CodeInvalidTransition,
		},
		{
			name:    "finish verification while drafting",
			prepare: draftState,
			apply: func(view symphony.View) symphony.Command {
				return symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: view.Works[0].ID, Decision: symphony.VerificationAccepted}
			},
			code: symphony.CodeInvalidTransition,
		},
		{
			name:    "reject without diagnostics",
			prepare: submittedState,
			apply: func(view symphony.View) symphony.Command {
				return symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: view.Works[len(view.Works)-1].ID, Decision: symphony.VerificationRejected}
			},
			code: symphony.CodeInvalidCommand,
		},
		{
			name:    "back off from draft",
			prepare: draftState,
			apply: func(view symphony.View) symphony.Command {
				return symphony.BackOff{Meta: symphony.CommandMeta{RequestID: "back-off"}, WorkID: view.Works[0].ID, Message: "go earlier"}
			},
			code: symphony.CodeInvalidTransition,
		},
		{
			name:    "cancel terminal work",
			prepare: submittedState,
			apply: func(view symphony.View) symphony.Command {
				return symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: view.Works[0].ID}
			},
			code: symphony.CodeWorkTerminal,
		},
		{
			name:    "cancel unknown work",
			prepare: draftState,
			apply: func(symphony.View) symphony.Command {
				return symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: "missing"}
			},
			code: symphony.CodeWorkNotFound,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			engine, before := test.prepare(t, ctx)
			_, err := engine.Apply(ctx, test.apply(before))
			assertDomainCode(t, err, test.code)
			after, inspectErr := engine.Inspect(ctx, symphony.Status{})
			if inspectErr != nil {
				t.Fatalf("inspect after rejection: %v", inspectErr)
			}
			if after.Optimization.Revision != before.Optimization.Revision || after.DomainEventCount != before.DomainEventCount || after.PendingEffectCount != before.PendingEffectCount {
				t.Fatalf("rejected command mutated state: before %+v, after %+v", before, after)
			}
		})
	}
}

func TestBackOffFromCancelledVerificationIsAllowed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, before := submittedState(t, ctx)
	work := before.Works[len(before.Works)-1]
	if _, err := engine.Apply(ctx, symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: work.ID}); err != nil {
		t.Fatalf("cancel verification: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.BackOff{Meta: symphony.CommandMeta{RequestID: "back-off"}, WorkID: work.ID, Message: "revise measurement"}); err != nil {
		t.Fatalf("back off cancelled verification: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationDraftingBaseline || len(after.Baselines) != 2 {
		t.Fatalf("view after paused back-off = %+v", after)
	}
}

func TestStartBaselineDraftRecoversCancelledDraftWithNewWork(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, draft := draftState(t, ctx)
	if _, err := engine.Apply(ctx, symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: draft.Works[0].ID}); err != nil {
		t.Fatalf("cancel draft: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.StartBaselineDraft{Meta: symphony.CommandMeta{RequestID: "restart"}}); err != nil {
		t.Fatalf("restart baseline draft: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationDraftingBaseline || len(after.Works) != 2 {
		t.Fatalf("recovered draft view = %+v", after)
	}
	if after.Works[0].Status != symphony.WorkCancelled || after.Works[1].Status != symphony.WorkPending || after.Works[1].Role != symphony.RoleBaselineDraft || after.Works[1].Generation != 2 {
		t.Fatalf("recovered works = %+v", after.Works)
	}
}

func TestBaselineLifecycleSurvivesReopenBetweenEveryTransition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("init")})
	if err != nil {
		t.Fatalf("open for init: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	initial, _ := engine.Inspect(ctx, symphony.Status{})
	if err := engine.Close(); err != nil {
		t.Fatalf("close after init: %v", err)
	}

	engine, err = symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("submit")})
	if err != nil {
		t.Fatalf("open for submit: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: initial.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`)}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	submitted, _ := engine.Inspect(ctx, symphony.Status{})
	if err := engine.Close(); err != nil {
		t.Fatalf("close after submit: %v", err)
	}

	engine, err = symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("finish")})
	if err != nil {
		t.Fatalf("open for finish: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: submitted.Works[len(submitted.Works)-1].ID, Decision: symphony.VerificationAccepted}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	finished, _ := engine.Inspect(ctx, symphony.Status{})
	if finished.Optimization.Status != symphony.OptimizationOptimizing || finished.Baseline.Status != symphony.BaselineAccepted || finished.Optimization.Revision != 3 {
		t.Fatalf("finished view = %+v", finished)
	}
}

func TestDrainingDraftMayFinishButDoesNotStartVerification(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, draft := draftState(t, ctx)
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
		t.Fatalf("request shutdown: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "submit"},
		WorkID:     draft.Works[0].ID,
		Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatalf("finish draft while draining: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationDraining || after.Baseline.Status != symphony.BaselineSubmitted {
		t.Fatalf("draining draft view = %+v", after)
	}
	if len(after.Works) != 1 || after.Works[0].Status != symphony.WorkCompleted {
		t.Fatalf("draining draft started successor work: %+v", after.Works)
	}
}

func TestDrainReadyRequiresNoPendingWorkActiveSessionOrRuntimeEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, draft := draftState(t, ctx)

	ready, err := engine.DrainReady(ctx)
	if err != nil || ready {
		t.Fatalf("ready before shutdown = %v, err=%v", ready, err)
	}
	initialEffects, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(initialEffects) != 1 {
		t.Fatalf("claim initial effect = %+v, err=%v", initialEffects, err)
	}
	if err := engine.MarkEffectDispatched(ctx, initialEffects[0].ID); err != nil {
		t.Fatal(err)
	}
	session := symphony.AgentSession{
		ID: "draft-session", WorkID: draft.Works[0].ID, Generation: draft.Works[0].Generation,
		Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "draft-agent", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkAgentSessionEnded(ctx, session.ID, symphony.AgentSessionExited); err != nil {
		t.Fatal(err)
	}
	if ready, err := engine.DrainReady(ctx); err != nil || ready {
		t.Fatalf("ready with pending Work only = %v, err=%v", ready, err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: draft.Works[0].ID,
		Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if ready, err := engine.DrainReady(ctx); err != nil || ready {
		t.Fatalf("ready with close effect = %v, err=%v", ready, err)
	}
	closeEffects, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(closeEffects) != 1 || closeEffects[0].Type != "work.close_requested" {
		t.Fatalf("claim close effect = %+v, err=%v", closeEffects, err)
	}
	if err := engine.MarkEffectDispatched(ctx, closeEffects[0].ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := engine.DrainReady(ctx); err != nil || !ready {
		t.Fatalf("ready after normal child exit = %v, err=%v", ready, err)
	}
}

func TestDrainingVerificationMayFinishWithoutStartingSuccessor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		finish       symphony.FinishBaselineVerification
		wantBaseline symphony.BaselineStatus
	}{
		{name: "accepted", finish: symphony.FinishBaselineVerification{Decision: symphony.VerificationAccepted}, wantBaseline: symphony.BaselineAccepted},
		{name: "rejected", finish: symphony.FinishBaselineVerification{Decision: symphony.VerificationRejected, FailureKind: "invalid", Reason: "failed", RequestedChanges: "fix it"}, wantBaseline: symphony.BaselineRejected},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			engine, submitted := submittedState(t, ctx)
			if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
				t.Fatalf("request shutdown: %v", err)
			}
			command := test.finish
			command.Meta = symphony.CommandMeta{RequestID: "finish"}
			command.WorkID = submitted.Works[len(submitted.Works)-1].ID
			if _, err := engine.Apply(ctx, command); err != nil {
				t.Fatalf("finish verification while draining: %v", err)
			}
			after, _ := engine.Inspect(ctx, symphony.Status{})
			if after.Optimization.Status != symphony.OptimizationDraining || after.Baseline.Status != test.wantBaseline {
				t.Fatalf("draining verification view = %+v", after)
			}
			if len(after.Baselines) != 1 || len(after.Works) != 2 || after.Works[1].Status != symphony.WorkCompleted {
				t.Fatalf("draining verification started successor: baselines %+v, works %+v", after.Baselines, after.Works)
			}
		})
	}
}

func draftState(t *testing.T, ctx context.Context) (*symphony.Engine, symphony.View) {
	t.Helper()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect draft: %v", err)
	}
	return engine, view
}

func submittedState(t *testing.T, ctx context.Context) (*symphony.Engine, symphony.View) {
	t.Helper()
	engine, draft := draftState(t, ctx)
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: draft.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`)}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect submitted: %v", err)
	}
	return engine, view
}

func acceptedState(t *testing.T, ctx context.Context) (*symphony.Engine, symphony.View) {
	t.Helper()
	engine, submitted := submittedState(t, ctx)
	work := submitted.Works[len(submitted.Works)-1]
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "finish"}, WorkID: work.ID, Decision: symphony.VerificationAccepted}); err != nil {
		t.Fatalf("accept baseline: %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect accepted state: %v", err)
	}
	return engine, view
}

func pausedState(t *testing.T, ctx context.Context) (*symphony.Engine, symphony.View) {
	t.Helper()
	engine, draft := draftState(t, ctx)
	if _, err := engine.Apply(ctx, symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: draft.Works[0].ID}); err != nil {
		t.Fatalf("cancel draft: %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect paused state: %v", err)
	}
	return engine, view
}

func prefixedCountingIDs(prefix string) func() string {
	index := 0
	return func() string {
		index++
		return prefix + "-id-" + string(rune('a'+index-1))
	}
}
