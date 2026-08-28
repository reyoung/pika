package symphony_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestReceiptReplaySurvivesDatabaseReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	command := symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "stable-request"},
		OptimizationID: "optimization-1",
		Repository:     "/repo/one",
	}
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	original, err := engine.Apply(ctx, command)
	if err != nil {
		t.Fatalf("apply init: %v", err)
	}
	before, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect before reopen: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}

	reopened, err := symphony.Open(ctx, databasePath, symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	replayed, err := reopened.Apply(ctx, command)
	if err != nil {
		t.Fatalf("replay init: %v", err)
	}
	if !replayed.Replayed || replayed.ID != original.ID || replayed.Revision != original.Revision || string(replayed.Result) != string(original.Result) {
		t.Fatalf("replayed receipt = %+v, original = %+v", replayed, original)
	}
	after, err := reopened.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect after replay: %v", err)
	}
	if after.DomainEventCount != before.DomainEventCount || after.PendingEffectCount != before.PendingEffectCount || len(after.Works) != len(before.Works) {
		t.Fatalf("replay duplicated effects: before %+v, after %+v", before, after)
	}
}

func TestCommandFailureBeforeCommitLeavesNoProjectedStateAfterReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	injected := errors.New("injected failure before command commit")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{
		NewID: countingIDs(),
		ApplyCheckpoint: func(point symphony.ApplyCheckpoint, command string) error {
			if point == symphony.ApplyBeforeCommit && command == "init" {
				return injected
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}
	if _, err := engine.Apply(ctx, command); !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want injected failure", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Inspect(ctx, symphony.Status{}); err == nil {
		t.Fatal("pre-commit failure left an initialized Optimization")
	}
	if _, err := reopened.Apply(ctx, command); err != nil {
		t.Fatalf("fresh Apply after rollback: %v", err)
	}
	view, err := reopened.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Works) != 1 || view.DomainEventCount != 1 || view.PendingEffectCount != 1 {
		t.Fatalf("projected state after retry = %+v", view)
	}
}

func TestCommandFailureAfterCommitReplaysReceiptWithoutDuplicateState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	injected := errors.New("injected response loss after command commit")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{
		NewID: countingIDs(),
		ApplyCheckpoint: func(point symphony.ApplyCheckpoint, command string) error {
			if point == symphony.ApplyAfterCommit && command == "init" {
				return injected
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}
	if _, err := engine.Apply(ctx, command); !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want injected response loss", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	beforeReplay, err := reopened.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("committed state is missing after response loss: %v", err)
	}
	receipt, err := reopened.Apply(ctx, command)
	if err != nil {
		t.Fatalf("replay after response loss: %v", err)
	}
	if !receipt.Replayed || receipt.RequestID != "init" {
		t.Fatalf("replayed receipt = %+v", receipt)
	}
	afterReplay, err := reopened.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterReplay.Works) != 1 || afterReplay.DomainEventCount != 1 || afterReplay.PendingEffectCount != 1 ||
		len(afterReplay.Works) != len(beforeReplay.Works) || afterReplay.DomainEventCount != beforeReplay.DomainEventCount || afterReplay.PendingEffectCount != beforeReplay.PendingEffectCount {
		t.Fatalf("replay duplicated committed state: before=%+v after=%+v", beforeReplay, afterReplay)
	}
}

func TestBestTransitionSurvivesDatabaseCommitBoundaryFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		checkpoint symphony.ApplyCheckpoint
		committed  bool
	}{
		{name: "before commit", checkpoint: symphony.ApplyBeforeCommit, committed: false},
		{name: "after commit response loss", checkpoint: symphony.ApplyAfterCommit, committed: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "pika.db")
			injected := errors.New("injected finish integration boundary failure")
			fired := false
			engine, integration := integrationReadyEngine(t, ctx, path, func(point symphony.ApplyCheckpoint, command string) error {
				if !fired && point == test.checkpoint && command == "finish_integration" {
					fired = true
					return injected
				}
				return nil
			})
			command := symphony.FinishIntegration{
				Meta: symphony.CommandMeta{RequestID: "finish-integration"}, WorkID: integration.ID,
				Outcome: symphony.IntegrationAccepted, ObservedBestSHA: "baseline-sha", AppliedSHA: "applied-sha",
				Result: json.RawMessage(`{"verified":true}`),
			}
			if _, err := engine.Apply(ctx, command); !errors.Is(err, injected) {
				t.Fatalf("finish error=%v, want injected failure", err)
			}
			if !fired {
				t.Fatal("finish Integration checkpoint did not fire")
			}
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := symphony.Open(ctx, path, symphony.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			beforeReplay, err := reopened.Inspect(ctx, symphony.Status{})
			if err != nil {
				t.Fatal(err)
			}
			wantSequence := int64(0)
			if test.committed {
				wantSequence = 1
			}
			if beforeReplay.Best == nil || beforeReplay.Best.Sequence != wantSequence {
				t.Fatalf("Best after boundary failure=%+v, want sequence %d", beforeReplay.Best, wantSequence)
			}
			receipt, err := reopened.Apply(ctx, command)
			if err != nil {
				t.Fatalf("retry finish Integration: %v", err)
			}
			if receipt.Replayed != test.committed {
				t.Fatalf("retry replayed=%v, want %v", receipt.Replayed, test.committed)
			}
			afterReplay, err := reopened.Inspect(ctx, symphony.Status{})
			if err != nil {
				t.Fatal(err)
			}
			if afterReplay.Best == nil || afterReplay.Best.Sequence != 1 || afterReplay.Best.CommitSHA != "applied-sha" {
				t.Fatalf("retry did not produce exactly one Best transition: %+v", afterReplay.Best)
			}
		})
	}
}

func integrationReadyEngine(t *testing.T, ctx context.Context, path string, checkpoint func(symphony.ApplyCheckpoint, string) error) (*symphony.Engine, symphony.WorkView) {
	t.Helper()
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: countingIDs(), ApplyCheckpoint: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "submit-baseline"}, WorkID: view.Works[0].ID, Definition: validBaselineDefinition(),
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	verification := pendingWorkByRole(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta: symphony.CommandMeta{RequestID: "accept-baseline"}, WorkID: verification.ID,
		Decision: symphony.VerificationAccepted, Evidence: validBenchmarkEvidence(), InitialBestSHA: "baseline-sha",
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	iteration := pendingWorksByRole(view, symphony.RoleIteration)[0]
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "finish-iteration"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-sha", Summary: "faster",
	}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	integration := pendingWorkByRole(t, view, symphony.RoleIntegration)
	if _, err := engine.Apply(ctx, symphony.PrepareBestUpdate{
		Meta: symphony.CommandMeta{RequestID: "prepare-best"}, WorkID: integration.ID, Validation: validIntegrationValidation(),
	}); err != nil {
		t.Fatal(err)
	}
	return engine, integration
}

func TestSameRequestIDDifferentBodyIsRejectedWithoutMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{NewID: countingIDs()})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "same-request"},
		OptimizationID: "optimization-1",
		Repository:     "/repo/one",
	}); err != nil {
		t.Fatalf("apply init: %v", err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})

	_, err = engine.Apply(ctx, symphony.Init{
		Meta:           symphony.CommandMeta{RequestID: "same-request"},
		OptimizationID: "optimization-1",
		Repository:     "/repo/two",
	})
	assertDomainCode(t, err, symphony.CodeIdempotencyConflict)
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Repository != before.Optimization.Repository || after.Optimization.Revision != before.Optimization.Revision || after.DomainEventCount != before.DomainEventCount {
		t.Fatalf("conflict mutated state: before %+v, after %+v", before, after)
	}
}

func TestTerminalReceiptReplayAfterReopenDoesNotDuplicateSuccessor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("before")})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization-1", Repository: "/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	draft, _ := engine.Inspect(ctx, symphony.Status{})
	command := symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "terminal-submit"},
		WorkID:     draft.Works[0].ID,
		Definition: validBaselineDefinition(),
	}
	original, err := engine.Apply(ctx, command)
	if err != nil {
		t.Fatalf("submit baseline: %v", err)
	}
	before, _ := engine.Inspect(ctx, symphony.Status{})
	if err := engine.Close(); err != nil {
		t.Fatalf("close after terminal command: %v", err)
	}

	reopened, err := symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("after")})
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	replayed, err := reopened.Apply(ctx, command)
	if err != nil {
		t.Fatalf("replay terminal command: %v", err)
	}
	if !replayed.Replayed || replayed.ID != original.ID {
		t.Fatalf("replayed receipt = %+v, original = %+v", replayed, original)
	}
	after, _ := reopened.Inspect(ctx, symphony.Status{})
	if len(after.Works) != len(before.Works) || after.DomainEventCount != before.DomainEventCount || after.PendingEffectCount != before.PendingEffectCount {
		t.Fatalf("terminal replay duplicated successor: before %+v, after %+v", before, after)
	}
}

func TestExpectedRevisionConflictDoesNotMutateState(t *testing.T) {
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
	staleRevision := before.Optimization.Revision - 1
	_, err = engine.Apply(ctx, symphony.CancelWork{
		Meta:   symphony.CommandMeta{RequestID: "cancel", ExpectedRevision: &staleRevision},
		WorkID: before.Works[0].ID,
	})
	assertDomainCode(t, err, symphony.CodeRevisionConflict)
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.Optimization.Revision != before.Optimization.Revision || after.Works[0].Status != before.Works[0].Status {
		t.Fatalf("revision conflict mutated state: before %+v, after %+v", before, after)
	}
}

func TestInitFailureDiagnosticSurvivesReopenWithoutCreatingWork(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{NewID: prefixedCountingIDs("failure")})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	if err := engine.RecordInitFailure(ctx, "repository is not a Git worktree"); err != nil {
		t.Fatalf("record init failure: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close engine: %v", err)
	}
	reopened, err := symphony.Open(ctx, path, symphony.Options{})
	if err != nil {
		t.Fatalf("reopen engine: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	diagnostics, err := reopened.InitDiagnostics(ctx)
	if err != nil {
		t.Fatalf("read init diagnostics: %v", err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Message != "repository is not a Git worktree" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	_, err = reopened.Inspect(ctx, symphony.Status{})
	assertDomainCode(t, err, symphony.CodeNotInitialized)
}

func assertDomainCode(t *testing.T, err error, want symphony.ErrorCode) {
	t.Helper()
	var domainErr *symphony.DomainError
	if !errors.As(err, &domainErr) {
		t.Fatalf("error = %v, want DomainError %q", err, want)
	}
	if domainErr.Code != want {
		t.Fatalf("error code = %q, want %q", domainErr.Code, want)
	}
}
