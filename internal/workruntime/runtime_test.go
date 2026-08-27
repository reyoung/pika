package workruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type fakeRuntime struct {
	snapshot   workruntime.Snapshot
	starts     []workruntime.StartSpec
	closedPane []string
	prompts    []struct{ pane, message string }
	promptErr  error
}

func (r *fakeRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) { return r.snapshot, nil }
func (r *fakeRuntime) Start(_ context.Context, spec workruntime.StartSpec) (workruntime.Observation, error) {
	r.starts = append(r.starts, spec)
	observation := workruntime.Observation{AgentName: spec.AgentName, AgentKind: spec.AgentKind, WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2", TerminalID: "term-1", Status: "idle"}
	r.snapshot.Sessions = append(r.snapshot.Sessions, observation)
	return observation, nil
}
func (r *fakeRuntime) Prompt(_ context.Context, pane, message string) error {
	r.prompts = append(r.prompts, struct{ pane, message string }{pane, message})
	return r.promptErr
}
func (r *fakeRuntime) Close(_ context.Context, paneID string) error {
	r.closedPane = append(r.closedPane, paneID)
	return nil
}

func TestStartEffectReattachesAfterUncertainAcknowledgement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}

	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("uncertain dispatch retry: %v", err)
	}
	if len(runtime.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(runtime.starts))
	}
	session, binding, found, err := engine.CurrentAgentSession(ctx, work.ID)
	if err != nil || !found {
		t.Fatalf("current agent session: found=%v err=%v", found, err)
	}
	if session.ID != effect.ID || session.Status != symphony.AgentSessionRunning || binding.TerminalID != "term-1" {
		t.Fatalf("session=%+v binding=%+v", session, binding)
	}
}

func TestStartPersistsProbedProviderVersionAndCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	registry, err := provider.NewRegistry(provider.NewCodexAdapter())
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := registry.Probe(ctx, "codex", provider.ProbeRequest{Executable: "/bin/echo"})
	if err != nil {
		t.Fatal(err)
	}
	sink := workruntime.Sink{Store: engine, Runtime: &fakeRuntime{}, AgentKind: "codex", Providers: registry, RequireProviderCapabilities: true}
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatal(err)
	}
	history, err := engine.AgentSessionHistory(ctx, work.ID)
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	if history[0].ProviderVersion != capabilities.Version || !strings.Contains(string(history[0].ProviderCapabilities), `"fresh_session":true`) {
		t.Fatalf("provider metadata = %+v", history[0])
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.AgentSessions) != 1 || view.AgentSessions[0].ProviderVersion != capabilities.Version {
		t.Fatalf("status Agent Sessions=%+v err=%v", view.AgentSessions, err)
	}
}

func TestReconcileTracksMoveButNeverCompletesWorkFromAgentStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runtime.snapshot.Sessions[0].WorkspaceID = "w2"
	runtime.snapshot.Sessions[0].TabID = "w2:t1"
	runtime.snapshot.Sessions[0].PaneID = "w2:p1"
	for _, status := range []string{"idle", "done", "unknown", "exited"} {
		runtime.snapshot.Sessions[0].Status = status
		if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).Reconcile(ctx); err != nil {
			t.Fatalf("reconcile %s: %v", status, err)
		}
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if view.Works[0].Status != symphony.WorkPending {
		t.Fatalf("work status = %s, want pending", view.Works[0].Status)
	}
	_, binding, found, err := engine.CurrentAgentSession(ctx, work.ID)
	if err != nil || !found || binding.PaneID != "w2:p1" || binding.TerminalID != "term-1" {
		t.Fatalf("moved binding=%+v found=%v err=%v", binding, found, err)
	}
}

func TestReconcileLostPaneEnqueuesOrderedCloseAndFreshSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runtime.snapshot.Sessions = nil
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).Reconcile(ctx); err != nil {
		t.Fatalf("reconcile missing pane: %v", err)
	}
	if len(runtime.closedPane) != 0 {
		t.Fatalf("reconciliation closed panes: %+v", runtime.closedPane)
	}
	if _, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("current session after loss: found=%v err=%v", found, err)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatalf("pending effects: %v", err)
	}
	if len(effects) != 2 || effects[0].Type != "session.close_requested" || effects[1].Type != "work.start_requested" || effects[1].ID == effect.ID {
		t.Fatalf("replacement effects = %+v", effects)
	}
	if err := sink.Dispatch(ctx, effects[0]); err != nil {
		t.Fatalf("close retired Session: %v", err)
	}
	if err := sink.Dispatch(ctx, effects[1]); err != nil {
		t.Fatalf("start fresh Session: %v", err)
	}
	if len(runtime.closedPane) != 1 || runtime.closedPane[0] != "w1:p2" || len(runtime.starts) != 2 {
		t.Fatalf("recovery runtime effects: closed=%+v starts=%+v", runtime.closedPane, runtime.starts)
	}
}

func TestFollowUpDeliveryIsRevalidatedAndTransportFailureIsNotRetried(t *testing.T) {
	for _, test := range []struct {
		name       string
		promptErr  error
		wantStatus string
	}{
		{name: "delivered", wantStatus: "delivered"},
		{name: "unknown", promptErr: errors.New("connection lost after send"), wantStatus: "delivery_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 8, 27, 13, 0, 0, 0, time.UTC)
			engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{Now: func() time.Time { return now }, FollowUpInactivity: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Close() })
			if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
				t.Fatal(err)
			}
			view, _ := engine.Inspect(ctx, symphony.Status{})
			if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`)}); err != nil {
				t.Fatal(err)
			}
			view, _ = engine.Inspect(ctx, symphony.Status{})
			var target symphony.WorkView
			for _, work := range view.Works {
				if work.Role == symphony.RoleBaselineVerification {
					target = work
				}
			}
			targetSession := symphony.AgentSession{ID: "target-session", WorkID: target.ID, Generation: 1, Role: target.Role, AgentKind: "codex", AgentName: "target", Status: symphony.AgentSessionStarting}
			if err := engine.EnsureAgentSession(ctx, targetSession); err != nil {
				t.Fatal(err)
			}
			if err := engine.BindPane(ctx, targetSession.ID, symphony.PaneBinding{WorkspaceID: "w", TabID: "t", PaneID: "target-pane", TerminalID: "target-terminal"}); err != nil {
				t.Fatal(err)
			}
			if err := engine.IngestProviderEvent(ctx, "codex", targetSession.ID, json.RawMessage(`{"session_id":"codex-target","turn_id":"turn","hook_event_name":"Stop"}`)); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
				t.Fatalf("promote: %v %v", promoted, err)
			}
			view, _ = engine.Inspect(ctx, symphony.Status{})
			var generator symphony.WorkView
			for _, work := range view.Works {
				if work.Role == symphony.RoleFollowUp {
					generator = work
				}
			}
			generatorSession := symphony.AgentSession{ID: "generator-session", WorkID: generator.ID, Generation: generator.Generation, Role: generator.Role, AgentKind: "codex", AgentName: "generator", Status: symphony.AgentSessionStarting}
			if err := engine.EnsureAgentSession(ctx, generatorSession); err != nil {
				t.Fatal(err)
			}
			if err := engine.BindPane(ctx, generatorSession.ID, symphony.PaneBinding{WorkspaceID: "w", TabID: "tg", PaneID: "generator-pane", TerminalID: "generator-terminal"}); err != nil {
				t.Fatal(err)
			}
			grant, err := engine.MintAgentGrant(ctx, generatorSession.ID, toolapp.CatalogForRole(generator.Role), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_followup_message", Arguments: json.RawMessage(`{"idempotency_key":"message","message":"继续验证"}`)}); err != nil {
				t.Fatal(err)
			}
			effects, _ := engine.PendingEffects(ctx, 100)
			var delivery symphony.RuntimeEffect
			for _, effect := range effects {
				if effect.Type == "followup.deliver_requested" {
					delivery = effect
				}
			}
			if delivery.ID == "" {
				t.Fatal("delivery effect not found")
			}
			runtime := &fakeRuntime{promptErr: test.promptErr}
			if err := (workruntime.Sink{Store: engine, Runtime: runtime}).Dispatch(ctx, delivery); err != nil {
				t.Fatalf("delivery dispatch: %v", err)
			}
			view, _ = engine.Inspect(ctx, symphony.Status{})
			if view.FollowUps[0].Status != test.wantStatus || len(runtime.prompts) != 1 || runtime.prompts[0].pane != "target-pane" {
				t.Fatalf("status=%+v prompts=%+v", view.FollowUps[0], runtime.prompts)
			}
		})
	}
}

func initializedRuntime(t *testing.T, ctx context.Context) (*symphony.Engine, symphony.WorkView, symphony.RuntimeEffect) {
	t.Helper()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
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
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil || len(effects) != 1 {
		t.Fatalf("pending effects=%+v err=%v", effects, err)
	}
	return engine, view.Works[0], effects[0]
}
