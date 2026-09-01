package maintenance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

const (
	fromDigest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	toDigest    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	thirdDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestPrepareFreezesActiveIdentitiesAndIsReplayable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	domain := &fakeDomain{targets: []maintenance.Target{{
		WorkID: "work-1", SessionID: "session-1", WorkGeneration: 7,
	}}}
	gate := &fakeGate{}
	process := maintenance.Process{
		Store: store, Runtime: gate, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest, Version: "old"}, nil
		},
		Now: func() time.Time { return now },
	}
	request := maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest, Version: "new"}}

	first, err := process.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := process.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	quiesced, calls := gate.snapshot()
	if !quiesced || calls != 2 {
		t.Fatalf("quiesce gate = %+v", gate)
	}
	if first.ID == "" || first.ID != replay.ID || replay.State != maintenance.StateQuiescing {
		t.Fatalf("prepare replay changed status: first=%+v replay=%+v", first, replay)
	}
	if first.RequestID != request.RequestID || first.FromGeneration.Digest != fromDigest || first.ToGeneration.Digest != toDigest {
		t.Fatalf("generation identity was not persisted: %+v", first)
	}
	if len(first.Targets) != 1 || first.Targets[0] != domain.targets[0] {
		t.Fatalf("frozen targets = %+v", first.Targets)
	}
}

func TestPrepareActivationWaitsForWorkspaceWritersBeforeQuiescingOrPersisting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), "workspace")
	writer, err := optimizationworkspace.AcquireSharedMutationLease(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	store := maintenance.NewFileStore(root)
	gate := &fakeGate{}
	process := maintenance.Process{
		Store: store, Runtime: gate, Domain: &fakeDomain{},
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest, Version: "old"}, nil
		},
		StateActivation: func(activationCtx context.Context, activate func() error) error {
			lease, acquireErr := optimizationworkspace.AcquireExclusiveMutationLease(activationCtx, root)
			if acquireErr != nil {
				return acquireErr
			}
			defer lease.Close()
			return activate()
		},
	}
	done := make(chan error, 1)
	go func() {
		_, prepareErr := process.Prepare(ctx, maintenance.PrepareRequest{
			RequestID: "prepare-lease", ToGeneration: maintenance.Generation{Digest: toDigest, Version: "new"},
		})
		done <- prepareErr
	}()
	select {
	case err := <-done:
		t.Fatalf("Prepare crossed active writer lease: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, calls := gate.snapshot(); calls != 0 {
		t.Fatalf("Prepare quiesced before acquiring exclusive lease: calls=%d", calls)
	}
	if _, err := store.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Prepare persisted while writer held lease: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	status, err := store.Read()
	_, calls := gate.snapshot()
	if err != nil || status.State != maintenance.StateQuiescing || calls != 1 {
		t.Fatalf("Prepare after writer release: status=%+v gate=%+v err=%v", status, gate, err)
	}
}

func TestPrepareRejectsDifferentRequestHotUpdateAndTruncatedDigest(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	process := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: &fakeDomain{},
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest}, nil
		},
	}
	request := maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest}}
	if _, err := process.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-2", ToGeneration: request.ToGeneration}); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("different request error = %v", err)
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{
		RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest, Version: "different"},
	}); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("same request with conflicting version error = %v", err)
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-3", ToGeneration: maintenance.Generation{Digest: "not-a-digest"}}); err == nil {
		t.Fatal("truncated digest was accepted")
	}

	other := maintenance.Process{
		Store: maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace")), Runtime: &fakeGate{}, Domain: &fakeDomain{},
		CurrentGeneration:   func() (maintenance.Generation, error) { return maintenance.Generation{Digest: fromDigest}, nil },
		HotUpdateInProgress: func() bool { return true },
	}
	if _, err := other.Prepare(context.Background(), request); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("hot-update conflict error = %v", err)
	}
}

func TestPreparePersistFailureDoesNotAcceptState(t *testing.T) {
	t.Parallel()
	store := &scriptedStore{inner: maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace")), failAt: 1}
	gate := &fakeGate{}
	process := maintenance.Process{
		Store: store, Runtime: gate, Domain: &fakeDomain{},
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest}, nil
		},
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{
		RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest},
	}); err == nil {
		t.Fatal("prepare persist failure was accepted")
	}
	if quiesced, _ := gate.snapshot(); quiesced {
		t.Fatal("failed persist irreversibly quiesced the gate")
	}
	if _, err := store.inner.Read(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed prepare wrote durable state: %v", err)
	}
}

func TestQuiescingCrashRecoveryBecomesReadyOnlyWhenFrozenWorksTerminal(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	domain := &fakeDomain{targets: []maintenance.Target{{WorkID: "work-1", SessionID: "session-1", WorkGeneration: 1}}}
	first := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) { return maintenance.Generation{Digest: fromDigest}, nil },
	}
	request := maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest}}
	if _, err := first.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	recoveredGate := &fakeGate{}
	recovered := maintenance.Process{Store: store, Runtime: recoveredGate, Domain: domain}
	status, stop, err := recovered.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !recoveredGate.quiesced || stop || status.State != maintenance.StateQuiescing {
		t.Fatalf("unsafe quiescing recovery: status=%+v stop=%v gate=%+v", status, stop, recoveredGate)
	}
	domain.terminal = true
	status, stop, err = recovered.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stop || status.State != maintenance.StateReady || status.ReadyAt == "" {
		t.Fatalf("terminal frozen Work did not become ready: status=%+v stop=%v", status, stop)
	}
}

func TestPollIdentityErrorPersistsFailedAndAllowsNewPrepare(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	domain := &fakeDomain{targets: []maintenance.Target{{WorkID: "work-1", SessionID: "session-1", WorkGeneration: 1}}, inspectErr: errors.New("generation drifted")}
	process := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) { return maintenance.Generation{Digest: fromDigest}, nil },
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest}}); err != nil {
		t.Fatal(err)
	}
	status, stop, err := process.Poll(context.Background())
	if err == nil || stop || status.State != maintenance.StateFailed {
		t.Fatalf("identity failure = status=%+v stop=%v err=%v", status, stop, err)
	}
	domain.inspectErr = nil
	next, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-2", ToGeneration: maintenance.Generation{Digest: toDigest}})
	if err != nil || next.State != maintenance.StateQuiescing || next.RequestID != "prepare-2" {
		t.Fatalf("prepare after failed = %+v err=%v", next, err)
	}
}

func TestHoldingResumeRetriesAfterPersistedWriteFailureWithoutDuplicateRuntime(t *testing.T) {
	t.Parallel()
	inner := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	store := &scriptedStore{inner: inner}
	domain := &fakeDomain{terminal: true}
	process := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest}, nil
		},
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.RecoverWithGeneration(context.Background(), maintenance.Generation{Digest: toDigest}); err != nil {
		t.Fatal(err)
	}
	holdingBeforeResume, err := inner.Read()
	if err != nil {
		t.Fatal(err)
	}

	steps := 0
	process.ResumeRuntime = func(_ context.Context, targets []maintenance.Target) error {
		if !reflect.DeepEqual(targets, holdingBeforeResume.Targets) {
			t.Fatalf("resume targets=%+v, want %+v", targets, holdingBeforeResume.Targets)
		}
		current, err := inner.Read()
		if err != nil {
			return err
		}
		if current.State != maintenance.StateHolding || current.ResumeRequestID != "resume-1" {
			t.Fatalf("state during runtime recovery = %+v", current)
		}
		steps++
		return nil
	}
	store.failAt = store.writes + 2
	if _, err := process.Resume(context.Background(), maintenance.ResumeRequest{RequestID: "resume-1"}); err == nil {
		t.Fatal("resumed persist failure was accepted")
	}
	if steps != 1 {
		t.Fatalf("runtime steps after failed persist = %d", steps)
	}
	holding, err := inner.Read()
	if err != nil || holding.State != maintenance.StateHolding || holding.ResumeRequestID != "resume-1" {
		t.Fatalf("intent was not durable: %+v err=%v", holding, err)
	}
	store.failAt = 0
	resumed, err := process.Resume(context.Background(), maintenance.ResumeRequest{RequestID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	if steps != 1 || resumed.State != maintenance.StateResumed {
		t.Fatalf("retry duplicated runtime: steps=%d resumed=%+v", steps, resumed)
	}
	if _, err := process.Resume(context.Background(), maintenance.ResumeRequest{RequestID: "resume-2"}); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("different resume replay error = %v", err)
	}
}

func TestHoldingResumeCrashAfterRuntimeStartReplaysDurableIntent(t *testing.T) {
	t.Parallel()
	inner := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	store := &scriptedStore{inner: inner}
	domain := &fakeDomain{terminal: true}
	first := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) {
			return maintenance.Generation{Digest: fromDigest}, nil
		},
	}
	if _, err := first.Prepare(context.Background(), maintenance.PrepareRequest{
		RequestID: "prepare-1", ToGeneration: maintenance.Generation{Digest: toDigest},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.RecoverWithGeneration(context.Background(), maintenance.Generation{Digest: toDigest}); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	applied := map[string]bool{}
	resumeStep := func(context.Context, []maintenance.Target) error {
		attempts++
		applied["retire-recover-dispatch"] = true
		return nil
	}
	first.ResumeRuntime = resumeStep
	store.failAt = store.writes + 2
	if _, err := first.Resume(context.Background(), maintenance.ResumeRequest{RequestID: "resume-1"}); err == nil {
		t.Fatal("final resume write failure was accepted")
	}

	store.failAt = 0
	restarted := maintenance.Process{Store: store, ResumeRuntime: resumeStep}
	resumed, err := restarted.Resume(context.Background(), maintenance.ResumeRequest{RequestID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != maintenance.StateResumed || attempts != 2 || len(applied) != 1 {
		t.Fatalf("crash retry did not replay one idempotent operation: status=%+v attempts=%d applied=%v", resumed, attempts, applied)
	}
}

func TestHoldingTakeoverAcceptsTargetAndOriginalFromGenerationOnly(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	domain := &fakeDomain{terminal: true}
	bridge := maintenance.Generation{Digest: fromDigest, Version: "bridge"}
	target := maintenance.Generation{Digest: toDigest, Version: "current"}
	process := maintenance.Process{
		Store: store, Runtime: &fakeGate{}, Domain: domain,
		CurrentGeneration: func() (maintenance.Generation, error) { return bridge, nil },
	}
	if _, err := process.Prepare(context.Background(), maintenance.PrepareRequest{RequestID: "prepare-1", ToGeneration: target}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	holding, stop, err := process.RecoverWithGeneration(context.Background(), target)
	if err != nil || stop || holding.State != maintenance.StateHolding || holding.HoldingGeneration.Digest != toDigest {
		t.Fatalf("target holding = %+v stop=%v err=%v", holding, stop, err)
	}
	reopen, _, err := process.RecoverWithGeneration(context.Background(), target)
	if err != nil || reopen.HoldingGeneration.Digest != toDigest {
		t.Fatalf("same-target reopen = %+v err=%v", reopen, err)
	}
	if _, _, err := process.RecoverWithGeneration(context.Background(), maintenance.Generation{Digest: fromDigest[:16]}); err == nil {
		t.Fatal("truncated digest was accepted for holding takeover")
	}
	if _, _, err := process.RecoverWithGeneration(context.Background(), maintenance.Generation{Digest: thirdDigest}); !errors.Is(err, maintenance.ErrConflict) {
		t.Fatalf("third generation error = %v", err)
	}

	restored := maintenance.Process{Store: store, Runtime: &fakeGate{}, Domain: domain}
	status, stop, err := restored.RecoverWithGeneration(context.Background(), bridge)
	if err != nil {
		t.Fatal(err)
	}
	if stop || status.State != maintenance.StateHolding || status.HoldingGeneration.Digest != fromDigest {
		t.Fatalf("bridge did not safely reclaim holding: status=%+v stop=%v", status, stop)
	}
	status, stop, err = restored.RecoverWithGeneration(context.Background(), target)
	if err != nil || stop || status.HoldingGeneration != target {
		t.Fatalf("target did not reclaim holding from bridge: status=%+v stop=%v err=%v", status, stop, err)
	}
	status, stop, err = restored.RecoverWithGeneration(context.Background(), bridge)
	if err != nil || stop || status.HoldingGeneration != bridge {
		t.Fatalf("bridge did not reclaim holding again: status=%+v stop=%v err=%v", status, stop, err)
	}
}

func TestAuthorizeGenerationFailsClosedByMaintenanceState(t *testing.T) {
	t.Parallel()
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	status := maintenance.Status{
		ID: "maintenance-1", RequestID: "prepare-1", State: maintenance.StateQuiescing,
		FromGeneration: maintenance.Generation{Digest: fromDigest},
		ToGeneration:   maintenance.Generation{Digest: toDigest},
		Targets:        []maintenance.Target{{WorkID: "work-1", SessionID: "session-1", WorkGeneration: 1}},
		StartedAt:      "2026-09-01T11:00:00Z", UpdatedAt: "2026-09-01T11:00:00Z",
	}
	assertAllowed := func(generation string) {
		t.Helper()
		if _, err := maintenance.AuthorizeGeneration(store, maintenance.Generation{Digest: generation}); err != nil {
			t.Fatalf("%s rejected in %s: %v", generation, status.State, err)
		}
	}
	assertRejected := func(generation string) {
		t.Helper()
		if _, err := maintenance.AuthorizeGeneration(store, maintenance.Generation{Digest: generation}); !errors.Is(err, maintenance.ErrConflict) {
			t.Fatalf("%s accepted in %s: %v", generation, status.State, err)
		}
	}
	write := func() {
		t.Helper()
		if err := store.Write(status); err != nil {
			t.Fatal(err)
		}
	}

	write()
	assertAllowed(fromDigest)
	assertRejected(toDigest)
	assertRejected(thirdDigest)

	status.State = maintenance.StateReady
	status.ReadyAt = "2026-09-01T11:01:00Z"
	status.UpdatedAt = status.ReadyAt
	write()
	assertAllowed(fromDigest)
	assertAllowed(toDigest)
	assertRejected(thirdDigest)

	status.State = maintenance.StateHolding
	status.HoldingGeneration = maintenance.Generation{Digest: thirdDigest}
	status.HoldingAt = "2026-09-01T11:02:00Z"
	status.UpdatedAt = status.HoldingAt
	write()
	assertAllowed(fromDigest)
	assertAllowed(toDigest)
	assertAllowed(thirdDigest)

	status.State = maintenance.StateFailed
	status.HoldingGeneration = maintenance.Generation{}
	status.ReadyAt, status.HoldingAt = "", ""
	status.Failure = "identity drift"
	status.UpdatedAt = "2026-09-01T11:03:00Z"
	write()
	assertAllowed(fromDigest)
	assertRejected(toDigest)
	assertRejected(thirdDigest)

	status.State = maintenance.StateResumed
	status.ResumeRequestID = "resume-1"
	status.Failure = ""
	status.ReadyAt = "2026-09-01T11:01:00Z"
	status.HoldingGeneration = maintenance.Generation{Digest: toDigest}
	status.HoldingAt = "2026-09-01T11:02:00Z"
	status.ResumedAt = "2026-09-01T11:04:00Z"
	status.UpdatedAt = status.ResumedAt
	write()
	assertAllowed(thirdDigest)
}

func TestDirectMutationAndHoldingWebUIAuthorizationMatrix(t *testing.T) {
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	from := maintenance.Generation{Digest: fromDigest}
	to := maintenance.Generation{Digest: toDigest}
	if _, err := maintenance.AuthorizeDirectMutation(store, from); err != nil {
		t.Fatalf("absent direct mutation: %v", err)
	}
	base := maintenance.Status{
		ID: "maintenance", RequestID: "prepare",
		FromGeneration: from, ToGeneration: to,
		Targets:   []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt: "2026-09-01T11:00:00Z", UpdatedAt: "2026-09-01T11:00:00Z",
	}
	for _, state := range []maintenance.State{
		maintenance.StateQuiescing, maintenance.StateReady, maintenance.StateHolding, maintenance.StateFailed,
	} {
		status := base
		status.State = state
		switch state {
		case maintenance.StateReady:
			status.ReadyAt, status.UpdatedAt = "2026-09-01T11:01:00Z", "2026-09-01T11:01:00Z"
		case maintenance.StateHolding:
			status.ReadyAt = "2026-09-01T11:01:00Z"
			status.HoldingAt, status.UpdatedAt = "2026-09-01T11:02:00Z", "2026-09-01T11:02:00Z"
			status.HoldingGeneration = to
		case maintenance.StateFailed:
			status.Failure = "identity drift"
		}
		if err := store.Write(status); err != nil {
			t.Fatal(err)
		}
		for _, generation := range []maintenance.Generation{from, to} {
			if _, err := maintenance.AuthorizeDirectMutation(store, generation); !errors.Is(err, maintenance.ErrConflict) {
				t.Fatalf("direct mutation generation=%s state=%s error=%v", generation.Digest, state, err)
			}
		}
		if _, err := maintenance.AuthorizeHoldingWebUI(store, to); state == maintenance.StateHolding {
			if err != nil {
				t.Fatalf("target Holding WebUI rejected: %v", err)
			}
		} else if !errors.Is(err, maintenance.ErrConflict) {
			t.Fatalf("WebUI state=%s error=%v", state, err)
		}
		if _, err := maintenance.AuthorizeHoldingWebUI(store, from); !errors.Is(err, maintenance.ErrConflict) {
			t.Fatalf("from-generation WebUI state=%s error=%v", state, err)
		}
	}
	resumed := base
	resumed.State = maintenance.StateResumed
	resumed.ReadyAt = "2026-09-01T11:01:00Z"
	resumed.HoldingGeneration = to
	resumed.HoldingAt = "2026-09-01T11:02:00Z"
	resumed.ResumeRequestID = "resume"
	resumed.ResumedAt, resumed.UpdatedAt = "2026-09-01T11:03:00Z", "2026-09-01T11:03:00Z"
	if err := store.Write(resumed); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.AuthorizeDirectMutation(store, maintenance.Generation{Digest: thirdDigest}); err != nil {
		t.Fatalf("resumed direct mutation: %v", err)
	}
	if _, err := maintenance.AuthorizeHoldingWebUI(store, maintenance.Generation{Digest: thirdDigest}); err != nil {
		t.Fatalf("resumed WebUI: %v", err)
	}
}

func TestRecoverValidatesDurableIdentityAfterResumeIntent(t *testing.T) {
	store := maintenance.NewFileStore(filepath.Join(t.TempDir(), "workspace"))
	from := maintenance.Generation{Digest: fromDigest}
	to := maintenance.Generation{Digest: toDigest}
	status := maintenance.Status{
		ID: "maintenance", RequestID: "prepare", ResumeRequestID: "resume",
		State: maintenance.StateHolding, FromGeneration: from, ToGeneration: to, HoldingGeneration: to,
		Targets:   []maintenance.Target{{WorkID: "work", SessionID: "session", WorkGeneration: 1}},
		StartedAt: "2026-09-01T11:00:00Z", ReadyAt: "2026-09-01T11:01:00Z",
		HoldingAt: "2026-09-01T11:02:00Z", UpdatedAt: "2026-09-01T11:03:00Z",
	}
	if err := store.Write(status); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("durable/live Agent set drifted")
	domain := &recoveryValidatingDomain{fakeDomain: fakeDomain{terminal: true}, err: injected}
	process := maintenance.Process{Store: store, Runtime: &fakeGate{}, Domain: domain}
	if _, _, err := process.RecoverWithGeneration(context.Background(), to); !errors.Is(err, injected) {
		t.Fatalf("recover error=%v", err)
	}
	if domain.calls != 1 || domain.status.ResumeRequestID != "resume" {
		t.Fatalf("recovery validator calls=%d status=%+v", domain.calls, domain.status)
	}
}

type fakeGate struct {
	mu       sync.Mutex
	quiesced bool
	calls    int
}

func (g *fakeGate) Quiesce(run func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if err := run(); err != nil {
		return err
	}
	g.quiesced = true
	return nil
}

func (g *fakeGate) snapshot() (bool, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.quiesced, g.calls
}

type fakeDomain struct {
	targets    []maintenance.Target
	terminal   bool
	inspectErr error
}

type recoveryValidatingDomain struct {
	fakeDomain
	calls  int
	status maintenance.Status
	err    error
}

func (d *recoveryValidatingDomain) ValidateMaintenanceRecovery(_ context.Context, status maintenance.Status) error {
	d.calls++
	d.status = status
	return d.err
}

func (d *fakeDomain) ActiveMaintenanceTargets(context.Context) ([]maintenance.Target, error) {
	return append([]maintenance.Target(nil), d.targets...), nil
}

func (d *fakeDomain) MaintenanceTargetsTerminal(context.Context, []maintenance.Target) (bool, error) {
	if d.inspectErr != nil {
		return false, d.inspectErr
	}
	return d.terminal, nil
}

type scriptedStore struct {
	inner  *maintenance.FileStore
	writes int
	failAt int
}

func (s *scriptedStore) Read() (maintenance.Status, error) { return s.inner.Read() }

func (s *scriptedStore) Write(status maintenance.Status) error {
	s.writes++
	if s.failAt != 0 && s.writes == s.failAt {
		return errors.New("injected maintenance write failure")
	}
	return s.inner.Write(status)
}
