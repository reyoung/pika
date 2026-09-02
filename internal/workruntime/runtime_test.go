package workruntime_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/activation"
	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type fakeRuntime struct {
	snapshot      workruntime.Snapshot
	snapshotCalls int
	snapshotHook  func(int, *workruntime.Snapshot)
	starts        []workruntime.StartSpec
	closedPane    []string
	prompts       []struct{ pane, message string }
	promptErr     error
	keySends      []struct {
		target string
		keys   []string
	}
	keyErr error
}

type unconfirmedCursorRuntime struct {
	*fakeRuntime
	onPrompt func(context.Context, string, string, string) error
}

type pollingPromptStore struct {
	*symphony.Engine
	firstQuery chan struct{}
	once       sync.Once
}

type startAdmissionBarrierStore struct {
	*symphony.Engine
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *startAdmissionBarrierStore) ReserveAgentStart(
	ctx context.Context,
	request symphony.AgentStartRequest,
) (symphony.AgentStartReservation, bool, error) {
	s.once.Do(func() { close(s.reached) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	return s.Engine.ReserveAgentStart(ctx, request)
}

type recordingWorkspacePreparer struct {
	calls int
	fail  bool
}

func (p *recordingWorkspacePreparer) PrepareWork(_ context.Context, work symphony.RuntimeWork) (symphony.RuntimeWork, error) {
	p.calls++
	if p.fail {
		return symphony.RuntimeWork{}, errors.New("injected preparation failure")
	}
	return work, nil
}

type blockingWorkspacePreparer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingWorkspacePreparer) PrepareWork(ctx context.Context, work symphony.RuntimeWork) (symphony.RuntimeWork, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return work, nil
	case <-ctx.Done():
		return symphony.RuntimeWork{}, ctx.Err()
	}
}

type recordingAgentPreparer struct {
	calls        int
	cleanupCalls int
	fail         bool
}

func (p *recordingAgentPreparer) Prepare(
	_ context.Context,
	_ symphony.AgentSession,
	_ symphony.RuntimeWork,
) (workruntime.Preparation, error) {
	p.calls++
	if p.fail {
		return workruntime.Preparation{}, errors.New("injected prelaunch failure")
	}
	return workruntime.Preparation{Cleanup: func() error {
		p.cleanupCalls++
		return nil
	}}, nil
}

type responseLostRuntime struct {
	starts        int
	snapshotCalls int
	visibleAt     int
	observation   workruntime.Observation
}

func (r *responseLostRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	r.snapshotCalls++
	if r.visibleAt > 0 && r.snapshotCalls >= r.visibleAt {
		return workruntime.Snapshot{Sessions: []workruntime.Observation{r.observation}}, nil
	}
	return workruntime.Snapshot{}, nil
}

func (r *responseLostRuntime) Start(ctx context.Context, spec workruntime.StartSpec) (workruntime.Observation, error) {
	if spec.BeforeSubmit != nil {
		if err := spec.BeforeSubmit(ctx); err != nil {
			return workruntime.Observation{}, err
		}
	}
	r.starts++
	if r.observation.AgentName == "" {
		r.observation = workruntime.Observation{
			AgentName: spec.AgentName, AgentKind: spec.AgentKind,
			WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", TerminalID: "terminal", Status: "working",
		}
	}
	return workruntime.Observation{}, errors.New("connection lost after agent.start submission")
}

func (*responseLostRuntime) Prompt(context.Context, string, string) error { return nil }
func (*responseLostRuntime) Close(context.Context, string) error          { return nil }

type blockingSubmittedRuntime struct {
	mu          sync.Mutex
	starts      int
	submitted   chan struct{}
	release     chan struct{}
	observation workruntime.Observation
}

func (r *blockingSubmittedRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observation.AgentName == "" {
		return workruntime.Snapshot{}, nil
	}
	return workruntime.Snapshot{Sessions: []workruntime.Observation{r.observation}}, nil
}

func (r *blockingSubmittedRuntime) Start(ctx context.Context, spec workruntime.StartSpec) (workruntime.Observation, error) {
	if spec.BeforeSubmit != nil {
		if err := spec.BeforeSubmit(ctx); err != nil {
			return workruntime.Observation{}, err
		}
	}
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	close(r.submitted)
	select {
	case <-r.release:
	case <-ctx.Done():
		return workruntime.Observation{}, ctx.Err()
	}
	observation := workruntime.Observation{
		AgentName: spec.AgentName, AgentKind: spec.AgentKind,
		WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", TerminalID: "terminal", Status: "working",
	}
	r.mu.Lock()
	r.observation = observation
	r.mu.Unlock()
	return observation, nil
}

func (*blockingSubmittedRuntime) Prompt(context.Context, string, string) error { return nil }
func (*blockingSubmittedRuntime) Close(context.Context, string) error          { return nil }

type definitelyNotSubmittedRuntime struct{}

func (*definitelyNotSubmittedRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	return workruntime.Snapshot{}, nil
}
func (*definitelyNotSubmittedRuntime) Start(context.Context, workruntime.StartSpec) (workruntime.Observation, error) {
	return workruntime.Observation{}, &workruntime.LaunchError{
		DefinitelyNotSubmitted: true, Err: errors.New("injected failure before launch RPC"),
	}
}
func (*definitelyNotSubmittedRuntime) Prompt(context.Context, string, string) error { return nil }
func (*definitelyNotSubmittedRuntime) Close(context.Context, string) error          { return nil }

type abortErrorStore struct {
	*symphony.Engine
}

func (s *abortErrorStore) ReserveAgentStart(
	ctx context.Context,
	request symphony.AgentStartRequest,
) (symphony.AgentStartReservation, bool, error) {
	reservation, eligible, err := s.Engine.ReserveAgentStart(ctx, request)
	if err != nil || !eligible {
		return reservation, eligible, err
	}
	return abortErrorReservation{AgentStartReservation: reservation}, true, nil
}

type abortErrorReservation struct {
	symphony.AgentStartReservation
}

func (r abortErrorReservation) Abort() error {
	return errors.Join(r.AgentStartReservation.Abort(), errors.New("injected reservation abort failure"))
}

type failingGrantStore struct {
	*symphony.Engine
	fail bool
}

func (s *failingGrantStore) MintAgentGrant(
	ctx context.Context,
	sessionID string,
	catalog []string,
	ttl time.Duration,
) (symphony.AgentGrant, error) {
	if s.fail {
		return symphony.AgentGrant{}, errors.New("injected grant failure after Context materialization")
	}
	return s.Engine.MintAgentGrant(ctx, sessionID, catalog, ttl)
}

func (s *pollingPromptStore) PromptSubmissionObservedAfter(ctx context.Context, sessionID string, after int64, message string) (bool, error) {
	observed, err := s.Engine.PromptSubmissionObservedAfter(ctx, sessionID, after, message)
	s.once.Do(func() { close(s.firstQuery) })
	return observed, err
}

func (r *unconfirmedCursorRuntime) PromptForProvider(ctx context.Context, target, message, providerKind string) error {
	return r.onPrompt(ctx, target, message, providerKind)
}

func (r *fakeRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	r.snapshotCalls++
	if r.snapshotHook != nil {
		r.snapshotHook(r.snapshotCalls, &r.snapshot)
	}
	return r.snapshot, nil
}
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
func (r *fakeRuntime) SendAgentKeys(_ context.Context, target string, keys []string) error {
	r.keySends = append(r.keySends, struct {
		target string
		keys   []string
	}{target: target, keys: append([]string(nil), keys...)})
	return r.keyErr
}

func TestRetireMaintenanceTargetsIsExactValidatedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	engine, _, startEffect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex", Providers: provider.DefaultRegistry()}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	if err := dispatcher.DispatchEffect(ctx, startEffect.ID); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.Works) != 1 {
		t.Fatalf("inspect initial Work: view=%+v err=%v", view, err)
	}
	work := view.Works[0]
	session, _, found, err := engine.CurrentAgentSession(ctx, work.ID)
	if err != nil || !found {
		t.Fatalf("current frozen Session: found=%v err=%v", found, err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "complete-frozen-work"}, WorkID: work.ID, Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	target := maintenance.Target{SessionID: session.ID, WorkID: work.ID, WorkGeneration: work.Generation}

	wrong := target
	wrong.WorkGeneration++
	if err := sink.RetireMaintenanceTargets(ctx, []maintenance.Target{wrong}); err == nil {
		t.Fatal("generation-drift target was retired")
	}
	if len(runtime.closedPane) != 0 {
		t.Fatalf("generation-drift target closed panes: %+v", runtime.closedPane)
	}
	missing := target
	missing.SessionID = "missing-frozen-session"
	if err := sink.RetireMaintenanceTargets(ctx, []maintenance.Target{target, missing}); err == nil {
		t.Fatal("partially valid target list was retired")
	}
	if len(runtime.closedPane) != 0 {
		t.Fatalf("target list was not validated before retirement: %+v", runtime.closedPane)
	}

	if err := sink.RetireMaintenanceTargets(ctx, []maintenance.Target{target}); err != nil {
		t.Fatal(err)
	}
	if err := sink.RetireMaintenanceTargets(ctx, []maintenance.Target{target}); err != nil {
		t.Fatalf("idempotent retirement replay: %v", err)
	}
	if len(runtime.closedPane) != 1 {
		t.Fatalf("frozen pane close count=%d, want 1", len(runtime.closedPane))
	}
	if err := dispatcher.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runtime.starts) != 2 {
		t.Fatalf("Agent starts=%d, want frozen plus one successor", len(runtime.starts))
	}
	if err := sink.RetireMaintenanceTargets(ctx, []maintenance.Target{target}); err != nil {
		t.Fatalf("replay after successor dispatch: %v", err)
	}
	if len(runtime.closedPane) != 1 || len(runtime.starts) != 2 {
		t.Fatalf("replay touched successor: closes=%v starts=%d", runtime.closedPane, len(runtime.starts))
	}
}

func TestSchedulerPauseInterruptsAndResumeContinuesSameSession(t *testing.T) {
	ctx := context.Background()
	engine, _, startEffect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex", Providers: provider.DefaultRegistry()}
	if err := sink.Dispatch(ctx, startEffect); err != nil {
		t.Fatal(err)
	}
	runtime.snapshot.Sessions[0].Status = "working"
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	pause, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchEffect(ctx, schedulerEffectID(t, pause)); err != nil {
		t.Fatal(err)
	}
	paused, _ := engine.Inspect(ctx, symphony.Status{})
	if len(runtime.keySends) != 1 || runtime.keySends[0].target == "" || len(runtime.keySends[0].keys) != 1 || runtime.keySends[0].keys[0] != "ctrl+c" {
		t.Fatalf("interrupt deliveries=%+v", runtime.keySends)
	}
	if paused.Scheduler.Latest == nil || paused.Scheduler.Latest.Status != "complete" || paused.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionSent {
		t.Fatalf("pause cycle=%+v", paused.Scheduler.Latest)
	}
	sessionID := paused.Scheduler.Latest.Actions[0].AgentSessionID

	resumeSnapshots := runtime.snapshotCalls
	runtime.snapshotHook = func(call int, snapshot *workruntime.Snapshot) {
		// The first resume snapshot still sees the interrupted turn as working;
		// the next observation sees the settled prompt.
		if call > resumeSnapshots+1 {
			snapshot.Sessions[0].Status = "idle"
		}
	}
	resume, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchEffect(ctx, schedulerEffectID(t, resume)); err != nil {
		t.Fatal(err)
	}
	resumed, _ := engine.Inspect(ctx, symphony.Status{})
	if len(runtime.prompts) != 1 || runtime.prompts[0].message != "继续" {
		t.Fatalf("resume prompts=%+v", runtime.prompts)
	}
	if resumed.Scheduler.Latest == nil || resumed.Scheduler.Latest.Actions[0].AgentSessionID != sessionID || resumed.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionSent {
		t.Fatalf("resume cycle=%+v", resumed.Scheduler.Latest)
	}
	active, err := engine.ActiveAgentSessions(ctx)
	if err != nil || len(active) != 1 || active[0].Session.ID != sessionID {
		t.Fatalf("active Session after resume=%+v err=%v", active, err)
	}
}

func TestCursorResumeAcceptsOnlyNewMatchingDurableSubmitHookWhenLifecycleTransitionIsMissed(t *testing.T) {
	ctx := context.Background()
	engine, _, startEffect := initializedRuntime(t, ctx)
	store := &pollingPromptStore{Engine: engine, firstQuery: make(chan struct{})}
	baseRuntime := &fakeRuntime{}
	runtime := &unconfirmedCursorRuntime{fakeRuntime: baseRuntime}
	sink := workruntime.Sink{Store: store, Runtime: runtime, AgentKind: "cursor", Providers: provider.DefaultRegistry(), PromptEvidenceTimeout: time.Second}
	if err := sink.Dispatch(ctx, startEffect); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.AgentSessions) != 1 {
		t.Fatalf("started Cursor Session=%+v err=%v", view.AgentSessions, err)
	}
	sessionID := view.AgentSessions[0].ID
	baseRuntime.snapshot.Sessions[0].Status = "working"
	pause, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause-cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (outbox.Dispatcher{Store: engine, Sink: sink}).DispatchEffect(ctx, schedulerEffectID(t, pause)); err != nil {
		t.Fatal(err)
	}
	baseRuntime.snapshot.Sessions[0].Status = "idle"
	// A stale submit hook for a different prompt must not be enough. The
	// transport deliberately reports that Herdr missed the lifecycle transition;
	// the matching durable submission arrives only after the first false query.
	if err := engine.IngestProviderEvent(ctx, "cursor", sessionID, json.RawMessage(`{"conversation_id":"cursor-session","generation_id":"stale","hook_event_name":"beforeSubmitPrompt","prompt":"old"}`)); err != nil {
		t.Fatal(err)
	}
	const transportFailure = "Herdr lifecycle transition was not observed"
	promptCalls := make(chan struct{ message, kind string }, 1)
	runtime.onPrompt = func(_ context.Context, _, message, kind string) error {
		promptCalls <- struct{ message, kind string }{message: message, kind: kind}
		return errors.New(transportFailure)
	}
	ingested := make(chan error, 1)
	go func() {
		<-store.firstQuery
		ingested <- engine.IngestProviderEvent(context.Background(), "cursor", sessionID, json.RawMessage(`{"conversation_id":"cursor-session","generation_id":"resume","hook_event_name":"beforeSubmitPrompt","prompt":"继续"}`))
	}()
	resume, err := engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume-cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (outbox.Dispatcher{Store: engine, Sink: sink}).DispatchEffect(ctx, schedulerEffectID(t, resume)); err != nil {
		t.Fatal(err)
	}
	if call := <-promptCalls; call.kind != "cursor" || call.message != "继续" {
		t.Fatalf("Cursor resume prompt kind=%q message=%q", call.kind, call.message)
	}
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.Scheduler.Latest == nil || len(view.Scheduler.Latest.Actions) != 1 ||
		(view.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionSent && view.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionObserved) {
		t.Fatalf("Cursor durable prompt confirmation cycle=%+v err=%v", view.Scheduler.Latest, err)
	}

	// With no event after the new high-watermark, the same transport error must
	// remain a delivery failure; neither the stale nor prior matching hook can
	// confirm this second resume.
	baseRuntime.snapshot.Sessions[0].Status = "working"
	pause, err = engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause-cursor-again"}})
	if err != nil {
		t.Fatal(err)
	}
	negativeSink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "cursor", Providers: provider.DefaultRegistry(), PromptEvidenceTimeout: time.Millisecond}
	if err := (outbox.Dispatcher{Store: engine, Sink: negativeSink}).DispatchEffect(ctx, schedulerEffectID(t, pause)); err != nil {
		t.Fatal(err)
	}
	baseRuntime.snapshot.Sessions[0].Status = "idle"
	resume, err = engine.Apply(ctx, symphony.ResumeScheduler{Meta: symphony.CommandMeta{RequestID: "resume-cursor-again"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (outbox.Dispatcher{Store: engine, Sink: negativeSink}).DispatchEffect(ctx, schedulerEffectID(t, resume)); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.Scheduler.Latest == nil || len(view.Scheduler.Latest.Actions) != 1 || view.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionFailed || !strings.Contains(view.Scheduler.Latest.Actions[0].Error, transportFailure) {
		t.Fatalf("stale Cursor prompt evidence cycle=%+v err=%v", view.Scheduler.Latest, err)
	}
}

func TestSchedulerUnknownRuntimeStatusIsReportedAsPartialWithoutUnsafeKeys(t *testing.T) {
	ctx := context.Background()
	engine, _, startEffect := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex", Providers: provider.DefaultRegistry()}
	if err := sink.Dispatch(ctx, startEffect); err != nil {
		t.Fatal(err)
	}
	runtime.snapshot.Sessions[0].Status = "unknown"
	pause, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (outbox.Dispatcher{Store: engine, Sink: sink}).DispatchEffect(ctx, schedulerEffectID(t, pause)); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if len(runtime.keySends) != 0 || view.Scheduler.Latest == nil || view.Scheduler.Latest.Status != "partial" ||
		view.Scheduler.Latest.Actions[0].Status != symphony.SchedulerActionFailed {
		t.Fatalf("keys=%+v cycle=%+v", runtime.keySends, view.Scheduler.Latest)
	}
}

func schedulerEffectID(t *testing.T, receipt symphony.Receipt) string {
	t.Helper()
	var result struct {
		EffectID string `json:"effect_id"`
	}
	if err := json.Unmarshal(receipt.Result, &result); err != nil || result.EffectID == "" {
		t.Fatalf("Scheduler receipt=%s err=%v", receipt.Result, err)
	}
	return result.EffectID
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
	if runtime.starts[0].PaneLabel != "Baseline" || runtime.starts[0].PaneLabelNeedsID || runtime.starts[0].DedicatedTab {
		t.Fatalf("baseline pane naming = %+v", runtime.starts[0])
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

func TestReconcileSettledIdleProviderTurnArmsFollowUpAfterProviderSilence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now: func() time.Time { return now }, FollowUpInactivity: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "submit-baseline"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition(),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	var target symphony.WorkView
	for _, work := range view.Works {
		if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
			target = work
			break
		}
	}
	if target.ID == "" {
		t.Fatal("baseline verification target was not created")
	}
	session := symphony.AgentSession{
		ID: "target-session", WorkID: target.ID, Generation: target.Generation, Role: target.Role,
		AgentKind: "codex", AgentName: "target-agent", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{
		WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.IngestProviderEvent(ctx, "codex", session.ID, json.RawMessage(
		`{"session_id":"codex-target","turn_id":"turn-1","hook_event_name":"UserPromptSubmit","prompt":"continue"}`,
	)); err != nil {
		t.Fatal(err)
	}
	journal, err := engine.ConversationJournal(ctx, target.ID)
	if err != nil || len(journal.Turns) != 1 || journal.Turns[0].Status != "running" {
		t.Fatalf("provider turn = %+v err=%v", journal.Turns, err)
	}

	now = now.Add(2 * time.Minute)
	if err := (workruntime.Reconciler{Store: engine}).ReconcileSnapshot(ctx, workruntime.Snapshot{Sessions: []workruntime.Observation{{
		AgentName: "target-agent", AgentKind: "codex", WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal", Status: "done",
	}}}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.FollowUps) != 1 || view.FollowUps[0].Status != "waiting" {
		t.Fatalf("idle provider turn did not arm Follow-up: %+v", view.FollowUps)
	}
	request := view.FollowUps[0]
	if request.ActivitySource != "provider_activity_timeout" || request.DueAt == "" {
		t.Fatalf("idle timeout provenance = %+v", request)
	}

	now = now.Add(30 * time.Second)
	if err := (workruntime.Reconciler{Store: engine}).ReconcileSnapshot(ctx, workruntime.Snapshot{Sessions: []workruntime.Observation{{
		AgentName: "target-agent", AgentKind: "codex", WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal", Status: "done",
	}}}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.FollowUps) != 1 || view.FollowUps[0].Status != "waiting" || view.FollowUps[0].DueAt != request.DueAt {
		t.Fatalf("idle pane observation reset Follow-up deadline: %+v", view.FollowUps)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote timed-out Follow-up: promoted=%v err=%v", promoted, err)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || promoted {
		t.Fatalf("promoted duplicate Follow-up: promoted=%v err=%v", promoted, err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	request = view.FollowUps[0]
	var generator symphony.WorkView
	for _, work := range view.Works {
		if work.ID == request.GeneratorWorkID {
			generator = work
			break
		}
	}
	if generator.ID == "" {
		t.Fatalf("Follow-up generator was not created: %+v", request)
	}
	generatorSession := symphony.AgentSession{
		ID: "follow-up-generator", WorkID: generator.ID, Generation: generator.Generation, Role: generator.Role,
		AgentKind: "codex", AgentName: "follow-up-generator", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, generatorSession); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, generatorSession.ID, symphony.PaneBinding{
		WorkspaceID: "workspace", TabID: "generator-tab", PaneID: "generator-pane", TerminalID: "generator-terminal",
	}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, generatorSession.ID, toolapp.CatalogForRole(generator.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{
		Name:      "submit_followup_message",
		Arguments: json.RawMessage(`{"idempotency_key":"idle-timeout-message","message":"continue the incomplete work"}`),
	})
	if err != nil || !result.Terminal {
		t.Fatalf("submit Follow-up: result=%+v err=%v", result, err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.FollowUps) != 1 || view.FollowUps[0].Status != "ready" {
		t.Fatalf("ready Follow-up = %+v err=%v", view.FollowUps, err)
	}
	var delivery symphony.RuntimeEffect
	effects, err := engine.PendingEffects(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range effects {
		if effect.Type == "followup.deliver_requested" {
			delivery = effect
			break
		}
	}
	if delivery.ID == "" {
		t.Fatal("Follow-up delivery effect was not created")
	}
	runtime := &fakeRuntime{}
	if err := (workruntime.Sink{Store: engine, Runtime: runtime}).Dispatch(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.FollowUps[0].Status != "delivered" || len(runtime.prompts) != 1 {
		t.Fatalf("delivered Follow-up = %+v prompts=%+v err=%v", view.FollowUps, runtime.prompts, err)
	}
	expectedRetryDueAt := now.Add(time.Minute).Format(time.RFC3339Nano)

	// The first delivery may emit pane updates before the target's next
	// UserPromptSubmit arrives. Re-observing the same settled Agent must not
	// arm a second request for the already-delivered provider Turn.
	now = now.Add(30 * time.Second)
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).ReconcileSnapshot(ctx, workruntime.Snapshot{Sessions: []workruntime.Observation{{
		AgentName: "target-agent", AgentKind: "codex", WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal", Status: "done",
	}}}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.FollowUps) != 1 || view.FollowUps[0].Status != "delivered" {
		t.Fatalf("duplicate settled-idle arm after delivery: followups=%+v err=%v", view.FollowUps, err)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || promoted {
		t.Fatalf("started duplicate Follow-up generator before delivery quiet period: promoted=%v err=%v", promoted, err)
	}
	generatorCount := 0
	for _, work := range view.Works {
		if work.Role == symphony.RoleFollowUp {
			generatorCount++
		}
	}
	if generatorCount != 1 || len(runtime.prompts) != 1 {
		t.Fatalf("duplicate Follow-up generator after delivery: generators=%d prompts=%d", generatorCount, len(runtime.prompts))
	}

	// A genuinely unresponsive target remains eligible after the delivery-based
	// quiet period; the delivery timestamp delays, but does not disable, retry.
	now = now.Add(5 * time.Minute)
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).ReconcileSnapshot(ctx, workruntime.Snapshot{Sessions: []workruntime.Observation{{
		AgentName: "target-agent", AgentKind: "codex", WorkspaceID: "workspace", TabID: "tab", PaneID: "target-pane", TerminalID: "target-terminal", Status: "done",
	}}}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil || len(view.FollowUps) != 2 || view.FollowUps[1].Status != "waiting" {
		t.Fatalf("settled-idle retry was disabled: followups=%+v err=%v", view.FollowUps, err)
	}
	if view.FollowUps[1].DueAt != expectedRetryDueAt {
		t.Fatalf("retry due_at did not use latest delivery anchor: got=%s want=%s", view.FollowUps[1].DueAt, expectedRetryDueAt)
	}
	if promoted, err := engine.PromoteDueFollowUps(ctx); err != nil || !promoted {
		t.Fatalf("promote settled-idle retry: promoted=%v err=%v", promoted, err)
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

func TestLostSessionReplacementStartsOneFreshAgentForActiveWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, initialStart := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	if err := dispatcher.DispatchEffect(ctx, initialStart.ID); err != nil {
		t.Fatal(err)
	}
	runtime.snapshot.Sessions = nil
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var replacement symphony.RuntimeEffect
	for _, effect := range effects {
		if effect.Type == "work.start_requested" {
			replacement = effect
		}
	}
	if replacement.ID == "" ||
		!strings.Contains(string(replacement.Payload), `"reason":"lost_session_replacement"`) ||
		!strings.Contains(string(replacement.Payload), `"predecessor_session_id":"`+initialStart.ID+`"`) {
		t.Fatalf("lost-session replacement effect=%+v", replacement)
	}
	if err := dispatcher.DispatchEffect(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	if len(runtime.starts) != 2 {
		t.Fatalf("Agent starts=%d, want initial plus exactly one replacement", len(runtime.starts))
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || !found || session.ID != replacement.ID {
		t.Fatalf("fresh replacement Session=%+v found=%v err=%v", session, found, err)
	}
}

func TestLostSessionReplacementRejectsUnrelatedHistoricalLostSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, initialStart := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	if err := sink.Dispatch(ctx, initialStart); err != nil {
		t.Fatal(err)
	}
	runtime.snapshot.Sessions = nil
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var replacement symphony.RuntimeEffect
	for _, effect := range effects {
		if effect.Type == "work.start_requested" {
			replacement = effect
		}
	}
	unrelated := symphony.AgentSession{
		ID: "unrelated-lost", WorkID: work.ID, Generation: work.Generation, Role: work.Role,
		AgentKind: "codex", AgentName: "pika-unrelated-lost", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, unrelated); err != nil {
		t.Fatal(err)
	}
	if err := engine.MarkAgentSessionEnded(ctx, unrelated.ID, symphony.AgentSessionLost); err != nil {
		t.Fatal(err)
	}
	replacement.Payload = []byte(`{"work_id":"` + work.ID + `","reason":"lost_session_replacement","predecessor_session_id":"` + unrelated.ID + `"}`)
	if err := sink.Dispatch(ctx, replacement); err == nil {
		t.Fatal("replacement accepted unrelated historical lost Session")
	}
	if len(runtime.starts) != 1 {
		t.Fatalf("unrelated predecessor launched replacement: %+v", runtime.starts)
	}
}

func TestLostSessionReplacementRequiresExactDurableLostSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, _ := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	effect := symphony.RuntimeEffect{
		ID: "forged-replacement", Type: "work.start_requested",
		Payload: []byte(`{"work_id":"` + work.ID + `","reason":"lost_session_replacement"}`),
	}
	if err := (workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}).Dispatch(ctx, effect); err == nil {
		t.Fatal("replacement without predecessor_session_id was accepted")
	}
	if len(runtime.starts) != 0 {
		t.Fatalf("replacement without durable lost Session started Agents: %+v", runtime.starts)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("replacement without durable lost Session persisted Session=%+v found=%v err=%v", session, found, err)
	}
}

func TestCancelBeforeStartAdmissionPerformsNoWorkspaceOrRuntimePreparation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	store := &startAdmissionBarrierStore{
		Engine: engine, reached: make(chan struct{}), release: make(chan struct{}),
	}
	workspace := &recordingWorkspacePreparer{}
	runtime := &fakeRuntime{}
	done := make(chan error, 1)
	go func() {
		done <- (workruntime.Sink{
			Store: store, Runtime: runtime, AgentKind: "codex", WorkspacePreparer: workspace,
		}).Dispatch(ctx, effect)
	}()
	<-store.reached
	if _, err := engine.Apply(ctx, symphony.CancelWork{
		Meta: symphony.CommandMeta{RequestID: "cancel-before-start-admission"}, WorkID: work.ID,
	}); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if workspace.calls != 0 {
		t.Fatalf("stale start prepared Workspace %d times", workspace.calls)
	}
	if len(runtime.starts) != 0 {
		t.Fatalf("stale start launched Agents: %+v", runtime.starts)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("stale start persisted Session=%+v found=%v err=%v", session, found, err)
	}
}

func TestPauseRemainsResponsiveAndObservesStagedConcurrentLaunch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _, effect := initializedRuntime(t, ctx)
	workspace := &blockingWorkspacePreparer{entered: make(chan struct{}), release: make(chan struct{})}
	runtime := &fakeRuntime{}
	startDone := make(chan error, 1)
	go func() {
		startDone <- (workruntime.Sink{
			Store: engine, Runtime: runtime, AgentKind: "codex", WorkspacePreparer: workspace,
		}).Dispatch(ctx, effect)
	}()
	<-workspace.entered
	pauseDone := make(chan error, 1)
	go func() {
		_, err := engine.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause-during-start"}})
		pauseDone <- err
	}()
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Scheduler pause blocked behind unrelated launch preparation")
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.Scheduler.Latest == nil || len(view.Scheduler.Latest.Actions) != 1 ||
		view.Scheduler.Latest.Actions[0].AgentSessionID != effect.ID {
		t.Fatalf("pause missed staged launch: scheduler=%+v err=%v", view.Scheduler, err)
	}
	close(workspace.release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
}

func TestSameWorkCancelLinearizesAfterReservedLaunch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	workspace := &blockingWorkspacePreparer{entered: make(chan struct{}), release: make(chan struct{})}
	runtime := &fakeRuntime{}
	startDone := make(chan error, 1)
	go func() {
		startDone <- (workruntime.Sink{
			Store: engine, Runtime: runtime, AgentKind: "codex", WorkspacePreparer: workspace,
		}).Dispatch(ctx, effect)
	}()
	<-workspace.entered
	cancelDone := make(chan error, 1)
	go func() {
		_, err := engine.Apply(ctx, symphony.CancelWork{
			Meta: symphony.CommandMeta{RequestID: "cancel-during-reserved-start"}, WorkID: work.ID,
		})
		cancelDone <- err
	}()
	select {
	case err := <-cancelDone:
		t.Fatalf("same-Work cancellation bypassed launch reservation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(workspace.release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	if len(runtime.starts) != 1 {
		t.Fatalf("reserved launch starts=%d", len(runtime.starts))
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil || view.Works[0].Status != symphony.WorkCancelled {
		t.Fatalf("same-Work cancellation did not follow launch: works=%+v err=%v", view.Works, err)
	}
}

func TestPreparationFailureAbortsStartReservationAndAllowsRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	workspace := &recordingWorkspacePreparer{fail: true}
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{
		Store: engine, Runtime: runtime, AgentKind: "codex", WorkspacePreparer: workspace,
	}
	if err := sink.Dispatch(ctx, effect); err == nil || !strings.Contains(err.Error(), "injected preparation failure") {
		t.Fatalf("preparation failure=%v", err)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("failed preparation retained Session=%+v found=%v err=%v", session, found, err)
	}
	workspace.fail = false
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("retry after reservation abort: %v", err)
	}
	if workspace.calls != 2 || len(runtime.starts) != 1 {
		t.Fatalf("retry preparation calls=%d starts=%d", workspace.calls, len(runtime.starts))
	}
}

func TestPrelaunchFailureRemovesStagedSessionAndAllowsRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	preparer := &recordingAgentPreparer{fail: true}
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{
		Store: engine, Runtime: runtime, AgentKind: "codex", Preparer: preparer,
	}
	if err := sink.Dispatch(ctx, effect); err == nil || !strings.Contains(err.Error(), "injected prelaunch failure") {
		t.Fatalf("prelaunch failure=%v", err)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || found {
		t.Fatalf("failed prelaunch retained Session=%+v found=%v err=%v", session, found, err)
	}
	preparer.fail = false
	if err := sink.Dispatch(ctx, effect); err != nil {
		t.Fatalf("retry after staged reservation abort: %v", err)
	}
	if preparer.calls != 2 || len(runtime.starts) != 1 {
		t.Fatalf("retry prelaunch calls=%d starts=%d", preparer.calls, len(runtime.starts))
	}
}

func TestDefinitelyNotSubmittedLaunchAbortsSessionAndProviderPreparation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	preparer := &recordingAgentPreparer{}
	err := (workruntime.Sink{
		Store: engine, Runtime: &definitelyNotSubmittedRuntime{}, AgentKind: "codex", Preparer: preparer,
	}).Dispatch(ctx, effect)
	if err == nil || !workruntime.LaunchDefinitelyNotSubmitted(err) {
		t.Fatalf("deterministic launch failure=%v", err)
	}
	if preparer.cleanupCalls != 1 {
		t.Fatalf("provider cleanup calls=%d", preparer.cleanupCalls)
	}
	if session, _, found, readErr := engine.CurrentAgentSession(ctx, work.ID); readErr != nil || found {
		t.Fatalf("definite launch failure retained Session=%+v found=%v err=%v", session, found, readErr)
	}
}

func TestStartSurfacesReservationAbortFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _, effect := initializedRuntime(t, ctx)
	err := (workruntime.Sink{
		Store: &abortErrorStore{Engine: engine}, Runtime: &fakeRuntime{}, AgentKind: "codex",
		WorkspacePreparer: &recordingWorkspacePreparer{fail: true},
	}).Dispatch(ctx, effect)
	if err == nil || !strings.Contains(err.Error(), "injected preparation failure") ||
		!strings.Contains(err.Error(), "injected reservation abort failure") {
		t.Fatalf("joined preparation/abort error=%v", err)
	}
}

func TestRealActivationFailureRemovesSessionOwnedRowsAndFilesThenRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	effects, err := engine.PendingEffects(ctx, 1)
	if err != nil || len(effects) != 1 {
		t.Fatalf("initial start effect=%+v err=%v", effects, err)
	}
	instructionRoot := filepath.Join(root, "instructions")
	if err := instructions.Install(instructionRoot); err != nil {
		t.Fatal(err)
	}
	contextRoot := filepath.Join(root, "contexts")
	activationStore := &failingGrantStore{Engine: engine, fail: true}
	sink := workruntime.Sink{
		Store: engine, Runtime: &fakeRuntime{}, AgentKind: "codex",
		Preparer: activation.Preparer{
			Store: activationStore, InstructionRoot: instructionRoot, ContextsRoot: contextRoot, SocketPath: "/tmp/pika.sock",
		},
	}
	if err := sink.Dispatch(ctx, effects[0]); err == nil || !strings.Contains(err.Error(), "injected grant failure") {
		t.Fatalf("real activation failure=%v", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var ownedRows int
	if err := database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM context_snapshots WHERE agent_session_id = ?) +
		(SELECT COUNT(*) FROM instruction_snapshots WHERE agent_session_id = ?) +
		(SELECT COUNT(*) FROM session_grants WHERE agent_session_id = ?) +
		(SELECT COUNT(*) FROM agent_sessions WHERE id = ?)`,
		effects[0].ID, effects[0].ID, effects[0].ID, effects[0].ID).Scan(&ownedRows); err != nil {
		t.Fatal(err)
	}
	if ownedRows != 0 {
		t.Fatalf("failed activation retained %d Session-owned rows", ownedRows)
	}
	if _, err := os.Stat(filepath.Join(contextRoot, effects[0].ID)); !os.IsNotExist(err) {
		t.Fatalf("failed activation retained Context files: %v", err)
	}
	activationStore.fail = false
	if err := sink.Dispatch(ctx, effects[0]); err != nil {
		t.Fatalf("retry real activation after cleanup: %v", err)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, view.Works[0].ID); err != nil || !found || session.ID != effects[0].ID {
		t.Fatalf("retried activation Session=%+v found=%v err=%v", session, found, err)
	}
}

func TestUnknownStartReasonFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, _ := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	err := (workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}).Dispatch(ctx, symphony.RuntimeEffect{
		ID: "unknown-reason", Type: "work.start_requested",
		Payload: []byte(`{"work_id":"` + work.ID + `","reason":"future_unreviewed_reason"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported reason") {
		t.Fatalf("unknown reason error=%v", err)
	}
	if len(runtime.starts) != 0 {
		t.Fatalf("unknown reason launched Agents: %+v", runtime.starts)
	}
}

func TestSubmittedLaunchResponseLossNeverBlindlyRestartsSameEffect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	runtime := &responseLostRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	if err := sink.Dispatch(ctx, effect); err == nil {
		t.Fatal("response loss was not surfaced")
	}
	if err := sink.Dispatch(ctx, effect); err == nil || !strings.Contains(err.Error(), "awaiting runtime reconciliation") {
		t.Fatalf("uncertain retry error=%v", err)
	}
	if runtime.starts != 1 {
		t.Fatalf("same uncertain effect invoked Runtime.Start %d times", runtime.starts)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, work.ID); err != nil || !found || session.ID != effect.ID {
		t.Fatalf("uncertain Session=%+v found=%v err=%v", session, found, err)
	}
}

func TestDispatcherAndReconcilerReplaceConfirmedMissingSubmittedLaunchOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _, original := initializedRuntime(t, ctx)
	runtime := &responseLostRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	if err := dispatcher.DispatchPending(ctx); err == nil {
		t.Fatal("response loss was not surfaced by Dispatcher")
	}
	if engine.AgentStartReservationInFlight(original.ID) {
		t.Fatal("response-lost launch retained in-process reservation ownership")
	}
	if err := (workruntime.Reconciler{
		Store: engine, Runtime: runtime, LossConfirmation: time.Millisecond,
	}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.starts != 1 {
		t.Fatalf("uncertain original effect invoked Start %d times", runtime.starts)
	}
	effects, err := engine.PendingEffects(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 2 || effects[0].Type != "session.close_requested" || effects[1].Type != "work.start_requested" {
		t.Fatalf("confirmed launch loss effects=%+v", effects)
	}
	payload, intent, err := symphony.DecodeAgentStartPayload(effects[1].Payload)
	if err != nil || intent != symphony.AgentStartLostReplacement ||
		payload.PredecessorSessionID != original.ID {
		t.Fatalf("replacement payload=%+v intent=%q err=%v", payload, intent, err)
	}
}

func TestReconcilerNeverLosesSubmittedLaunchWhileReservationIsInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, effect := initializedRuntime(t, ctx)
	runtime := &blockingSubmittedRuntime{submitted: make(chan struct{}), release: make(chan struct{})}
	startDone := make(chan error, 1)
	go func() {
		startDone <- (outbox.Dispatcher{
			Store: engine, Sink: workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"},
		}).DispatchPending(ctx)
	}()
	<-runtime.submitted
	lossConfirmation := 5 * time.Millisecond
	deadline := time.Now().Add(5 * lossConfirmation)
	for time.Now().Before(deadline) {
		if err := (workruntime.Reconciler{
			Store: engine, Runtime: runtime, LossConfirmation: lossConfirmation,
		}).Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if effects, err := engine.PendingEffects(ctx, 10); err != nil || len(effects) != 0 {
		t.Fatalf("in-flight launch created loss effects=%+v err=%v", effects, err)
	}
	close(runtime.release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	starts := runtime.starts
	observation := runtime.observation
	runtime.mu.Unlock()
	if starts != 1 {
		t.Fatalf("in-flight launch Start calls=%d", starts)
	}
	session, binding, found, err := engine.CurrentAgentSession(ctx, work.ID)
	if err != nil || !found || session.ID != effect.ID || binding.PaneID != observation.PaneID {
		t.Fatalf("original Session=%+v binding=%+v found=%v err=%v", session, binding, found, err)
	}
}

func TestReconcilerBindsLateVisibleSubmittedLaunchWithoutReplacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, work, _ := initializedRuntime(t, ctx)
	runtime := &responseLostRuntime{visibleAt: 3}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	if err := (outbox.Dispatcher{Store: engine, Sink: sink}).DispatchPending(ctx); err == nil {
		t.Fatal("response loss was not surfaced by Dispatcher")
	}
	if err := (workruntime.Reconciler{
		Store: engine, Runtime: runtime, LossConfirmation: time.Millisecond,
	}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.starts != 1 {
		t.Fatalf("late-visible original effect invoked Start %d times", runtime.starts)
	}
	session, binding, found, err := engine.CurrentAgentSession(ctx, work.ID)
	if err != nil || !found || session.ID == "" || binding.PaneID != runtime.observation.PaneID {
		t.Fatalf("late-visible Session=%+v binding=%+v found=%v err=%v", session, binding, found, err)
	}
	if effects, err := engine.PendingEffects(ctx, 10); err != nil || len(effects) != 0 {
		t.Fatalf("late-visible launch created replacement effects=%+v err=%v", effects, err)
	}
}

func TestLostSessionReplacementQueuedBeforeCancelDoesNotStartCancelledWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, cancelledWork, initialStart := initializedRuntime(t, ctx)
	runtime := &fakeRuntime{}
	sink := workruntime.Sink{Store: engine, Runtime: runtime, AgentKind: "codex"}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: sink}
	if err := dispatcher.DispatchEffect(ctx, initialStart.ID); err != nil {
		t.Fatal(err)
	}

	runtime.snapshot.Sessions = nil
	if err := (workruntime.Reconciler{Store: engine, Runtime: runtime}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.CancelWork{
		Meta: symphony.CommandMeta{RequestID: "cancel-lost-work"}, WorkID: cancelledWork.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.StartBaselineDraft{
		Meta: symphony.CommandMeta{RequestID: "start-intended-successor"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := dispatcher.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	var successor symphony.WorkView
	for _, work := range view.Works {
		if work.ID != cancelledWork.ID && work.Status == symphony.WorkPending {
			successor = work
		}
	}
	if successor.ID == "" {
		t.Fatalf("intended successor missing: %+v", view.Works)
	}
	if len(runtime.starts) != 2 {
		t.Fatalf("Agent starts=%d, want initial plus intended successor only: %+v", len(runtime.starts), runtime.starts)
	}
	if session, _, found, err := engine.CurrentAgentSession(ctx, cancelledWork.ID); err != nil || found {
		t.Fatalf("cancelled Work gained stale replacement: session=%+v found=%v err=%v", session, found, err)
	}
	if _, _, found, err := engine.CurrentAgentSession(ctx, successor.ID); err != nil || !found {
		t.Fatalf("intended successor Session: found=%v err=%v", found, err)
	}
	if pending, err := engine.PendingEffects(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("stale effect was not completed as a no-op: pending=%+v err=%v", pending, err)
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
			if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
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
