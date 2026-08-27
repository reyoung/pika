package symphony_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestInitCreatesBaselineDraftAtomically(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now:   func() time.Time { return time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC) },
		NewID: sequenceIDs("receipt-1", "baseline-1", "work-1", "event-1", "effect-1"),
	})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	receipt, err := engine.Apply(ctx, symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "request-1"},
		OptimizationID: "optimization-1",
		Repository:     "/workspace/repository",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if receipt.Revision != 1 || receipt.Replayed {
		t.Fatalf("receipt = %+v", receipt)
	}

	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if view.Optimization.Status != symphony.OptimizationDraftingBaseline {
		t.Fatalf("optimization status = %q", view.Optimization.Status)
	}
	if view.Optimization.Revision != 1 {
		t.Fatalf("optimization revision = %d", view.Optimization.Revision)
	}
	if view.Baseline == nil || view.Baseline.Number != 1 || view.Baseline.Status != symphony.BaselineDrafting {
		t.Fatalf("baseline = %+v", view.Baseline)
	}
	if len(view.Works) != 1 || view.Works[0].Role != symphony.RoleBaselineDraft || view.Works[0].Status != symphony.WorkPending {
		t.Fatalf("works = %+v", view.Works)
	}
	if view.DomainEventCount != 1 || view.PendingEffectCount != 1 {
		t.Fatalf("audit counts = events %d, effects %d", view.DomainEventCount, view.PendingEffectCount)
	}
}

func TestInitPersistsSchedulerSettingsUsedWhenOptimizationStarts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
		IterationConcurrency: 2, MaxPendingAttempts: 6,
	}); err != nil {
		t.Fatal(err)
	}
	draft, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: draft.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatal(err)
	}
	verification, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta: symphony.CommandMeta{RequestID: "verify"}, WorkID: verification.Works[1].ID,
		Decision: symphony.VerificationAccepted, InitialBestSHA: "best-0",
	}); err != nil {
		t.Fatal(err)
	}
	started, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Attempts) != 2 {
		t.Fatalf("initial Attempts = %d, want configured concurrency 2", len(started.Attempts))
	}
}

func TestSubmitBaselineDefinitionStartsIndependentVerification(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now:   func() time.Time { return time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC) },
		NewID: countingIDs(),
	})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "init"},
		OptimizationID: "optimization-1",
		Repository:     "/workspace/repository",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	before, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect initial state: %v", err)
	}

	revision := before.Optimization.Revision
	definition := json.RawMessage(`{"target":"kernel","metric":"latency_ms"}`)
	receipt, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "submit", ExpectedRevision: &revision},
		WorkID:     before.Works[0].ID,
		Definition: definition,
	})
	if err != nil {
		t.Fatalf("submit baseline: %v", err)
	}
	if receipt.Revision != 2 {
		t.Fatalf("receipt revision = %d, want 2", receipt.Revision)
	}

	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect submitted state: %v", err)
	}
	if after.Optimization.Status != symphony.OptimizationVerifyingBaseline {
		t.Fatalf("optimization status = %q", after.Optimization.Status)
	}
	if after.Baseline == nil || after.Baseline.Status != symphony.BaselineVerifying || string(after.Baseline.Definition) != string(definition) {
		t.Fatalf("baseline = %+v", after.Baseline)
	}
	if len(after.Works) != 2 || after.Works[0].Status != symphony.WorkCompleted || after.Works[1].Role != symphony.RoleBaselineVerification || after.Works[1].Status != symphony.WorkPending {
		t.Fatalf("works = %+v", after.Works)
	}
}

func TestFinishBaselineVerificationTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		decision             symphony.VerificationDecision
		failureKind          string
		reason               string
		requestedChanges     string
		wantOptimization     symphony.OptimizationStatus
		wantBaselines        int
		wantCurrentBaseline  symphony.BaselineStatus
		wantHistoricalStatus symphony.BaselineStatus
		wantWorks            int
	}{
		{
			name:                 "accepted",
			decision:             symphony.VerificationAccepted,
			wantOptimization:     symphony.OptimizationOptimizing,
			wantBaselines:        1,
			wantCurrentBaseline:  symphony.BaselineAccepted,
			wantHistoricalStatus: symphony.BaselineAccepted,
			wantWorks:            6,
		},
		{
			name:                 "rejected creates successor",
			decision:             symphony.VerificationRejected,
			failureKind:          "measurement_unstable",
			reason:               "variance exceeds the declared tolerance",
			requestedChanges:     "increase warmup and repeat count",
			wantOptimization:     symphony.OptimizationDraftingBaseline,
			wantBaselines:        2,
			wantCurrentBaseline:  symphony.BaselineDrafting,
			wantHistoricalStatus: symphony.BaselineRejected,
			wantWorks:            3,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			engine := openSubmittedBaseline(t, ctx)
			before, err := engine.Inspect(ctx, symphony.Status{})
			if err != nil {
				t.Fatalf("inspect before verification: %v", err)
			}
			revision := before.Optimization.Revision
			verificationWork := before.Works[len(before.Works)-1]
			if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
				Meta:             symphony.CommandMeta{RequestID: "finish", ExpectedRevision: &revision},
				WorkID:           verificationWork.ID,
				Decision:         test.decision,
				FailureKind:      test.failureKind,
				Reason:           test.reason,
				RequestedChanges: test.requestedChanges,
				Evidence:         json.RawMessage(`{"command":"benchmark --verify"}`),
			}); err != nil {
				t.Fatalf("finish verification: %v", err)
			}

			after, err := engine.Inspect(ctx, symphony.Status{})
			if err != nil {
				t.Fatalf("inspect after verification: %v", err)
			}
			if after.Optimization.Status != test.wantOptimization || after.Optimization.Revision != 3 {
				t.Fatalf("optimization = %+v", after.Optimization)
			}
			if len(after.Baselines) != test.wantBaselines {
				t.Fatalf("baselines = %+v", after.Baselines)
			}
			if after.Baseline == nil || after.Baseline.Status != test.wantCurrentBaseline {
				t.Fatalf("current baseline = %+v", after.Baseline)
			}
			if after.Baselines[0].Status != test.wantHistoricalStatus || len(after.Baselines[0].Definition) == 0 {
				t.Fatalf("historical baseline = %+v", after.Baselines[0])
			}
			if len(after.Works) != test.wantWorks || after.Works[1].Status != symphony.WorkCompleted {
				t.Fatalf("works = %+v", after.Works)
			}
			if test.decision == symphony.VerificationRejected && after.Baseline.PredecessorID != after.Baselines[0].ID {
				t.Fatalf("successor baseline = %+v", after.Baseline)
			}
			if test.decision == symphony.VerificationRejected {
				historical := after.Baselines[0]
				if historical.FailureKind != test.failureKind || historical.FailureReason != test.reason || historical.RequestedChanges != test.requestedChanges || string(historical.VerificationEvidence) != `{"command":"benchmark --verify"}` {
					t.Fatalf("historical rejection context = %+v", historical)
				}
				runtimeWork, err := engine.RuntimeWork(ctx, after.Works[len(after.Works)-1].ID)
				if err != nil {
					t.Fatalf("read successor Draft runtime context: %v", err)
				}
				if runtimeWork.PredecessorBaselineID != historical.ID || runtimeWork.PredecessorFailureKind != test.failureKind || runtimeWork.PredecessorFailureReason != test.reason || runtimeWork.PredecessorRequestedChanges != test.requestedChanges || string(runtimeWork.PredecessorVerificationEvidence) != `{"command":"benchmark --verify"}` {
					t.Fatalf("successor Draft rejection context = %+v", runtimeWork)
				}
			}
		})
	}
}

func TestCancelBaselineWorkPausesOptimization(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.CancelWork{Meta: symphony.CommandMeta{RequestID: "cancel"}, WorkID: before.Works[0].ID}); err != nil {
		t.Fatalf("cancel work: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationPaused || after.Works[0].Status != symphony.WorkCancelled {
		t.Fatalf("view after cancel = %+v", after)
	}
}

func TestBackOffVerificationCreatesNewDraftWithMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := openSubmittedBaseline(t, ctx)
	before, _ := engine.Inspect(ctx, symphony.Status{})
	verificationWork := before.Works[len(before.Works)-1]
	if _, err := engine.Apply(ctx, symphony.BackOff{
		Meta:    symphony.CommandMeta{RequestID: "back-off"},
		WorkID:  verificationWork.ID,
		Message: "the benchmark needs a longer warmup",
	}); err != nil {
		t.Fatalf("back off: %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Status != symphony.OptimizationDraftingBaseline || len(after.Baselines) != 2 || after.Baseline.PredecessorID != after.Baselines[0].ID {
		t.Fatalf("view after back-off = %+v", after)
	}
	if len(after.BackOffs) != 1 || after.BackOffs[0].Message != "the benchmark needs a longer warmup" {
		t.Fatalf("back-offs = %+v", after.BackOffs)
	}
}

func TestRequestShutdownDrainsWithoutCancellingWork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*testing.T, context.Context) (*symphony.Engine, symphony.View)
	}{
		{name: "drafting baseline", prepare: draftState},
		{name: "verifying baseline", prepare: submittedState},
		{name: "optimizing", prepare: acceptedState},
		{name: "paused", prepare: pausedState},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			engine, before := test.prepare(t, ctx)
			if _, err := engine.Apply(ctx, symphony.RequestShutdown{Meta: symphony.CommandMeta{RequestID: "shutdown"}}); err != nil {
				t.Fatalf("request shutdown: %v", err)
			}
			after, _ := engine.Inspect(ctx, symphony.Status{})
			if after.Optimization.Status != symphony.OptimizationDraining {
				t.Fatalf("optimization status = %q", after.Optimization.Status)
			}
			if len(after.Works) != len(before.Works) {
				t.Fatalf("shutdown changed work count: before %+v, after %+v", before.Works, after.Works)
			}
			for index := range before.Works {
				if after.Works[index].Status != before.Works[index].Status {
					t.Fatalf("shutdown mutated work: before %+v, after %+v", before.Works, after.Works)
				}
			}
		})
	}
}

func openSubmittedBaseline(t *testing.T, ctx context.Context) *symphony.Engine {
	t.Helper()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{
		Now:   func() time.Time { return time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC) },
		NewID: countingIDs(),
	})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "init"},
		OptimizationID: "optimization-1",
		Repository:     "/workspace/repository",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	initial, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect initialized state: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "submit"},
		WorkID:     initial.Works[0].ID,
		Definition: json.RawMessage(`{"target":"kernel","metric":"latency_ms"}`),
	}); err != nil {
		t.Fatalf("submit baseline: %v", err)
	}
	return engine
}

func sequenceIDs(ids ...string) func() string {
	index := 0
	return func() string {
		if index >= len(ids) {
			panic("test exhausted deterministic IDs")
		}
		id := ids[index]
		index++
		return id
	}
}

func countingIDs() func() string {
	index := 0
	return func() string {
		index++
		return fmt.Sprintf("id-%d", index)
	}
}
