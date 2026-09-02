package symphony

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
)

func TestFlowV3RequiresFreshBenchmarkBeforeEveryExperiment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, baseSHA := flowV3AtBenchmark(t, ctx)

	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	firstBenchmark := pendingWorkForRole(t, view, RoleBenchmark)
	if len(pendingWorksForRole(view, RoleIteration)) != 0 || firstBenchmark.ExperimentCycleID == "" || len(view.ExperimentCycles) != 1 {
		t.Fatalf("flow v3 did not begin with Benchmark Work: %+v", view)
	}
	measurements, _ := json.Marshal(map[string]any{"schema_version": 1, "cases": []any{map[string]any{"case_id": "case-1", "values": map[string]any{"latency": 10}}}})
	if _, err := engine.Apply(ctx, FinishIterationBenchmark{Meta: CommandMeta{RequestID: "missing-environment"}, WorkID: firstBenchmark.ID,
		Outcome: BenchmarkMeasured, Measurements: measurements, Provider: "codex", Model: "test", Artifacts: []ArtifactInput{contractArtifact("reference/missing-environment.json")}}); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("measured Benchmark without environment error = %v", err)
	}
	firstReference := finishFlowV3Benchmark(t, ctx, engine, firstBenchmark, 10)
	if receipt, found, err := engine.WorkRoleTerminalReceipt(ctx, firstBenchmark.ID, RoleBenchmark); err != nil || !found || receipt.ID != firstReference.ID {
		t.Fatalf("Benchmark terminal receipt = %+v found=%v err=%v", receipt, found, err)
	}

	view, _ = engine.Inspect(ctx, Status{})
	firstIteration := pendingWorkForRole(t, view, RoleIteration)
	if firstIteration.ExperimentCycleID != firstBenchmark.ExperimentCycleID || firstReference.ExperimentCycleID != firstIteration.ExperimentCycleID {
		t.Fatalf("reference/Iteration Cycle identity drifted: benchmark=%+v iteration=%+v receipt=%+v", firstBenchmark, firstIteration, firstReference)
	}
	runtime, err := engine.RuntimeWork(ctx, firstIteration.ID)
	if err != nil || runtime.ReferenceReceipt == nil || runtime.ReferenceReceipt.ID != firstReference.ID {
		t.Fatalf("Iteration RuntimeWork has no scoped Reference Receipt: %+v err=%v", runtime, err)
	}

	firstCheckpoint := strings.Repeat("b", 40)
	firstArtifact := contractArtifact("experiments/first.json")
	firstExperiment := flowV3Experiment(firstReference.ID, baseSHA, firstCheckpoint, "kept", firstArtifact.RelativePath, 9)
	firstRecord, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "first-experiment"}, WorkID: firstIteration.ID, Experiment: firstExperiment, Artifacts: []ArtifactInput{firstArtifact}})
	if err != nil {
		t.Fatalf("record first flow v3 Experiment: %v", err)
	}
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "second-in-same-work"}, WorkID: firstIteration.ID, Experiment: firstExperiment, Artifacts: []ArtifactInput{firstArtifact}}); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("flow v3 accepted a second Experiment in one Work: %v", err)
	}
	if _, err := engine.Apply(ctx, StartNextExperiment{Meta: CommandMeta{RequestID: "next-cycle"}, WorkID: firstIteration.ID}); err != nil {
		t.Fatalf("start next Experiment Cycle: %v", err)
	}
	if _, found, err := engine.WorkRoleTerminalReceipt(ctx, firstIteration.ID, RoleIteration); err != nil || !found {
		t.Fatalf("start_next_experiment terminal receipt found=%v err=%v", found, err)
	}

	view, _ = engine.Inspect(ctx, Status{})
	secondBenchmark := pendingWorkForRole(t, view, RoleBenchmark)
	if secondBenchmark.ExperimentCycleID == firstBenchmark.ExperimentCycleID || len(pendingWorksForRole(view, RoleIteration)) != 0 {
		t.Fatalf("next Experiment bypassed fresh Benchmark: %+v", view.Works)
	}
	if len(view.ExperimentCycles) != 2 || view.ExperimentCycles[0].Status != "completed" || view.ReferenceReceipts[0].ConsumedExperimentID != receiptExperimentID(t, firstRecord) {
		t.Fatalf("first Cycle/Receipt was not durably completed and consumed: cycles=%+v receipts=%+v", view.ExperimentCycles, view.ReferenceReceipts)
	}
	secondReference := finishFlowV3Benchmark(t, ctx, engine, secondBenchmark, 9)
	view, _ = engine.Inspect(ctx, Status{})
	secondIteration := pendingWorkForRole(t, view, RoleIteration)
	secondCheckpoint := strings.Repeat("c", 40)
	secondArtifact := contractArtifact("experiments/second.json")
	secondRecord, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "second-experiment"}, WorkID: secondIteration.ID,
		Experiment: flowV3Experiment(secondReference.ID, firstCheckpoint, secondCheckpoint, "kept", secondArtifact.RelativePath, 8), Artifacts: []ArtifactInput{secondArtifact}})
	if err != nil {
		t.Fatalf("record second flow v3 Experiment: %v", err)
	}
	if _, err := engine.Apply(ctx, FinishIteration{Meta: CommandMeta{RequestID: "candidate"}, WorkID: secondIteration.ID,
		Outcome: IterationCandidate, ExperimentID: receiptExperimentID(t, secondRecord), CandidateSHA: secondCheckpoint,
		Summary: "candidate after two benchmark-gated Experiments", Evidence: candidateEvidence()}); err != nil {
		t.Fatalf("finish flow v3 Candidate: %v", err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if len(pendingWorksForRole(view, RoleIntegration)) != 1 || view.ExperimentCycles[1].Status != "completed" {
		t.Fatalf("flow v3 Candidate did not close Cycle and queue Integration: %+v", view)
	}
}

func TestFlowV3UnavailableBenchmarkPausesAndResumeRetriesSameCycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _ := flowV3AtBenchmark(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	benchmark := pendingWorkForRole(t, view, RoleBenchmark)
	cycleID := benchmark.ExperimentCycleID
	if _, err := engine.Apply(ctx, FinishIterationBenchmark{Meta: CommandMeta{RequestID: "unavailable"}, WorkID: benchmark.ID,
		Outcome: BenchmarkUnavailable, Reason: "GPU executor unavailable", Provider: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	paused, _ := engine.Inspect(ctx, Status{})
	if paused.Optimization.Status != OptimizationPaused || paused.Scheduler.Status != SchedulerRunning || paused.ExperimentCycles[0].Status != "benchmark_unavailable" {
		t.Fatalf("unavailable Benchmark did not pause Optimization independently of Scheduler: %+v", paused)
	}
	receipt, err := engine.Apply(ctx, ResumeScheduler{Meta: CommandMeta{RequestID: "retry"}})
	if err != nil {
		t.Fatalf("resume unavailable Benchmark: %v", err)
	}
	if receipt.Revision == paused.Optimization.Revision {
		t.Fatal("Benchmark retry was incorrectly treated as Scheduler no-op")
	}
	retried, _ := engine.Inspect(ctx, Status{})
	newBenchmark := pendingWorkForRole(t, retried, RoleBenchmark)
	if retried.Optimization.Status != OptimizationOptimizing || newBenchmark.ID == benchmark.ID || newBenchmark.ExperimentCycleID != cycleID || retried.ExperimentCycles[0].Status != "benchmark_pending" {
		t.Fatalf("resume did not retry the same Experiment Cycle: %+v", retried)
	}
	var runs, unavailableRuns int64
	if err := engine.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(status = 'unavailable') FROM benchmark_runs WHERE experiment_cycle_id = ?`, cycleID).Scan(&runs, &unavailableRuns); err != nil {
		t.Fatal(err)
	}
	if runs != 2 || unavailableRuns != 1 {
		t.Fatalf("Benchmark failure history was not preserved: runs=%d unavailable=%d", runs, unavailableRuns)
	}
}

func TestFlowV3OnlineStateSurvivesEachExperimentGateBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, baseSHA := flowV3AtBenchmark(t, ctx)
	path := engine.databasePath
	reopen := func() {
		t.Helper()
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		var err error
		engine, err = Open(ctx, path, Options{})
		if err != nil {
			t.Fatalf("reopen flow v3 boundary: %v", err)
		}
	}
	t.Cleanup(func() { _ = engine.Close() })

	reopen()
	view, _ := engine.Inspect(ctx, Status{})
	benchmark := pendingWorkForRole(t, view, RoleBenchmark)
	reference := finishFlowV3Benchmark(t, ctx, engine, benchmark, 10)
	reopen()
	view, _ = engine.Inspect(ctx, Status{})
	iteration := pendingWorkForRole(t, view, RoleIteration)
	artifact := contractArtifact("experiments/rejected.json")
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "rejected-experiment"}, WorkID: iteration.ID,
		Experiment: flowV3Experiment(reference.ID, baseSHA, "", "rejected", artifact.RelativePath, 0), Artifacts: []ArtifactInput{artifact}}); err != nil {
		t.Fatal(err)
	}
	reopen()
	if _, err := engine.Apply(ctx, StartNextExperiment{Meta: CommandMeta{RequestID: "next-after-rejected"}, WorkID: iteration.ID}); err != nil {
		t.Fatal(err)
	}
	reopen()
	view, _ = engine.Inspect(ctx, Status{})
	if len(view.ExperimentCycles) != 2 || view.ExperimentCycles[0].Status != "completed" || pendingWorkForRole(t, view, RoleBenchmark).ExperimentCycleID == benchmark.ExperimentCycleID {
		t.Fatalf("reopened flow v3 gate boundaries are inconsistent: %+v", view.ExperimentCycles)
	}
}

func TestFlowV3BenchmarkCompletionWhileDrainingDoesNotStartIteration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _ := flowV3AtBenchmark(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	benchmark := pendingWorkForRole(t, view, RoleBenchmark)
	if _, err := engine.Apply(ctx, RequestShutdown{Meta: CommandMeta{RequestID: "shutdown"}}); err != nil {
		t.Fatal(err)
	}
	finishFlowV3Benchmark(t, ctx, engine, benchmark, 10)
	view, _ = engine.Inspect(ctx, Status{})
	if view.Optimization.Status != OptimizationDraining || len(pendingWorksForRole(view, RoleIteration)) != 0 || view.ExperimentCycles[0].Status != "abandoned" {
		t.Fatalf("draining Benchmark spawned successor Work: %+v", view)
	}
	for _, work := range view.Works {
		if work.Status == WorkPending {
			t.Fatalf("draining flow v3 retained pending Work: %+v", work)
		}
	}
}

func TestFlowV3ReconfigureIterationAgentsCancelsBenchmarkSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _ := flowV3AtBenchmarkWithConcurrency(t, ctx, 2)
	if err := engine.ReconfigureIterationAgents(ctx, 1); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if view.Optimization.IterationConcurrency != 1 || len(pendingWorksForRole(view, RoleBenchmark)) != 1 {
		t.Fatalf("flow v3 Benchmark slots were not reconfigured: %+v", view)
	}
	for _, attempt := range view.Attempts {
		if attempt.SlotIndex == 1 && (attempt.Status != "cancelled" || attempt.FailureReason != "iteration_agent_removed") {
			t.Fatalf("removed Benchmark slot Attempt was not cancelled: %+v", attempt)
		}
	}
	path := engine.databasePath
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen reconfigured flow v3: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

func TestFlowV3ReconfigureRemovesUnavailableBenchmarkWithoutPermanentPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _ := flowV3AtBenchmarkWithConcurrency(t, ctx, 2)
	view, _ := engine.Inspect(ctx, Status{})
	slotByAttempt := make(map[string]int64, len(view.Attempts))
	for _, attempt := range view.Attempts {
		slotByAttempt[attempt.ID] = attempt.SlotIndex
	}
	var removedBenchmark WorkView
	for _, work := range pendingWorksForRole(view, RoleBenchmark) {
		if slotByAttempt[work.AttemptID] == 1 {
			removedBenchmark = work
			break
		}
	}
	if removedBenchmark.ID == "" {
		t.Fatal("slot 1 Benchmark Work was not found")
	}
	if _, err := engine.Apply(ctx, FinishIterationBenchmark{Meta: CommandMeta{RequestID: "unavailable-removed-slot"}, WorkID: removedBenchmark.ID,
		Outcome: BenchmarkUnavailable, Reason: "executor unavailable"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ReconfigureIterationAgents(ctx, 1); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if view.Optimization.Status != OptimizationOptimizing || view.Optimization.IterationConcurrency != 1 || len(pendingWorksForRole(view, RoleBenchmark)) != 1 {
		t.Fatalf("removed unavailable Benchmark left Optimization paused: %+v", view)
	}
	for _, cycle := range view.ExperimentCycles {
		if cycle.Status == "benchmark_unavailable" {
			t.Fatalf("removed Benchmark Cycle remained unavailable: %+v", cycle)
		}
	}
	path := engine.databasePath
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen after removing unavailable Benchmark slot: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
}

func TestFlowV3ResumeFillsSlotsAddedWhileBenchmarkUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _ := flowV3AtBenchmark(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	benchmark := pendingWorkForRole(t, view, RoleBenchmark)
	if _, err := engine.Apply(ctx, FinishIterationBenchmark{Meta: CommandMeta{RequestID: "unavailable-before-add"}, WorkID: benchmark.ID,
		Outcome: BenchmarkUnavailable, Reason: "executor unavailable"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ReconfigureIterationAgents(ctx, 2); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if view.Optimization.Status != OptimizationPaused || len(pendingWorksForRole(view, RoleBenchmark)) != 0 {
		t.Fatalf("adding slots bypassed unavailable Benchmark pause: %+v", view)
	}
	if _, err := engine.Apply(ctx, ResumeScheduler{Meta: CommandMeta{RequestID: "resume-after-add"}}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if view.Optimization.Status != OptimizationOptimizing || len(pendingWorksForRole(view, RoleBenchmark)) != 2 || len(view.Attempts) != 2 {
		t.Fatalf("Benchmark retry did not fill newly configured slot: %+v", view)
	}
}

func flowV3AtBenchmark(t *testing.T, ctx context.Context) (*Engine, string) {
	return flowV3AtBenchmarkWithConcurrency(t, ctx, 1)
}

func flowV3AtBenchmarkWithConcurrency(t *testing.T, ctx context.Context, concurrency int64) (*Engine, string) {
	t.Helper()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	baseSHA := strings.Repeat("a", 40)
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "flow-v3", Repository: "/repo",
		FlowVersion: FlowVersion3, SkillSnapshot: ptrSnapshot(testSkillSnapshot(t)), IterationConcurrency: concurrency}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "baseline"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "verify"}, WorkID: verification.ID,
		Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	diagnosis := pendingWorkForRole(t, view, RoleDiagnosis)
	artifact := contractArtifact("diagnosis/profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "diagnosis"}, WorkID: diagnosis.ID,
		Outcome: DiagnosisReady, Report: validDiagnosisReport(view.Baseline.ID, baseSHA, artifact.RelativePath), Artifacts: []ArtifactInput{artifact}}); err != nil {
		t.Fatal(err)
	}
	return engine, baseSHA
}

func finishFlowV3Benchmark(t *testing.T, ctx context.Context, engine *Engine, work WorkView, latency float64) ReferenceReceiptView {
	t.Helper()
	measurements, _ := json.Marshal(map[string]any{"schema_version": 1, "cases": []any{map[string]any{"case_id": "case-1", "values": map[string]any{"latency": latency}}}})
	artifact := contractArtifact("reference/raw.json")
	if _, err := engine.Apply(ctx, FinishIterationBenchmark{Meta: CommandMeta{RequestID: "benchmark-" + work.ID}, WorkID: work.ID,
		Outcome: BenchmarkMeasured, Measurements: measurements, Environment: json.RawMessage(`{"gpu":"test"}`), Provider: "codex", Model: "test", Artifacts: []ArtifactInput{artifact}}); err != nil {
		t.Fatalf("finish Benchmark Work: %v", err)
	}
	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range view.ReferenceReceipts {
		if receipt.ExperimentCycleID == work.ExperimentCycleID {
			return receipt
		}
	}
	t.Fatal("Reference Receipt was not created")
	return ReferenceReceiptView{}
}

func flowV3Experiment(referenceReceiptID, parent, checkpoint, outcome, artifactPath string, latency float64) json.RawMessage {
	var evidence struct {
		BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity"`
	}
	_ = json.Unmarshal(testcontract.Evidence(), &evidence)
	value := map[string]any{
		"schema_version": 2, "reference_receipt_id": referenceReceiptID, "parent_checkpoint_sha": parent,
		"hypothesis": map[string]any{"diagnosis_hypothesis_id": "hypothesis-1", "summary": "fuse launch boundaries"},
		"change":     map[string]any{"summary": "fuse two launches", "paths": []string{"target.txt"}, "mechanism": "fusion"},
		"outcome":    outcome, "summary": outcome + " result",
		"artifacts": []any{map[string]any{"path": artifactPath, "kind": "benchmark"}},
	}
	if outcome == "kept" {
		value["checkpoint_sha"] = checkpoint
		value["correctness"] = map[string]any{"benchmark_integrity": evidence.BenchmarkIntegrity}
		value["benchmark_measurements"] = map[string]any{"schema_version": 1, "cases": []any{
			map[string]any{"case_id": "case-1", "values": map[string]any{"latency": latency}},
		}}
	}
	encoded, _ := json.Marshal(value)
	return encoded
}
