package symphony_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
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

func TestLegacyBaselineRevisionRemainsGrandfathered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, path, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE baseline_revisions SET measurement_contract_version = NULL`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = symphony.Open(ctx, path, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Baseline.MeasurementContractVersion != 0 {
		t.Fatalf("legacy contract version = %d", view.Baseline.MeasurementContractVersion)
	}
	legacyOnly := json.RawMessage(`{"target":"kernel","candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"target.txt","kind":"unit_test"}]},"benchmark_integrity":{"schema_version":1,"case_ids":["case-1"],"benchmark_repeats":1,"warmup_invocations_per_repeat":0,"measured_invocations_per_repeat":1,"canonical_inputs_device_resident":true,"canonical_inputs_candidate_visible":false,"oracle_outputs_device_resident":true,"oracle_outputs_immutable":true,"working_tensor_addresses_stable":true,"restore_inputs_before_every_invocation":true,"check_outputs_after_every_invocation":true,"device_side_validation":true,"deferred_compact_host_transfer":true,"kernel_timing_excludes_integrity":true,"end_to_end_timing_includes_integrity":true}}`)
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "legacy-submit"}, WorkID: view.Works[0].ID, Definition: legacyOnly}); err != nil {
		t.Fatalf("grandfathered Definition rejected: %v", err)
	}
}

func TestRuntimeWorkLoadsAcceptedLegacyDefinitionWithoutCandidatePolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, databasePath := acceptedOptimizationWithCases(t, ctx, []string{"case-1"}, 1)
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(benchmarkDefinition([]string{"case-1"}), &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "candidate_change_policy")
	legacyDefinition, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE baseline_revisions SET definition_json = ?`, legacyDefinition); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	iteration := pendingWorkByRole(t, view, symphony.RoleIteration)
	runtimeWork, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeWork.CandidateChangePolicy != nil {
		t.Fatalf("legacy RuntimeWork parsed a candidate policy: %+v", runtimeWork.CandidateChangePolicy)
	}
}

func TestTerminalTransactionsPersistNormalizedMeasurements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine := acceptedOptimization(t, ctx)
	records, err := engine.WorkbenchRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Metrics) != 1 || len(records.CaseWeights) != 1 || len(records.MeasurementSets) != 1 || len(records.CaseValues) != 1 {
		t.Fatalf("Development Baseline records = %+v", records)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	iteration := pendingWorksByRole(view, symphony.RoleIteration)[0]
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "candidate"}, WorkID: iteration.ID, Outcome: symphony.IterationCandidate, CandidateSHA: "candidate-sha", Summary: "candidate", Evidence: validBenchmarkEvidence()}); err != nil {
		t.Fatal(err)
	}
	view, err = engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	integration := pendingWorkByRole(t, view, symphony.RoleIntegration)
	if _, err := engine.Apply(ctx, symphony.PrepareBestUpdate{Meta: symphony.CommandMeta{RequestID: "prepare"}, WorkID: integration.ID, Validation: validIntegrationValidation()}); err != nil {
		t.Fatal(err)
	}
	records, err = engine.WorkbenchRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records.MeasurementSets) != 3 || len(records.CaseValues) != 3 || len(records.Comparisons) != 2 {
		t.Fatalf("Integration measurement records = %+v", records)
	}
}

func TestNewBaselineRevisionRequiresMeasurementContract(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, before := draftState(t, ctx)
	if before.Baseline.MeasurementContractVersion != 1 {
		t.Fatalf("measurement contract version = %d, want 1", before.Baseline.MeasurementContractVersion)
	}
	legacyOnly := json.RawMessage(`{"target":"kernel","candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"target.txt","kind":"unit_test"}]},"benchmark_integrity":{"schema_version":1,"case_ids":["case-1"],"benchmark_repeats":1,"warmup_invocations_per_repeat":0,"measured_invocations_per_repeat":1,"canonical_inputs_device_resident":true,"canonical_inputs_candidate_visible":false,"oracle_outputs_device_resident":true,"oracle_outputs_immutable":true,"working_tensor_addresses_stable":true,"restore_inputs_before_every_invocation":true,"check_outputs_after_every_invocation":true,"device_side_validation":true,"deferred_compact_host_transfer":true,"kernel_timing_excludes_integrity":true,"end_to_end_timing_includes_integrity":true}}`)
	_, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "missing-measurements"}, WorkID: before.Works[0].ID, Definition: legacyOnly,
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != before.Optimization.Revision {
		t.Fatalf("invalid measurement contract advanced revision from %d to %d", before.Optimization.Revision, after.Optimization.Revision)
	}
}

func TestNewBaselineRevisionRequiresCanonicalCandidateChangePolicy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, before := draftState(t, ctx)
	missingPolicy := strings.Replace(string(validBaselineDefinition()), `"candidate_change_policy":{"schema_version":1,"protected_validation_paths":[{"path":"target.txt","kind":"unit_test"}]},`, "", 1)
	_, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "missing-candidate-policy"}, WorkID: before.Works[0].ID, Definition: json.RawMessage(missingPolicy),
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	if err == nil || !strings.Contains(err.Error(), "candidate_change_policy") {
		t.Fatalf("missing candidate policy error = %v", err)
	}
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != before.Optimization.Revision || after.Works[0].Status != symphony.WorkPending {
		t.Fatalf("missing candidate policy mutated state: before=%+v after=%+v", before, after)
	}
}

func TestNewBaselineRevisionRejectsHistoricalAllowedCandidateSurface(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	engine, before := draftState(t, ctx)
	legacyPolicy := strings.Replace(string(validBaselineDefinition()), "{", `{"optimization_contract":{"allowed_candidate_surface":["kernel.cu"]},`, 1)
	_, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta:       symphony.CommandMeta{RequestID: "historical-allowlist"},
		WorkID:     before.Works[0].ID,
		Definition: json.RawMessage(legacyPolicy),
	})
	assertDomainCode(t, err, symphony.CodeInvalidCommand)
	after, inspectErr := engine.Inspect(ctx, symphony.Status{})
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Optimization.Revision != before.Optimization.Revision || after.Baseline.Status != symphony.BaselineDrafting || after.Works[0].Status != symphony.WorkPending {
		t.Fatalf("historical candidate surface mutated state: before=%+v after=%+v", before, after)
	}
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

func TestPrepareBestUpdateRejectsComputedTenXWithoutIndependentRetest(t *testing.T) {
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
	withoutRetest := strings.Replace(string(validIntegrationValidation()), `"candidate":{"latency":100}`, `"candidate":{"latency":11}`, 1)
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
