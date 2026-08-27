package outbox_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestDispatcherRecordsAndAcknowledgesCommittedEffects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	recorder := &outbox.Recorder{}
	dispatcher := outbox.Dispatcher{Store: engine, Sink: recorder}
	if err := dispatcher.DispatchPending(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(recorder.Effects()) != 1 || recorder.Effects()[0].Type != "work.start_requested" {
		t.Fatalf("recorded effects = %+v", recorder.Effects())
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if view.PendingEffectCount != 0 {
		t.Fatalf("pending effects = %d, want 0", view.PendingEffectCount)
	}
	if err := dispatcher.DispatchPending(ctx); err != nil {
		t.Fatalf("redispatch: %v", err)
	}
	if len(recorder.Effects()) != 1 {
		t.Fatalf("acknowledged effect was dispatched again: %+v", recorder.Effects())
	}
}

func TestUncertainEffectIsRecoveredAndReconciledBySink(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	claimed, err := engine.ClaimPendingEffects(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim effects=%+v err=%v", claimed, err)
	}
	if pending, err := engine.PendingEffects(ctx, 1); err != nil || len(pending) != 0 {
		t.Fatalf("pending while dispatch uncertain=%+v err=%v", pending, err)
	}
	if err := engine.RecoverUncertainEffects(ctx); err != nil {
		t.Fatalf("recover uncertain effects: %v", err)
	}
	recorder := &outbox.Recorder{}
	if err := (outbox.Dispatcher{Store: engine, Sink: recorder}).DispatchPending(ctx); err != nil {
		t.Fatalf("redispatch recovered effect: %v", err)
	}
	if got := recorder.Effects(); len(got) != 1 || got[0].ID != claimed[0].ID {
		t.Fatalf("recovered effects = %+v", got)
	}
}
