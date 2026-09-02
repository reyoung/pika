package symphony

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWorkStartLockEntryIsRemovedAfterReservationRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, Init{
		Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	effects, err := engine.PendingEffects(ctx, 1)
	if err != nil || len(effects) != 1 {
		t.Fatalf("effects=%+v err=%v", effects, err)
	}
	reservation, eligible, err := engine.ReserveAgentStart(ctx, AgentStartRequest{
		EffectID: effects[0].ID, WorkID: view.Works[0].ID, Intent: AgentStartInitial,
	})
	if err != nil || !eligible {
		t.Fatalf("reserve eligible=%v err=%v", eligible, err)
	}
	if len(engine.workStartLocks) != 1 {
		t.Fatalf("live per-Work locks=%d", len(engine.workStartLocks))
	}
	if err := reservation.Abort(); err != nil {
		t.Fatal(err)
	}
	if len(engine.workStartLocks) != 0 {
		t.Fatalf("released per-Work locks leaked=%d", len(engine.workStartLocks))
	}
}
