package symphony_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/iterationcases"
	"github.com/reyoung/pika-go/internal/symphony"
	_ "modernc.org/sqlite"
)

func TestIterationCaseSetSeedsFreezesAndMonotonicallyLearnsRegressions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	full := caseIDs(15)
	engine, _ := acceptedOptimizationWithCases(t, ctx, full, 1)

	initial, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	wantSeed, err := iterationcases.Seed(full)
	if err != nil {
		t.Fatal(err)
	}
	if initial.IterationCaseSet == nil || initial.IterationCaseSet.Version != 1 || !slices.Equal(initial.IterationCaseSet.CaseIDs, wantSeed) {
		t.Fatalf("initial Iteration Case Set = %+v, want v1 %v", initial.IterationCaseSet, wantSeed)
	}
	iteration := pendingWorkByRole(t, initial, symphony.RoleIteration)
	frozenWork, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if frozenWork.IterationCaseSet == nil || !slices.Equal(frozenWork.IterationCaseSet.CaseIDs, wantSeed) {
		t.Fatalf("Round snapshot = %+v", frozenWork.IterationCaseSet)
	}
	incomplete := benchmarkEvidence(wantSeed[:len(wantSeed)-1])
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "incomplete"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "bad", Summary: "missing case", Evidence: incomplete}); err == nil || !strings.Contains(err.Error(), "exactly 10") {
		t.Fatalf("incomplete candidate evidence = %v, want exact coverage failure", err)
	}
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "candidate-1"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-1", Summary: "candidate", Evidence: benchmarkEvidence(wantSeed)}); err != nil {
		t.Fatal(err)
	}
	queued, _ := engine.Inspect(ctx, symphony.Status{})
	integration := pendingWorkByRole(t, queued, symphony.RoleIntegration)
	unseen := difference(full, wantSeed)
	reports := []symphony.RegressionCase{
		regression(wantSeed[0], "correctness", 100),
		regression(unseen[0], "correctness", 90),
		regression(wantSeed[1], "performance", 80),
		regression(unseen[1], "performance", 70),
		regression(unseen[2], "performance", 60),
		regression(unseen[3], "performance", 50),
	}
	if _, err := engine.Apply(ctx, symphony.FinishIntegration{Meta: symphony.CommandMeta{RequestID: "wrong-outcome-report"}, WorkID: integration.ID,
		Outcome: symphony.IntegrationAccepted, RegressionCases: reports}); err == nil || !strings.Contains(err.Error(), "allowed only") {
		t.Fatalf("accepted Integration with Regression Cases = %v", err)
	}
	receipt, err := engine.Apply(ctx, symphony.FinishIntegration{Meta: symphony.CommandMeta{RequestID: "reject-1"}, WorkID: integration.ID,
		Outcome: symphony.IntegrationRejected, Result: json.RawMessage(`{"reason":"obvious regressions"}`), RegressionCases: reports})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	wantAdded := unseen[:3]
	wantCurrent := append(append([]string(nil), wantSeed...), wantAdded...)
	if after.IterationCaseSet == nil || after.IterationCaseSet.Version != 2 || !slices.Equal(after.IterationCaseSet.CaseIDs, wantCurrent) {
		t.Fatalf("expanded Iteration Case Set = %+v, want v2 %v", after.IterationCaseSet, wantCurrent)
	}
	if len(after.Integrations[0].RegressionCases) != len(reports) {
		t.Fatalf("stored Regression Cases = %+v", after.Integrations[0].RegressionCases)
	}
	var result struct {
		Added []string `json:"added_iteration_case_ids"`
	}
	if err := json.Unmarshal(receipt.Result, &result); err != nil || !slices.Equal(result.Added, wantAdded) {
		t.Fatalf("finish receipt = %s, err=%v", receipt.Result, err)
	}
	oldRound, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRound.IterationCaseSet == nil || oldRound.IterationCaseSet.Version != 1 || !slices.Equal(oldRound.IterationCaseSet.CaseIDs, wantSeed) {
		t.Fatalf("completed Round changed after expansion: %+v", oldRound.IterationCaseSet)
	}
	newIteration := pendingWorkByRole(t, after, symphony.RoleIteration)
	newRound, err := engine.RuntimeWork(ctx, newIteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newRound.IterationCaseSet == nil || newRound.IterationCaseSet.Version != 2 || !slices.Equal(newRound.IterationCaseSet.CaseIDs, wantCurrent) {
		t.Fatalf("future Round snapshot = %+v, want v2", newRound.IterationCaseSet)
	}
	replayed, err := engine.Apply(ctx, symphony.FinishIntegration{Meta: symphony.CommandMeta{RequestID: "reject-1"}, WorkID: integration.ID,
		Outcome: symphony.IntegrationRejected, Result: json.RawMessage(`{"reason":"obvious regressions"}`), RegressionCases: reports})
	if err != nil || !replayed.Replayed {
		t.Fatalf("idempotent replay = %+v, %v", replayed, err)
	}
	replayedView, _ := engine.Inspect(ctx, symphony.Status{})
	if replayedView.IterationCaseSet.Version != 2 || len(replayedView.IterationCaseSet.CaseIDs) != 13 {
		t.Fatalf("replay grew Iteration Case Set: %+v", replayedView.IterationCaseSet)
	}
}

func TestRegressionReportIsFullyValidatedBeforeAnyExpansion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	full := caseIDs(15)
	engine, _ := acceptedOptimizationWithCases(t, ctx, full, 1)
	view, _ := engine.Inspect(ctx, symphony.Status{})
	iteration := pendingWorkByRole(t, view, symphony.RoleIteration)
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "candidate"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: "candidate", Summary: "candidate", Evidence: benchmarkEvidence(view.IterationCaseSet.CaseIDs)}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	integration := pendingWorkByRole(t, view, symphony.RoleIntegration)
	unseen := difference(full, view.IterationCaseSet.CaseIDs)
	reports := []symphony.RegressionCase{
		regression(unseen[0], "performance", 10), regression(unseen[1], "performance", 9),
		regression(unseen[2], "performance", 8), regression(unseen[3], "performance", 7),
		regression("outside-full-set", "performance", 6),
	}
	if _, err := engine.Apply(ctx, symphony.FinishIntegration{Meta: symphony.CommandMeta{RequestID: "invalid-report"}, WorkID: integration.ID,
		Outcome: symphony.IntegrationRejected, RegressionCases: reports}); err == nil || !strings.Contains(err.Error(), "Full Case Set") {
		t.Fatalf("invalid report = %v", err)
	}
	after, _ := engine.Inspect(ctx, symphony.Status{})
	if after.IterationCaseSet.Version != 1 || len(after.IterationCaseSet.CaseIDs) != 10 || pendingWorkByRole(t, after, symphony.RoleIntegration).ID != integration.ID {
		t.Fatalf("invalid report partially mutated state: set=%+v integrations=%+v", after.IterationCaseSet, after.Integrations)
	}
}

func TestLegacyOptimizingWorkspaceRequiresExplicitIterationCaseMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	full := caseIDs(3)
	engine, databasePath := acceptedOptimizationWithCases(t, ctx, full, 1)
	view, _ := engine.Inspect(ctx, symphony.Status{})
	iteration := pendingWorkByRole(t, view, symphony.RoleIteration)
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "legacy-reject"}, WorkID: iteration.ID,
		Outcome: symphony.IterationRejected, Summary: "historical rejection"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=ON;
		DELETE FROM iteration_round_cases;
		DELETE FROM iteration_cases;
		UPDATE iteration_rounds SET iteration_case_set_version = NULL;
		UPDATE optimizations SET iteration_case_set_version = NULL`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if opened, err := symphony.Open(ctx, databasePath, symphony.Options{}); err == nil || !strings.Contains(err.Error(), "migrate-iteration-cases") {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("normal open legacy workspace = %v, want migration gate", err)
	}
	migrator, err := symphony.Open(ctx, databasePath, symphony.Options{AllowIterationCaseMigration: true})
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{"case-2", "case-0", "case-1"}
	migrated, err := migrator.MigrateIterationCases(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != 1 || !slices.Equal(migrated.CaseIDs, selected) {
		t.Fatalf("migration result = %+v", migrated)
	}
	_ = migrator.Close()
	reopened, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatalf("open migrated workspace: %v", err)
	}
	defer reopened.Close()
	after, _ := reopened.Inspect(ctx, symphony.Status{})
	if after.IterationCaseSet == nil || !slices.Equal(after.IterationCaseSet.CaseIDs, selected) {
		t.Fatalf("migrated state = %+v", after.IterationCaseSet)
	}
	var sawHistorical, sawCurrent bool
	for _, round := range after.IterationRounds {
		switch round.Status {
		case "rejected":
			sawHistorical = round.IterationCaseSetVersion == 0
		case "running", "queued":
			sawCurrent = round.IterationCaseSetVersion == 1
		}
	}
	if !sawHistorical || !sawCurrent {
		t.Fatalf("migrated Round versions = %+v", after.IterationRounds)
	}
}

func acceptedOptimizationWithCases(t *testing.T, ctx context.Context, fullCaseIDs []string, concurrency int64) (*symphony.Engine, string) {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
		IterationConcurrency: concurrency, MaxPendingAttempts: 4}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID,
		Definition: benchmarkDefinition(fullCaseIDs)}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verification := pendingWorkByRole(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: verification.ID,
		Decision: symphony.VerificationAccepted, Evidence: benchmarkEvidence(fullCaseIDs), InitialBestSHA: "baseline"}); err != nil {
		t.Fatal(err)
	}
	return engine, databasePath
}

func benchmarkDefinition(caseIDs []string) json.RawMessage {
	contract := map[string]any{
		"schema_version": 1, "case_ids": caseIDs, "benchmark_repeats": 1,
		"warmup_invocations_per_repeat": 0, "measured_invocations_per_repeat": 1,
		"canonical_inputs_device_resident": true, "canonical_inputs_candidate_visible": false,
		"oracle_outputs_device_resident": true, "oracle_outputs_immutable": true,
		"working_tensor_addresses_stable": true, "restore_inputs_before_every_invocation": true,
		"check_outputs_after_every_invocation": true, "device_side_validation": true,
		"deferred_compact_host_transfer": true, "kernel_timing_excludes_integrity": true,
		"end_to_end_timing_includes_integrity": true,
	}
	contents, _ := json.Marshal(map[string]any{"target": "kernel", "benchmark_integrity": contract})
	return contents
}

func benchmarkEvidence(caseIDs []string) json.RawMessage {
	cases := make([]map[string]any, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		cases = append(cases, map[string]any{"case_id": caseID, "warmup_invocations": 0, "measured_invocations": 1,
			"input_restores": 1, "checked_invocations": 1, "mismatches": 0, "nonfinite": 0,
			"canonical_input_mutations": 0, "tolerance_passed": true})
	}
	contents, _ := json.Marshal(map[string]any{"benchmark_integrity": map[string]any{"schema_version": 1, "cases": cases}})
	return contents
}

func caseIDs(count int) []string {
	result := make([]string, 0, count)
	for index := range count {
		result = append(result, fmt.Sprintf("case-%d", index))
	}
	return result
}

func difference(full, selected []string) []string {
	seen := make(map[string]struct{}, len(selected))
	for _, caseID := range selected {
		seen[caseID] = struct{}{}
	}
	var result []string
	for _, caseID := range full {
		if _, ok := seen[caseID]; !ok {
			result = append(result, caseID)
		}
	}
	return result
}

func regression(caseID, kind string, severity int) symphony.RegressionCase {
	return symphony.RegressionCase{CaseID: caseID, Kind: kind, Summary: fmt.Sprintf("severity %d", severity),
		Evidence: json.RawMessage(fmt.Sprintf(`{"severity":%d}`, severity))}
}
