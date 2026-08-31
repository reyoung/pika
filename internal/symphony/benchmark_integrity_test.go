package symphony_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/symphony"
)

func validBaselineDefinition() json.RawMessage { return testcontract.Definition() }
func validBenchmarkEvidence() json.RawMessage  { return testcontract.Evidence() }
func validIntegrationValidation() json.RawMessage {
	return testcontract.Validation()
}

func TestBaselineDefinitionIntegrityFailureDoesNotAdvanceState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, before := draftState(t, ctx)
	_, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "invalid-integrity"},
		WorkID:     before.Works[0].ID,
		Definition: json.RawMessage(`{"target":"kernel"}`),
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != before.Optimization.Revision || after.Baseline.Status != symphony.BaselineDrafting || after.Works[0].Status != symphony.WorkPending {
		t.Fatalf("invalid Definition mutated state: before=%+v after=%+v", before, after)
	}
}

func TestAcceptedBaselineRejectsUncheckedBenchmarkInvocation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := openSubmittedBaseline(t, ctx)
	before, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := pendingWorkByRole(t, before, symphony.RoleBaselineVerification)
	unchecked := strings.Replace(string(validBenchmarkEvidence()), `"checked_invocations":1`, `"checked_invocations":0`, 1)
	_, err = engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta:     symphony.CommandMeta{RequestID: "unchecked-invocation"},
		WorkID:   work.ID,
		Decision: symphony.VerificationAccepted,
		Evidence: json.RawMessage(unchecked),
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != before.Optimization.Revision || after.Baseline.Status != symphony.BaselineVerifying || pendingWorkByRole(t, after, symphony.RoleBaselineVerification).ID != work.ID {
		t.Fatalf("invalid evidence mutated state: before=%+v after=%+v", before, after)
	}
}

func TestPrepareBestUpdateRejectsTenXWithoutIndependentRetest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	iteration := pendingWorksByRole(view, symphony.RoleIteration)[0]
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta:         symphony.CommandMeta{RequestID: "ten-x-candidate"},
		WorkID:       iteration.ID,
		Outcome:      symphony.IterationCandidate,
		CandidateSHA: "ten-x-sha",
		Summary:      "claims ten times faster",
		Evidence:     validBenchmarkEvidence(),
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	integration := pendingWorkByRole(t, queued, symphony.RoleIntegration)
	withoutRetest := strings.Replace(string(validIntegrationValidation()), `"max_case_speedup":1.1`, `"max_case_speedup":10`, 1)
	_, err = engine.Apply(ctx, symphony.PrepareBestUpdate{
		Meta:       symphony.CommandMeta{RequestID: "prepare-ten-x"},
		WorkID:     integration.ID,
		Validation: json.RawMessage(withoutRetest),
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != queued.Optimization.Revision || after.Integrations[0].Status != "running" {
		t.Fatalf("invalid 10x validation mutated state: before=%+v after=%+v", queued, after)
	}
}
