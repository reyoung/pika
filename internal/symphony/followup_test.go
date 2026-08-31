package symphony_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

func TestFollowUpFakeClockActivityGenerationAndSubmission(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	targetWork, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	stop := json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop","last_assistant_message":"still investigating"}`)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, stop); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if len(view.FollowUps) != 1 || view.FollowUps[0].Status != "waiting" || view.FollowUps[0].InactivityTimeoutMS != 300000 {
		t.Fatalf("armed Follow-up = %+v", view.FollowUps)
	}
	if view.PaneActivityNotice == "" {
		t.Fatal("status hides pane.updated approximation")
	}

	now = now.Add(4 * time.Minute)
	if err := engine.ObservePaneActivity(ctx, "target-pane"); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.FollowUps[0].ActivitySource != "pane.updated" || view.FollowUps[0].LastObservedPaneActivityAt == "" {
		t.Fatalf("pane activity = %+v", view.FollowUps[0])
	}
	now = now.Add(4 * time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || promoted {
		t.Fatalf("promoted before reset deadline: promoted=%v err=%v", promoted, err)
	}
	now = now.Add(2 * time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote due Follow-up: promoted=%v err=%v", promoted, err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	request := view.FollowUps[0]
	if request.Status != "generating" || request.GeneratorWorkID == "" {
		t.Fatalf("generating Follow-up = %+v", request)
	}
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
		}
	}
	if generator.Role != symphony.RoleFollowUp || generator.ParentWorkID != targetWork.ID {
		t.Fatalf("generator Work = %+v", generator)
	}
	grant := runningGrantForWork(t, ctx, engine, generator, "follow-up-session")
	app := toolapp.Application{Store: engine}
	result, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"follow-up-message","message":"请继续完成全量验证并调用 finish_baseline_verification。"}`)})
	if err != nil || !result.Terminal {
		t.Fatalf("submit Follow-up: result=%+v err=%v", result, err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.FollowUps[0].Status != "ready" || view.FollowUps[0].Message == "" {
		t.Fatalf("ready Follow-up = %+v", view.FollowUps[0])
	}
}

func TestActivityDuringGenerationSupersedesWithoutCancellingGenerator(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	_, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	generatorID := view.FollowUps[0].GeneratorWorkID
	if err := engine.ObservePaneActivity(ctx, "target-pane"); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if len(view.FollowUps) != 2 || view.FollowUps[0].Status != "superseded" || view.FollowUps[1].Status != "waiting" {
		t.Fatalf("activity supersession = %+v", view.FollowUps)
	}
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == generatorID {
			generator = work
		}
	}
	if generator.Status != symphony.WorkPending {
		t.Fatalf("generator was interrupted: %+v", generator)
	}
	grant := runningGrantForWork(t, ctx, engine, generator, "superseded-generator")
	if _, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"late-message","message":"late generated guidance"}`)}); err != nil {
		t.Fatalf("late generator submission: %v", err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.FollowUps[0].Status != "superseded" || view.FollowUps[0].Message != "late generated guidance" || view.FollowUps[1].Status != "waiting" {
		t.Fatalf("late message audit = %+v", view.FollowUps)
	}
}

func TestTargetFollowUpBudgetExhaustionPausesBaselineVerification(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute,
		FollowUpPolicies: map[symphony.WorkRole]symphony.FollowUpPolicy{
			symphony.RoleBaselineVerification: {MaxMessages: 1, GeneratorMaxAttempts: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	target, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote first Follow-up: %v %v", promoted, err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	request := view.FollowUps[0]
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
		}
	}
	grant := runningGrantForWork(t, ctx, engine, generator, "budget-generator")
	if _, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"budget-message","message":"continue"}`)}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	request = view.FollowUps[0]
	if _, _, deliver, err := engine.BeginFollowUpDelivery(ctx, request.ID, request.DeliveryID); err != nil || !deliver {
		t.Fatalf("begin delivery: deliver=%v err=%v", deliver, err)
	}
	if err := engine.ObservePaneActivity(ctx, "target-pane"); err != nil {
		t.Fatalf("observe delivery-caused pane update: %v", err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"codex-target","turn_id":"follow-up-delivery","hook_event_name":"UserPromptSubmit","prompt":"continue"}`)); err != nil {
		t.Fatalf("record provider echo of delivered prompt: %v", err)
	}
	dispatching, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || dispatching.FollowUps[0].Status != "dispatching" {
		t.Fatalf("delivery-caused activity superseded dispatch: %+v err=%v", dispatching.FollowUps, err)
	}
	if err := engine.FinishFollowUpDelivery(ctx, request.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-2","hook_event_name":"UserPromptSubmit","prompt":"continue"}`),
		json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-2","hook_event_name":"Stop"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.Optimization.Status != symphony.OptimizationPaused {
		t.Fatalf("optimization did not pause: %+v", view.Optimization)
	}
	for _, work := range view.Works {
		if work.ID == target.ID && work.Status != symphony.WorkCancelled {
			t.Fatalf("target Work was not cancelled: %+v", work)
		}
	}
	if len(view.FollowUps) != 2 || view.FollowUps[0].Status != "delivered" || view.FollowUps[1].Status != "target_exhausted" {
		t.Fatalf("Follow-up budget audit = %+v", view.FollowUps)
	}
}

func TestGeneratorRetriesFreshSessionsThenExhaustsItsOwnBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute,
		FollowUpPolicies: map[symphony.WorkRole]symphony.FollowUpPolicy{
			symphony.RoleBaselineVerification: {MaxMessages: 4, GeneratorMaxAttempts: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	target, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	request := view.FollowUps[0]
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
		}
	}
	first := runningGrantForWork(t, ctx, engine, generator, "generator-attempt-1")
	replacementID, err := engine.ReplaceLostAgentSession(ctx, first.AgentSessionID)
	if err != nil || replacementID == "" {
		t.Fatalf("first generator loss did not retry: id=%q err=%v", replacementID, err)
	}
	replacement := symphony.AgentSession{ID: replacementID, WorkID: generator.ID, Generation: generator.Generation, Role: symphony.RoleFollowUp, AgentKind: "codex", AgentName: "generator-attempt-2", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, replacement.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "retry-tab", PaneID: "retry-pane", TerminalID: "retry-terminal"}); err != nil {
		t.Fatal(err)
	}
	if nextID, err := engine.ReplaceLostAgentSession(ctx, replacement.ID); err != nil || nextID != "" {
		t.Fatalf("generator exhaustion scheduled another retry: id=%q err=%v", nextID, err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.Optimization.Status != symphony.OptimizationPaused || view.FollowUps[0].Status != "generator_exhausted" || view.FollowUps[0].GeneratorAttempts != 2 {
		t.Fatalf("generator exhaustion state = optimization=%+v followups=%+v", view.Optimization, view.FollowUps)
	}
	for _, work := range view.Works {
		if (work.ID == target.ID || work.ID == generator.ID) && work.Status != symphony.WorkCancelled {
			t.Fatalf("exhausted Work remained active: %+v", work)
		}
	}
}

func TestIterationFollowUpExhaustionRejectsOnlyThatAttempt(t *testing.T) {
	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	if err := engine.SetFollowUpPolicies(map[symphony.WorkRole]symphony.FollowUpPolicy{
		symphony.RoleIteration: {MaxMessages: 1, GeneratorMaxAttempts: 2},
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	target := pendingWorksByRole(before, symphony.RoleIteration)[0]
	targetSession := symphony.AgentSession{ID: "iteration-target-session", WorkID: target.ID, Generation: target.Generation, Role: target.Role, AgentKind: "codex", AgentName: "iteration-target", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, targetSession); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, targetSession.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "iteration-tab", PaneID: "iteration-pane", TerminalID: "iteration-terminal"}); err != nil {
		t.Fatal(err)
	}
	exhaustTargetWithOneDeliveredMessage(t, ctx, engine, targetSession, "iteration-pane")
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationOptimizing || after.Best == nil || after.Best.Sequence != 0 {
		t.Fatalf("Iteration exhaustion changed Optimization/Best: optimization=%+v best=%+v", after.Optimization, after.Best)
	}
	if attempt := attemptByID(t, after, target.AttemptID); attempt.Status != "rejected" || attempt.FailureReason != "followup_exhausted" {
		t.Fatalf("Attempt exhaustion = %+v", attempt)
	}
	if got := len(pendingWorksByRole(after, symphony.RoleIteration)); got != 4 {
		t.Fatalf("Iteration capacity was not refilled: %d", got)
	}
}

func TestIntegrationFollowUpExhaustionRejectsWithoutAdvancingBest(t *testing.T) {
	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	if err := engine.SetFollowUpPolicies(map[symphony.WorkRole]symphony.FollowUpPolicy{
		symphony.RoleIntegration: {MaxMessages: 1, GeneratorMaxAttempts: 2},
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	iteration := pendingWorksByRole(before, symphony.RoleIteration)[0]
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "candidate-for-exhaustion"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-sha", Summary: "candidate awaiting Integration", Evidence: validBenchmarkEvidence(),
	}); err != nil {
		t.Fatal(err)
	}
	queued, _ := engine.Inspect(ctx, symphony.Status{})
	target := pendingWorkByRole(t, queued, symphony.RoleIntegration)
	targetSession := symphony.AgentSession{ID: "integration-target-session", WorkID: target.ID, Generation: target.Generation, Role: target.Role, AgentKind: "codex", AgentName: "integration-target", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, targetSession); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, targetSession.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "integration-tab", PaneID: "integration-pane", TerminalID: "integration-terminal"}); err != nil {
		t.Fatal(err)
	}
	exhaustTargetWithOneDeliveredMessage(t, ctx, engine, targetSession, "integration-pane")
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationOptimizing || after.Best == nil || after.Best.Sequence != 0 || after.Best.CommitSHA != "baseline-sha" {
		t.Fatalf("Integration exhaustion changed Optimization/Best: optimization=%+v best=%+v", after.Optimization, after.Best)
	}
	if attempt := attemptByID(t, after, target.AttemptID); attempt.Status != "rejected" || attempt.FailureReason != "followup_exhausted" {
		t.Fatalf("Integration Attempt exhaustion = %+v", attempt)
	}
	if after.Integrations[0].Status != "rejected" {
		t.Fatalf("Integration was not rejected: %+v", after.Integrations)
	}
	if got := len(pendingWorksByRole(after, symphony.RoleIteration)); got != 4 {
		t.Fatalf("Iteration capacity was not refilled after Integration exhaustion: %d", got)
	}
}

func TestFollowUpExhaustionWhileDrainingStartsNoPrimaryWork(t *testing.T) {
	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	if err := engine.SetFollowUpPolicies(map[symphony.WorkRole]symphony.FollowUpPolicy{
		symphony.RoleIteration: {MaxMessages: 2, GeneratorMaxAttempts: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetFollowUpInactivity(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	target := pendingWorksByRole(view, symphony.RoleIteration)[0]
	targetSession := symphony.AgentSession{ID: "draining-target", WorkID: target.ID, Generation: target.Generation, Role: target.Role, AgentKind: "codex", AgentName: "draining-target", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, targetSession); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, targetSession.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "draining-target-tab", PaneID: "draining-target-pane", TerminalID: "draining-target-terminal"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"draining-provider","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote: %v %v", promoted, err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	request := view.FollowUps[0]
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
		}
	}
	grant := runningGrantForWork(t, ctx, engine, generator, "draining-generator")
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "drain-before-exhaustion"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	if replacement, err := engine.ReplaceLostAgentSession(ctx, grant.AgentSessionID); err != nil || replacement != "" {
		t.Fatalf("exhaust generator while draining: replacement=%q err=%v", replacement, err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationDraining || len(after.Works) != len(before.Works) {
		t.Fatalf("draining exhaustion started Work or changed lifecycle: before=%d after=%d optimization=%+v", len(before.Works), len(after.Works), after.Optimization)
	}
}

func exhaustTargetWithOneDeliveredMessage(t *testing.T, ctx context.Context, engine *symphony.Engine, targetSession symphony.AgentSession, paneID string) {
	t.Helper()
	if err := engine.SetFollowUpInactivity(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"target-provider","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote Follow-up: %v %v", promoted, err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	request := view.FollowUps[len(view.FollowUps)-1]
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
		}
	}
	grant := runningGrantForWork(t, ctx, engine, generator, "generator-"+targetSession.ID)
	if _, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"message-` + targetSession.ID + `","message":"continue"}`)}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	request = view.FollowUps[len(view.FollowUps)-1]
	if targetWorkID, gotPane, deliver, err := engine.BeginFollowUpDelivery(ctx, request.ID, request.DeliveryID); err != nil || !deliver || targetWorkID != targetSession.WorkID || gotPane != paneID {
		t.Fatalf("begin Follow-up delivery: target=%q pane=%q deliver=%v err=%v", targetWorkID, gotPane, deliver, err)
	}
	if err := engine.FinishFollowUpDelivery(ctx, request.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, event := range []json.RawMessage{
		json.RawMessage(`{"session_id":"target-provider","turn_id":"turn-2","hook_event_name":"UserPromptSubmit","prompt":"continue"}`),
		json.RawMessage(`{"session_id":"target-provider","turn_id":"turn-2","hook_event_name":"Stop"}`),
	} {
		if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, event); err != nil {
			t.Fatal(err)
		}
	}
}

func eligibleFollowUpTarget(t *testing.T, ctx context.Context, engine *symphony.Engine) (symphony.WorkView, symphony.AgentSession) {
	t.Helper()
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: validBaselineDefinition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	var work symphony.WorkView
	for _, candidate := range view.Works {
		if candidate.Role == symphony.RoleBaselineVerification && candidate.Status == symphony.WorkPending {
			work = candidate
		}
	}
	session := symphony.AgentSession{ID: "target-session", WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: "target-agent", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal"}); err != nil {
		t.Fatal(err)
	}
	return work, session
}

func runningGrantForWork(t *testing.T, ctx context.Context, engine *symphony.Engine, work symphony.WorkView, sessionID string) symphony.AgentGrant {
	t.Helper()
	session := symphony.AgentSession{ID: sessionID, WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: sessionID, Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab-" + sessionID, PaneID: "pane-" + sessionID, TerminalID: "terminal-" + sessionID}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(work.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}
