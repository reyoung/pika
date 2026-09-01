package workruntime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/workruntime"
)

func TestCoordinatorRecoversUncertainEffectsOnEveryRuntimeSnapshot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	store := &countingUncertainStore{Engine: engine}
	watcher := &twoSnapshotRuntime{cancel: cancel}
	dispatcher := &countingDispatcher{}
	coordinator := workruntime.Coordinator{
		Reconciler:  workruntime.Reconciler{Store: store, Runtime: watcher},
		Dispatcher:  dispatcher,
		EffectStore: store,
		Runtime:     watcher,
	}
	if err := coordinator.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if store.recoveries != 2 {
		t.Fatalf("uncertain-effect recoveries = %d, want one per runtime snapshot", store.recoveries)
	}
	if dispatcher.calls != 2 {
		t.Fatalf("dispatch calls = %d, want one per runtime snapshot", dispatcher.calls)
	}
}

func TestCoordinatorQuiesceWaitsForCriticalSectionAndRejectsEveryDispatchEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coordinator := &workruntime.Coordinator{}
	entered, release := make(chan struct{}), make(chan struct{})
	exclusiveDone := make(chan error, 1)
	go func() {
		exclusiveDone <- coordinator.Exclusive(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	quiesced := make(chan struct{})
	go func() {
		if err := coordinator.Quiesce(func() error { close(quiesced); return nil }); err != nil {
			t.Errorf("quiesce: %v", err)
		}
	}()
	select {
	case <-quiesced:
		t.Fatal("quiesce did not wait for the existing critical section")
	default:
	}
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	<-quiesced

	if err := coordinator.RecoverAndDispatch(ctx); !errors.Is(err, workruntime.ErrQuiesced) {
		t.Fatalf("direct dispatch after quiesce error = %v", err)
	}
	if err := coordinator.RecoverEffectsAndDispatch(ctx); !errors.Is(err, workruntime.ErrQuiesced) {
		t.Fatalf("non-Herdr dispatch after quiesce error = %v", err)
	}
	called := false
	promoted, err := coordinator.Promote(func() (bool, error) {
		called = true
		return true, nil
	})
	if err != nil || promoted || called {
		t.Fatalf("promotion crossed quiesce: promoted=%v called=%v err=%v", promoted, called, err)
	}
}

func TestCoordinatorQuiesceFailureLeavesDispatchSeamUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	dispatcher := &countingDispatcher{}
	coordinator := &workruntime.Coordinator{
		Reconciler:  workruntime.Reconciler{Store: engine, Runtime: nopRuntime{}},
		Dispatcher:  dispatcher,
		EffectStore: engine,
	}
	want := errors.New("persist failed")
	if err := coordinator.Quiesce(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("quiesce error = %v", err)
	}
	if err := coordinator.Exclusive(func() error { return nil }); err != nil {
		t.Fatalf("exclusive after failed quiesce: %v", err)
	}
	if err := coordinator.RecoverAndDispatch(ctx); err != nil {
		t.Fatalf("dispatch after failed quiesce: %v", err)
	}
	if dispatcher.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", dispatcher.calls)
	}
	if err := coordinator.Quiesce(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RecoverAndDispatch(ctx); !errors.Is(err, workruntime.ErrQuiesced) {
		t.Fatalf("successful quiesce did not freeze dispatch: %v", err)
	}
}

type countingUncertainStore struct {
	*symphony.Engine
	recoveries int
}

func (s *countingUncertainStore) RecoverUncertainEffects(ctx context.Context) error {
	s.recoveries++
	return s.Engine.RecoverUncertainEffects(ctx)
}

type countingDispatcher struct{ calls int }

func (d *countingDispatcher) DispatchPending(context.Context) error {
	d.calls++
	return nil
}

type nopRuntime struct{}

func (nopRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	return workruntime.Snapshot{}, nil
}
func (nopRuntime) Start(context.Context, workruntime.StartSpec) (workruntime.Observation, error) {
	return workruntime.Observation{}, errors.New("unexpected Start")
}
func (nopRuntime) Prompt(context.Context, string, string) error {
	return errors.New("unexpected Prompt")
}
func (nopRuntime) Close(context.Context, string) error { return errors.New("unexpected Close") }

type twoSnapshotRuntime struct{ cancel context.CancelFunc }

func (r *twoSnapshotRuntime) Watch(ctx context.Context, callback func(context.Context, workruntime.Snapshot) error) error {
	for range 2 {
		if err := callback(ctx, workruntime.Snapshot{}); err != nil {
			return err
		}
	}
	r.cancel()
	return nil
}

func (*twoSnapshotRuntime) Snapshot(context.Context) (workruntime.Snapshot, error) {
	return workruntime.Snapshot{}, nil
}

func (*twoSnapshotRuntime) Start(context.Context, workruntime.StartSpec) (workruntime.Observation, error) {
	return workruntime.Observation{}, errors.New("unexpected Start")
}

func (*twoSnapshotRuntime) Prompt(context.Context, string, string) error {
	return errors.New("unexpected Prompt")
}

func (*twoSnapshotRuntime) Close(context.Context, string) error {
	return errors.New("unexpected Close")
}
