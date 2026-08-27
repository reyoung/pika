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
