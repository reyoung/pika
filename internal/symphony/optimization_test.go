package symphony_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
)

func TestAcceptedBaselineSeedsBestAndIterationConcurrency(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := openSubmittedBaseline(t, ctx)
	before, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect submitted baseline: %v", err)
	}
	verification := pendingWorkByRole(t, before, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta:           symphony.CommandMeta{RequestID: "accept-baseline"},
		WorkID:         verification.ID,
		Decision:       symphony.VerificationAccepted,
		Evidence:       validBenchmarkEvidence(),
		InitialBestSHA: "baseline-sha",
	}); err != nil {
		t.Fatalf("accept baseline: %v", err)
	}

	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect optimization: %v", err)
	}
	if after.Best == nil || after.Best.Sequence != 0 || after.Best.CommitSHA != "baseline-sha" {
		t.Fatalf("initial Best = %+v", after.Best)
	}
	if len(after.Attempts) != 4 {
		t.Fatalf("attempt count = %d, want 4: %+v", len(after.Attempts), after.Attempts)
	}
	iterationWorks := pendingWorksByRole(after, symphony.RoleIteration)
	if len(iterationWorks) != 4 {
		t.Fatalf("pending iteration work count = %d, want 4: %+v", len(iterationWorks), after.Works)
	}
	for _, work := range iterationWorks {
		if work.AttemptID == "" || work.IterationRound != 1 {
			t.Fatalf("iteration work lacks attempt identity: %+v", work)
		}
	}
}

func TestIterationsQueueFIFOAndBestAdvanceRefreshesStaleCandidates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	initial, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect initial iterations: %v", err)
	}
	iterations := pendingWorksByRole(initial, symphony.RoleIteration)
	finishOrder := []int{2, 0, 1}
	for sequence, index := range finishOrder {
		work := iterations[index]
		if _, err := engine.Apply(ctx, symphony.FinishIteration{
			Meta:         symphony.CommandMeta{RequestID: "finish-iteration-" + string(rune('a'+sequence))},
			WorkID:       work.ID,
			Outcome:      symphony.IterationCandidate,
			CandidateSHA: "candidate-" + string(rune('a'+sequence)),
			Summary:      "improved candidate",
			Evidence:     validBenchmarkEvidence(),
		}); err != nil {
			t.Fatalf("finish iteration %d: %v", sequence, err)
		}
	}

	queued, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect integration queue: %v", err)
	}
	if len(queued.Integrations) != 3 {
		t.Fatalf("integrations = %+v", queued.Integrations)
	}
	for index, integration := range queued.Integrations {
		if integration.AttemptID != iterations[finishOrder[index]].AttemptID {
			t.Fatalf("integration FIFO order = %+v", queued.Integrations)
		}
		wantStatus := "queued"
		if index == 0 {
			wantStatus = "running"
		}
		if integration.Status != wantStatus {
			t.Fatalf("integration %d status = %q, want %q", index, integration.Status, wantStatus)
		}
		if integration.FIFOPosition != int64(index+1) {
			t.Fatalf("integration FIFO positions = %+v", queued.Integrations)
		}
	}
	integrationWork := pendingWorkByRole(t, queued, symphony.RoleIntegration)
	if integrationWork.AttemptID != iterations[2].AttemptID {
		t.Fatalf("active integration work = %+v", integrationWork)
	}
	prepareCommand := symphony.PrepareBestUpdate{
		Meta:       symphony.CommandMeta{RequestID: "prepare-best"},
		WorkID:     integrationWork.ID,
		Validation: validIntegrationValidation(),
	}
	if _, err := engine.Apply(ctx, prepareCommand); err != nil {
		t.Fatalf("prepare Best update: %v", err)
	}
	if replay, err := engine.Apply(ctx, prepareCommand); err != nil || !replay.Replayed {
		t.Fatalf("replay prepared Git intent: receipt=%+v err=%v", replay, err)
	}
	finishCommand := symphony.FinishIntegration{
		Meta:            symphony.CommandMeta{RequestID: "finish-integration"},
		WorkID:          integrationWork.ID,
		Outcome:         symphony.IntegrationAccepted,
		ObservedBestSHA: "baseline-sha",
		AppliedSHA:      "best-applied-a",
		Result:          json.RawMessage(`{"verified":true}`),
	}
	if _, err := engine.Apply(ctx, finishCommand); err != nil {
		t.Fatalf("finish integration: %v", err)
	}
	if replay, err := engine.Apply(ctx, finishCommand); err != nil || !replay.Replayed {
		t.Fatalf("replay finished Integration: receipt=%+v err=%v", replay, err)
	}

	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect advanced Best: %v", err)
	}
	if after.Best == nil || after.Best.Sequence != 1 || after.Best.CommitSHA != "best-applied-a" || after.Best.SourceAttemptID != iterations[2].AttemptID {
		t.Fatalf("advanced Best = %+v", after.Best)
	}
	if after.Integrations[1].Status != "stale" || after.Integrations[2].Status != "queued" {
		t.Fatalf("strict FIFO did not block behind stale head: %+v", after.Integrations)
	}
	staleAttempt := attemptByID(t, after, after.Integrations[1].AttemptID)
	if staleAttempt.Status != "iterating" || staleAttempt.CurrentIterationRound != 2 || staleAttempt.BaseSHA != "best-applied-a" {
		t.Fatalf("refreshed stale attempt = %+v", staleAttempt)
	}
	blockedAttempt := attemptByID(t, after, after.Integrations[2].AttemptID)
	if blockedAttempt.Status != "awaiting_integration" || blockedAttempt.CurrentIterationRound != 1 {
		t.Fatalf("later FIFO candidate bypassed stale head: %+v", blockedAttempt)
	}
	if got := len(pendingWorksByRole(after, symphony.RoleIteration)); got != 4 {
		t.Fatalf("active iteration concurrency = %d, want 4", got)
	}
	var refreshedWork symphony.WorkView
	for _, work := range pendingWorksByRole(after, symphony.RoleIteration) {
		if work.AttemptID == staleAttempt.ID && work.IterationRound == 2 {
			refreshedWork = work
		}
	}
	if refreshedWork.ID == "" {
		t.Fatalf("refreshed Work not found: %+v", after.Works)
	}
	queuePosition := after.Integrations[1].FIFOPosition
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "finish-refreshed"}, WorkID: refreshedWork.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-b2", Summary: "remeasured on new Best", Evidence: validBenchmarkEvidence(),
	}); err != nil {
		t.Fatalf("finish refreshed iteration: %v", err)
	}
	requeued, _ := engine.Inspect(ctx, symphony.Status{})
	if len(requeued.Integrations) != 4 || requeued.Integrations[1].Status != "refreshed" || requeued.Integrations[2].Status != "queued" {
		t.Fatalf("refreshed candidate did not retain FIFO identity: %+v", requeued.Integrations)
	}
	refreshedIntegration := requeued.Integrations[3]
	if refreshedIntegration.FIFOPosition != queuePosition || refreshedIntegration.Status != "running" || refreshedIntegration.IterationRound != 2 {
		t.Fatalf("refreshed candidate lost FIFO position: %+v", requeued.Integrations)
	}
}

func TestCancelIterationIsIsolatedAndRefillsCapacity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	before, _ := engine.Inspect(ctx, symphony.Status{})
	works := pendingWorksByRole(before, symphony.RoleIteration)
	cancelled := works[1]
	if _, err := engine.Apply(ctx, symphony.CancelWork{
		Meta: symphony.CommandMeta{RequestID: "cancel-one-attempt"}, WorkID: cancelled.ID,
	}); err != nil {
		t.Fatalf("cancel iteration: %v", err)
	}
	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect after cancellation: %v", err)
	}
	if after.Optimization.Status != symphony.OptimizationOptimizing || after.Best == nil || after.Best.Sequence != 0 {
		t.Fatalf("cancellation changed Optimization or Best: %+v %+v", after.Optimization, after.Best)
	}
	if attempt := attemptByID(t, after, cancelled.AttemptID); attempt.Status != "cancelled" {
		t.Fatalf("cancelled attempt = %+v", attempt)
	}
	for _, sibling := range works {
		if sibling.ID == cancelled.ID {
			continue
		}
		if got := workByID(t, after, sibling.ID); got.Status != symphony.WorkPending {
			t.Fatalf("sibling work changed: %+v", got)
		}
	}
	if got := len(pendingWorksByRole(after, symphony.RoleIteration)); got != 4 {
		t.Fatalf("iteration capacity after cancellation = %d, want 4", got)
	}
}

func TestIntegrationBackOffPreservesRoundAndQueuesRefresh(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	before, _ := engine.Inspect(ctx, symphony.Status{})
	iteration := pendingWorksByRole(before, symphony.RoleIteration)[0]
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "candidate"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-sha", Summary: "faster", Evidence: validBenchmarkEvidence(),
	}); err != nil {
		t.Fatalf("finish iteration: %v", err)
	}
	queued, _ := engine.Inspect(ctx, symphony.Status{})
	integration := pendingWorkByRole(t, queued, symphony.RoleIntegration)
	message := "rework the memory-layout choice"
	if _, err := engine.Apply(ctx, symphony.BackOff{
		Meta: symphony.CommandMeta{RequestID: "back-off-integration"}, WorkID: integration.ID, Message: message,
	}); err != nil {
		t.Fatalf("back off integration: %v", err)
	}
	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect backed-off integration: %v", err)
	}
	if after.Optimization.Status != symphony.OptimizationOptimizing || after.Best == nil || after.Best.Sequence != 0 {
		t.Fatalf("back-off changed Optimization or Best: %+v %+v", after.Optimization, after.Best)
	}
	attempt := attemptByID(t, after, integration.AttemptID)
	if attempt.Status != "iterating" || attempt.CurrentIterationRound != 2 || attempt.BaseSHA != "baseline-sha" {
		t.Fatalf("backed-off attempt = %+v", attempt)
	}
	round := roundByAttemptAndNumber(t, after, integration.AttemptID, 2)
	if round.Kind != "user_back_off" || round.Status != "running" || round.BackOffMessage != message {
		t.Fatalf("back-off round = %+v", round)
	}
	if after.Integrations[0].Status != "backed_off" {
		t.Fatalf("integration history = %+v", after.Integrations)
	}
}

func acceptedOptimization(t *testing.T, ctx context.Context) *symphony.Engine {
	t.Helper()
	engine := openSubmittedBaseline(t, ctx)
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatalf("inspect submitted baseline: %v", err)
	}
	work := pendingWorkByRole(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta: symphony.CommandMeta{RequestID: "accept-baseline"}, WorkID: work.ID,
		Decision: symphony.VerificationAccepted, Evidence: validBenchmarkEvidence(), InitialBestSHA: "baseline-sha",
	}); err != nil {
		t.Fatalf("accept baseline: %v", err)
	}
	return engine
}

func attemptByID(t *testing.T, view symphony.View, id string) symphony.AttemptView {
	t.Helper()
	for _, attempt := range view.Attempts {
		if attempt.ID == id {
			return attempt
		}
	}
	t.Fatalf("attempt %q not found: %+v", id, view.Attempts)
	return symphony.AttemptView{}
}

func workByID(t *testing.T, view symphony.View, id string) symphony.WorkView {
	t.Helper()
	for _, work := range view.Works {
		if work.ID == id {
			return work
		}
	}
	t.Fatalf("work %q not found", id)
	return symphony.WorkView{}
}

func roundByAttemptAndNumber(t *testing.T, view symphony.View, attemptID string, number int64) symphony.IterationRoundView {
	t.Helper()
	for _, round := range view.IterationRounds {
		if round.AttemptID == attemptID && round.Round == number {
			return round
		}
	}
	t.Fatalf("round %s/%d not found: %+v", attemptID, number, view.IterationRounds)
	return symphony.IterationRoundView{}
}

func pendingWorkByRole(t *testing.T, view symphony.View, role symphony.WorkRole) symphony.WorkView {
	t.Helper()
	works := pendingWorksByRole(view, role)
	if len(works) != 1 {
		t.Fatalf("pending %s works = %+v", role, works)
	}
	return works[0]
}

func pendingWorksByRole(view symphony.View, role symphony.WorkRole) []symphony.WorkView {
	var works []symphony.WorkView
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			works = append(works, work)
		}
	}
	return works
}
