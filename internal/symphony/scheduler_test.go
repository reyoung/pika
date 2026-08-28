package symphony_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestSchedulerPausePersistsTargetsAndGatesStarts(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	if before.Scheduler.Status != symphony.SchedulerRunning || before.Scheduler.Epoch != 0 {
		t.Fatalf("initial Scheduler = %+v", before.Scheduler)
	}

	pause, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}})
	if err != nil {
		t.Fatal(err)
	}
	paused, _ := engine.Inspect(ctx, symphony.Status{})
	if paused.Scheduler.Status != symphony.SchedulerPaused || paused.Scheduler.PausedAt == "" || paused.Scheduler.Epoch != 1 {
		t.Fatalf("paused Scheduler = %+v", paused.Scheduler)
	}
	if pause.Revision != before.Optimization.Revision+1 || paused.Scheduler.Latest == nil || paused.Scheduler.Latest.Action != "pause" {
		t.Fatalf("pause receipt=%+v Scheduler=%+v", pause, paused.Scheduler)
	}
	claimed, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil || len(claimed) != 1 || claimed[0].Type != "scheduler.control_requested" {
		t.Fatalf("effects while paused=%+v err=%v", claimed, err)
	}
	if err := engine.MarkEffectDispatched(ctx, claimed[0].ID); err != nil {
		t.Fatal(err)
	}

	noop, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause-again"}})
	if err != nil {
		t.Fatal(err)
	}
	if noop.Revision != pause.Revision {
		t.Fatalf("no-op pause revision=%d want %d", noop.Revision, pause.Revision)
	}
	var noopResult struct {
		Noop bool `json:"noop"`
	}
	if err := json.Unmarshal(noop.Result, &noopResult); err != nil || !noopResult.Noop {
		t.Fatalf("no-op result=%s err=%v", noop.Result, err)
	}
	if effects, err := engine.ClaimPendingEffects(ctx, 10); err != nil || len(effects) != 0 {
		t.Fatalf("no-op emitted effects=%+v err=%v", effects, err)
	}

	resume, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume"}})
	if err != nil {
		t.Fatal(err)
	}
	if resume.Revision != pause.Revision+1 {
		t.Fatalf("resume revision=%d", resume.Revision)
	}
	claimed, err = engine.ClaimPendingEffects(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Type != "scheduler.control_requested" {
		t.Fatalf("resume control priority=%+v err=%v", claimed, err)
	}
	if err := engine.MarkEffectDispatched(ctx, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = engine.ClaimPendingEffects(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Type != "work.start_requested" {
		t.Fatalf("held start after resume=%+v err=%v", claimed, err)
	}
}

func TestSchedulerPauseTargetsEveryActiveRoleAndRejectsShutdown(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	work := view.Works[0]
	if err := engine.EnsureAgentSession(ctx, symphony.AgentSession{ID: "session", WorkID: work.ID, Generation: work.Generation,
		Role: work.Role, AgentKind: "codex", AgentName: "pika-session", Status: symphony.AgentSessionRunning}); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, "session", symphony.PaneBinding{WorkspaceID: "w", TabID: "t", PaneID: "p", TerminalID: "term"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	paused, _ := engine.Inspect(ctx, symphony.Status{})
	if paused.Scheduler.Latest == nil || len(paused.Scheduler.Latest.Actions) != 1 {
		t.Fatalf("control targets=%+v", paused.Scheduler.Latest)
	}
	action := paused.Scheduler.Latest.Actions[0]
	if action.AgentSessionID != "session" || action.Role != symphony.RoleBaselineDraft || action.Action != "pause" {
		t.Fatalf("control target=%+v", action)
	}
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err == nil {
		t.Fatal("shutdown succeeded while Scheduler was paused")
	}
}

func TestSchedulerControlIsInvalidWhileDraining(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err == nil {
		t.Fatal("pause succeeded while draining")
	}
	if _, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume"}}); err == nil {
		t.Fatal("resume succeeded while draining")
	}
}

func TestSchedulerPausedStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := symphony.Open(ctx, path, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	view, err := reopened.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Scheduler.Status != symphony.SchedulerPaused || view.Scheduler.Epoch != 1 || view.Scheduler.Latest == nil {
		t.Fatalf("reopened Scheduler=%+v", view.Scheduler)
	}
	if effects, err := reopened.ClaimPendingEffects(ctx, 10); err != nil || len(effects) != 1 || effects[0].Type != "scheduler.control_requested" {
		t.Fatalf("reopened paused effects=%+v err=%v", effects, err)
	}
}

func TestSchedulerDispatchingActionRecoversAsDeliveryUnknown(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	work := view.Works[0]
	if err := engine.EnsureAgentSession(ctx, symphony.AgentSession{ID: "session", WorkID: work.ID, Generation: work.Generation,
		Role: work.Role, AgentKind: "codex", AgentName: "pika-session", Status: symphony.AgentSessionRunning}); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, "session", symphony.PaneBinding{WorkspaceID: "w", TabID: "t", PaneID: "p", TerminalID: "term"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	paused, _ := engine.Inspect(ctx, symphony.Status{})
	action := paused.Scheduler.Latest.Actions[0]
	if begun, err := engine.BeginSchedulerControlAction(ctx, action.ID, "working"); err != nil || !begun {
		t.Fatalf("begin action=%v err=%v", begun, err)
	}
	if err := engine.RecoverUncertainEffects(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := engine.SchedulerControlCycle(ctx, paused.Scheduler.Latest.ID)
	if err != nil || recovered.Actions[0].Status != symphony.SchedulerActionDeliveryUnknown {
		t.Fatalf("recovered cycle=%+v err=%v", recovered, err)
	}
}

func TestPausedSchedulerAcceptsTerminalDomainResultButHoldsSuccessorStart(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"},
		WorkID: before.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`)}); err != nil {
		t.Fatalf("terminal result while paused: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationVerifyingBaseline || len(after.Works) != 2 || after.Works[1].Status != symphony.WorkPending {
		t.Fatalf("domain successor while paused=%+v", after)
	}
	effects, err := engine.ClaimPendingEffects(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range effects {
		if effect.Type == "work.start_requested" {
			t.Fatalf("paused Scheduler released start effect %+v", effect)
		}
	}
}

func TestSchedulerPauseFreezesFollowUpClockAndResumeSupersedesWaitingTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: 5 * time.Minute, NewID: countingIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	_, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID,
		json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	originalDue, _ := time.Parse(time.RFC3339Nano, before.FollowUps[0].DueAt)
	now = now.Add(time.Minute)
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || promoted {
		t.Fatalf("paused Follow-up promotion=%v err=%v", promoted, err)
	}
	if _, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	shiftedDue, _ := time.Parse(time.RFC3339Nano, after.FollowUps[0].DueAt)
	if got := shiftedDue.Sub(originalDue); got != 10*time.Minute {
		t.Fatalf("deadline shift=%s want 10m", got)
	}
	if after.FollowUps[0].Status != "superseded" {
		t.Fatalf("waiting Follow-up status=%s want superseded", after.FollowUps[0].Status)
	}
}

func TestSchedulerGeneratedResumeActivityDoesNotSupersedeGeneratingFollowUp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute, NewID: countingIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	_, targetSession := eligibleFollowUpTarget(t, ctx, engine)
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID,
		json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"Stop"}`)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote=%v err=%v", promoted, err)
	}
	if _, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume"}}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	action := view.Scheduler.Latest.Actions[0]
	if begun, err := engine.BeginSchedulerControlAction(ctx, action.ID, "idle"); err != nil || !begun {
		t.Fatalf("begin resume=%v err=%v", begun, err)
	}
	if err := engine.FinishSchedulerControlAction(ctx, action.ID, symphony.SchedulerActionSent, ""); err != nil {
		t.Fatal(err)
	}
	if err := engine.ObservePaneActivity(ctx, "target-pane"); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID,
		json.RawMessage(`{"session_id":"codex-target","turn_id":"turn-resume","hook_event_name":"UserPromptSubmit","prompt":"继续"}`)); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if len(after.FollowUps) != 1 || after.FollowUps[0].Status != "generating" {
		t.Fatalf("Scheduler resume superseded Follow-up=%+v", after.FollowUps)
	}
}
