package scheduler_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/scheduler"
	"github.com/reyoung/pika-go/internal/symphony"
)

type inlineExclusive struct{}

func (inlineExclusive) Exclusive(run func() error) error { return run() }

type countingDispatcher struct {
	ids []string
}

func (d *countingDispatcher) DispatchEffect(_ context.Context, id string) error {
	d.ids = append(d.ids, id)
	return nil
}

func TestControllerDoesNotRedispatchReceiptReplayOrNoop(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	dispatcher := &countingDispatcher{}
	controller := scheduler.Controller{Store: engine, Dispatcher: dispatcher, Exclusive: inlineExclusive{}}
	command := symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause"}}
	first, err := controller.Apply(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.Replayed || len(dispatcher.ids) != 1 {
		t.Fatalf("first result=%+v dispatches=%v", first, dispatcher.ids)
	}
	replayed, err := controller.Apply(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Receipt.Replayed || len(dispatcher.ids) != 1 {
		t.Fatalf("replay result=%+v dispatches=%v", replayed, dispatcher.ids)
	}
	noop, err := controller.Apply(ctx, symphony.PauseScheduler{Meta: symphony.CommandMeta{RequestID: "pause-noop"}})
	if err != nil {
		t.Fatal(err)
	}
	if noop.Control.Status != "noop" || len(dispatcher.ids) != 1 {
		t.Fatalf("noop result=%+v dispatches=%v", noop, dispatcher.ids)
	}
}
