package symphony_test

import (
	"context"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestReconfigureIterationAgentsAtomicallyCancelsRemovedSlots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	if err := engine.ReconfigureIterationAgents(ctx, 1); err != nil {
		t.Fatal(err)
	}
	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Optimization.IterationConcurrency != 1 || len(pendingWorksByRole(after, symphony.RoleIteration)) != 1 {
		t.Fatalf("reconfigured state = concurrency %d works %+v", after.Optimization.IterationConcurrency, pendingWorksByRole(after, symphony.RoleIteration))
	}
	for _, attempt := range after.Attempts {
		if attempt.SlotIndex > 0 && (attempt.Status != "cancelled" || attempt.FailureReason != "iteration_agent_removed") {
			t.Fatalf("removed-slot Attempt was not cancelled: %+v", attempt)
		}
	}
}
