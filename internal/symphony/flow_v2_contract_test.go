package symphony

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
)

func TestFlowV1ResumesWithoutDiagnosisOrExperimentLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "v1", Repository: "/repo", IterationConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "accept"}, WorkID: verification.ID,
		Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: "legacy-best"}); err != nil {
		t.Fatal(err)
	}
	beforeClose, _ := engine.Inspect(ctx, Status{})
	if beforeClose.Optimization.FlowVersion != FlowVersion1 || len(beforeClose.Diagnoses) != 0 || len(beforeClose.IterationExperiments) != 0 {
		t.Fatalf("flow-v1 gained v2 state: %+v", beforeClose)
	}
	iteration := pendingWorkForRole(t, beforeClose, RoleIteration)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatalf("reopen flow-v1 workspace: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := reopened.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Optimization.FlowVersion != FlowVersion1 || pendingWorkForRole(t, after, RoleIteration).ID != iteration.ID || len(after.Diagnoses) != 0 || after.SkillSnapshot != nil {
		t.Fatalf("flow-v1 resume projection changed: %+v", after)
	}
}

func TestFlowV2LifecycleRequiresDiagnosisAndLatestKeptExperiment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "v2", Repository: "/repo",
		FlowVersion: FlowVersion2, SkillSnapshot: ptrSnapshot(testSkillSnapshot(t)), IterationConcurrency: 1,
		IterationHistoryLimit: 0, IterationHistoryLimitSet: true}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "accept"}, WorkID: verification.ID,
		Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if got := pendingWorksForRole(view, RoleIteration); len(got) != 0 {
		t.Fatalf("Iterations started before Diagnosis: %+v", got)
	}
	diagnosisWork := pendingWorkForRole(t, view, RoleDiagnosis)
	diagnosisArtifact := contractArtifact("diagnosis/profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "finish-diagnosis"}, WorkID: diagnosisWork.ID,
		Outcome: DiagnosisReady, Report: validDiagnosisReport(view.Baseline.ID, baseSHA, diagnosisArtifact.RelativePath), Artifacts: []ArtifactInput{diagnosisArtifact}}); err != nil {
		t.Fatalf("finish Diagnosis: %v", err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if len(view.Diagnoses) != 1 || view.Diagnoses[0].Status != DiagnosisReady {
		t.Fatalf("Diagnosis was not recorded: %+v", view.Diagnoses)
	}
	if view.Diagnoses[0].HypothesisCount == 0 || view.Knowledge.Provisional == 0 {
		t.Fatalf("Diagnosis status/knowledge summary = diagnoses=%+v knowledge=%+v", view.Diagnoses, view.Knowledge)
	}
	iteration := pendingWorkForRole(t, view, RoleIteration)
	firstCheckpoint := strings.Repeat("b", 40)
	negativeArtifact := contractArtifact("experiments/negative.json")
	negative := iterationExperiment(baseSHA, "", "rejected", negativeArtifact.RelativePath)
	extraArtifact := contractArtifact("experiments/extra.json")
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "extra-artifact"}, WorkID: iteration.ID, Experiment: negative, Artifacts: []ArtifactInput{negativeArtifact, extraArtifact}}); err == nil || !strings.Contains(err.Error(), "exactly match") {
		t.Fatalf("Experiment accepted extra submitted artifact metadata: %v", err)
	}
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "negative"}, WorkID: iteration.ID, Experiment: negative, Artifacts: []ArtifactInput{negativeArtifact}}); err != nil {
		t.Fatalf("record negative experiment: %v", err)
	}
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO evidence_artifacts
		(id, work_id, receipt_id, relative_path, byte_size, content_sha256, contract_version, created_at)
		VALUES ('duplicate-artifact', ?, NULL, ?, 2, ?, 1, '2026-01-01T00:00:00Z')`, iteration.ID, negativeArtifact.RelativePath, negativeArtifact.ContentSHA256); err == nil {
		t.Fatal("database accepted a duplicate Work artifact identity")
	}
	firstArtifact := contractArtifact("experiments/kept-one.json")
	first := iterationExperiment(baseSHA, firstCheckpoint, "kept", firstArtifact.RelativePath)
	firstReceipt, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "kept-one"}, WorkID: iteration.ID, Experiment: first, Artifacts: []ArtifactInput{firstArtifact}})
	if err != nil {
		t.Fatalf("record first kept experiment: %v", err)
	}
	firstID := receiptExperimentID(t, firstReceipt)
	secondCheckpoint := strings.Repeat("c", 40)
	secondArtifact := contractArtifact("experiments/kept-two.json")
	second := iterationExperiment(firstCheckpoint, secondCheckpoint, "kept", secondArtifact.RelativePath)
	secondReceipt, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "kept-two"}, WorkID: iteration.ID, Experiment: second, Artifacts: []ArtifactInput{secondArtifact}})
	if err != nil {
		t.Fatalf("record second kept experiment: %v", err)
	}
	secondID := receiptExperimentID(t, secondReceipt)
	runtime, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.CurrentCheckpointSHA != secondCheckpoint || len(runtime.IterationExperiments) != 3 || runtime.IterationExperiments[0].Outcome != "rejected" || runtime.IterationExperiments[1].ID != firstID || runtime.IterationExperiments[2].ID != secondID {
		t.Fatalf("ordered Experiment ledger/checkpoint = %+v checkpoint=%s", runtime.IterationExperiments, runtime.CurrentCheckpointSHA)
	}
	replayed, found, err := engine.ReplayIterationExperiment(ctx, iteration.ID, "kept-one", first)
	if err != nil || !found || replayed.ID != firstReceipt.ID || !replayed.Replayed {
		t.Fatalf("durable old Experiment replay = %+v, found=%v, err=%v", replayed, found, err)
	}
	changedFirst := bytes.Replace(first, []byte(`"summary":"kept result"`), []byte(`"summary":"different result"`), 1)
	if _, _, err := engine.ReplayIterationExperiment(ctx, iteration.ID, "kept-one", changedFirst); err == nil || !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("different Experiment input reused old key: %v", err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if view.Knowledge.Negative != 1 || view.Knowledge.Provisional < 2 {
		t.Fatalf("Experiment knowledge summary = %+v", view.Knowledge)
	}
	if _, err := engine.Apply(ctx, FinishIteration{Meta: CommandMeta{RequestID: "wrong-experiment"}, WorkID: iteration.ID,
		Outcome: IterationCandidate, ExperimentID: firstID, CandidateSHA: firstCheckpoint, Summary: "stale candidate", Evidence: candidateEvidence()}); err == nil {
		t.Fatal("flow-v2 accepted Candidate for a non-latest kept Experiment")
	}
	if _, err := engine.Apply(ctx, FinishIteration{Meta: CommandMeta{RequestID: "candidate"}, WorkID: iteration.ID,
		Outcome: IterationCandidate, ExperimentID: secondID, CandidateSHA: secondCheckpoint, Summary: "latest kept candidate", Evidence: candidateEvidence()}); err != nil {
		t.Fatalf("finish flow-v2 candidate: %v", err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	integration := pendingWorkForRole(t, view, RoleIntegration)
	if _, err := engine.Apply(ctx, PrepareBestUpdate{Meta: CommandMeta{RequestID: "prepare"}, WorkID: integration.ID, Validation: testcontract.Validation()}); err != nil {
		t.Fatalf("prepare Integration: %v", err)
	}
	if _, err := engine.Apply(ctx, FinishIntegration{Meta: CommandMeta{RequestID: "accept-integration"}, WorkID: integration.ID,
		Outcome: IntegrationAccepted, ObservedBestSHA: baseSHA, AppliedSHA: secondCheckpoint, Result: json.RawMessage(`{"verified":true}`)}); err != nil {
		t.Fatalf("accept Integration: %v", err)
	}
	preservedRuntime, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preservedRuntime.IterationExperiments) < 3 ||
		preservedRuntime.IterationExperiments[0].ScopeBestSHA != baseSHA ||
		preservedRuntime.IterationExperiments[0].ReceiptID == "" ||
		len(preservedRuntime.IterationExperiments[0].ArtifactIDs) != 1 ||
		preservedRuntime.IterationExperiments[1].ScopeBestSHA != baseSHA ||
		preservedRuntime.IterationExperiments[2].ScopeBestSHA != baseSHA {
		t.Fatalf("experiment scope_best_sha drifted after Best advanced: %+v", preservedRuntime.IterationExperiments)
	}
	artifacts, err := engine.readAllEvidenceArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	knowledge, err := BuildKnowledgeProjection(preservedRuntime, view, preservedRuntime.IterationExperiments, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	var negativeKnowledge *KnowledgeRecord
	for index := range knowledge {
		if knowledge[index].Source.ExperimentID == preservedRuntime.IterationExperiments[0].ID {
			negativeKnowledge = &knowledge[index]
			break
		}
	}
	if negativeKnowledge == nil || negativeKnowledge.Experiment == nil ||
		len(negativeKnowledge.Experiment.ArtifactIDs) != 1 ||
		negativeKnowledge.Experiment.ArtifactIDs[0] != preservedRuntime.IterationExperiments[0].ArtifactIDs[0] {
		t.Fatalf("no-measurements negative Knowledge omitted durable artifact ID: %+v", negativeKnowledge)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if view.Best == nil || view.Best.Sequence != 1 || view.Best.CommitSHA != secondCheckpoint {
		t.Fatalf("Integration did not advance Best from latest Experiment: %+v", view.Best)
	}

	// A zero history limit disables cross-Attempt history without dropping the
	// target's Diagnosis and ordered current-Round Experiment ledger.
	session := AgentSession{ID: "history-zero", WorkID: iteration.ID, Generation: 1, Role: RoleIteration, AgentKind: "codex", AgentName: "agent", Status: AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	projection, err := engine.ContextProjection(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.AttemptHistories) != 0 || projection.TargetWork.Diagnosis == nil || len(projection.TargetWork.IterationExperiments) != 3 || len(projection.KnowledgeExperiments) != 3 {
		t.Fatalf("history_limit=0 projection lost current facts or loaded history: %+v", projection)
	}
}

func TestFlowV2PauseResumeCancelAndRestartPreserveDiagnosisAndExperimentIdentity(t *testing.T) {
	ctx := context.Background()
	engine, databasePath, baseSHA := flowV2AtDiagnosis(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	diagnosis := pendingWorkForRole(t, view, RoleDiagnosis)
	diagnosisArtifact := contractArtifact("diagnosis/profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "finish-diagnosis"}, WorkID: diagnosis.ID,
		Outcome: DiagnosisReady, Report: validDiagnosisReport(view.Baseline.ID, baseSHA, diagnosisArtifact.RelativePath), Artifacts: []ArtifactInput{diagnosisArtifact}}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	iteration := pendingWorkForRole(t, view, RoleIteration)
	experimentArtifact := contractArtifact("experiments/negative.json")
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "negative"}, WorkID: iteration.ID,
		Experiment: iterationExperiment(baseSHA, "", "rejected", experimentArtifact.RelativePath), Artifacts: []ArtifactInput{experimentArtifact}}); err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil || len(runtime.IterationExperiments) != 1 {
		t.Fatalf("recorded Experiment = %+v, err=%v", runtime.IterationExperiments, err)
	}
	experimentID := runtime.IterationExperiments[0].ID
	if _, err := engine.Apply(ctx, PauseScheduler{Meta: CommandMeta{RequestID: "pause"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, ResumeScheduler{Meta: CommandMeta{RequestID: "resume"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, CancelWork{Meta: CommandMeta{RequestID: "cancel"}, WorkID: iteration.ID}); err != nil {
		t.Fatal(err)
	}
	afterCancel, _ := engine.Inspect(ctx, Status{})
	if afterCancel.Optimization.Status != OptimizationOptimizing || len(afterCancel.Diagnoses) != 1 || afterCancel.Diagnoses[0].Status != DiagnosisReady || len(afterCancel.IterationExperiments) != 1 || afterCancel.IterationExperiments[0].ID != experimentID {
		t.Fatalf("pause/resume/cancel changed v2 durable identity: %+v", afterCancel)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatalf("restart flow-v2 Engine: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered, err := reopened.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Scheduler.Status != SchedulerRunning || len(recovered.Diagnoses) != 1 || recovered.Diagnoses[0].Status != DiagnosisReady || len(recovered.IterationExperiments) != 1 || recovered.IterationExperiments[0].ID != experimentID {
		t.Fatalf("restart did not preserve v2 state: %+v", recovered)
	}
}

func TestFlowV2NegativeExperimentWithFullMeasurementsPersistsDerivedEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _, baseSHA := flowV2AtDiagnosis(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	diagnosisWork := pendingWorkForRole(t, view, RoleDiagnosis)
	diagnosisArtifact := contractArtifact("diagnosis/profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "finish-diagnosis"}, WorkID: diagnosisWork.ID,
		Outcome: DiagnosisReady, Report: validDiagnosisReport(view.Baseline.ID, baseSHA, diagnosisArtifact.RelativePath), Artifacts: []ArtifactInput{diagnosisArtifact}}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	iteration := pendingWorkForRole(t, view, RoleIteration)
	experimentArtifact := contractArtifact("experiments/negative-full.json")
	negativeFull := map[string]any{
		"schema_version":        1,
		"parent_checkpoint_sha": baseSHA,
		"hypothesis": map[string]any{
			"diagnosis_hypothesis_id": "hypothesis-1",
			"summary":                 "fuse launch boundaries",
		},
		"change": map[string]any{
			"summary":   "measure and reject",
			"paths":     []string{"target.txt"},
			"mechanism": "regressed",
		},
		"outcome": "rejected",
		"benchmark_measurements": map[string]any{
			"schema_version": 1,
			"comparisons": []any{
				map[string]any{"case_id": "case-1", "reference": map[string]any{"latency": 10.0}, "candidate": map[string]any{"latency": 12.0}},
			},
		},
		"artifacts": []any{map[string]any{"path": experimentArtifact.RelativePath, "kind": "benchmark"}},
		"summary":   "negative but fully measured",
	}
	raw, _ := json.Marshal(negativeFull)
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "negative-full"}, WorkID: iteration.ID, Experiment: raw, Artifacts: []ArtifactInput{experimentArtifact}}); err != nil {
		t.Fatalf("record negative full experiment: %v", err)
	}
	runtime, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.CurrentCheckpointSHA != baseSHA || len(runtime.IterationExperiments) != 1 {
		t.Fatalf("negative full experiment changed checkpoint or ledger size: checkpoint=%s experiments=%+v", runtime.CurrentCheckpointSHA, runtime.IterationExperiments)
	}
	stored := runtime.IterationExperiments[0]
	if stored.ScopeBestSHA != baseSHA || stored.ReceiptID == "" || len(stored.DerivedComparisons) == 0 || len(stored.ArtifactIDs) == 0 {
		t.Fatalf("negative full experiment did not persist measured evidence: %+v", stored)
	}
}

func TestExperimentScopeUsesFrozenRoundBaseAfterSiblingAdvancesBest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "scope-v2", Repository: "/repo",
		FlowVersion: FlowVersion2, SkillSnapshot: ptrSnapshot(testSkillSnapshot(t)), IterationConcurrency: 2}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "accept"}, WorkID: verification.ID,
		Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	diagnosis := pendingWorkForRole(t, view, RoleDiagnosis)
	diagnosisArtifact := contractArtifact("diagnosis/profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "diagnosis"}, WorkID: diagnosis.ID, Outcome: DiagnosisReady,
		Report: validDiagnosisReport(view.Baseline.ID, baseSHA, diagnosisArtifact.RelativePath), Artifacts: []ArtifactInput{diagnosisArtifact}}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	iterations := pendingWorksForRole(view, RoleIteration)
	if len(iterations) != 2 {
		t.Fatalf("iteration works = %+v", iterations)
	}
	artifact := contractArtifact("experiments/result.json")
	advancedSHA := strings.Repeat("b", 40)
	keptReceipt, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "kept"}, WorkID: iterations[0].ID,
		Experiment: iterationExperiment(baseSHA, advancedSHA, "kept", artifact.RelativePath), Artifacts: []ArtifactInput{artifact}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, FinishIteration{Meta: CommandMeta{RequestID: "candidate"}, WorkID: iterations[0].ID,
		Outcome: IterationCandidate, ExperimentID: receiptExperimentID(t, keptReceipt), CandidateSHA: advancedSHA, Summary: "candidate", Evidence: candidateEvidence()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	integration := pendingWorkForRole(t, view, RoleIntegration)
	if _, err := engine.Apply(ctx, PrepareBestUpdate{Meta: CommandMeta{RequestID: "prepare"}, WorkID: integration.ID, Validation: testcontract.Validation()}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, FinishIntegration{Meta: CommandMeta{RequestID: "integrate"}, WorkID: integration.ID,
		Outcome: IntegrationAccepted, ObservedBestSHA: baseSHA, AppliedSHA: advancedSHA, Result: json.RawMessage(`{"verified":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, RecordIterationExperiment{Meta: CommandMeta{RequestID: "old-round-negative"}, WorkID: iterations[1].ID,
		Experiment: iterationExperiment(baseSHA, "", "rejected", artifact.RelativePath), Artifacts: []ArtifactInput{artifact}}); err != nil {
		t.Fatal(err)
	}
	oldRound, err := engine.RuntimeWork(ctx, iterations[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRound.IterationExperiments) != 1 || oldRound.IterationExperiments[0].ScopeBestSHA != baseSHA {
		t.Fatalf("old sibling Experiment scope drifted to current Best: %+v", oldRound.IterationExperiments)
	}
}

func TestFinishDiagnosisRejectsInvalidEnvironmentWithoutAdvancingState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, _, baseSHA := flowV2AtDiagnosis(t, ctx)
	before, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	diagnosis := pendingWorkForRole(t, before, RoleDiagnosis)
	artifact := contractArtifact("diagnosis/profile.json")
	valid := validDiagnosisReport(before.Baseline.ID, baseSHA, artifact.RelativePath)
	for name, report := range map[string]json.RawMessage{
		"missing hardware": mutateDiagnosisSubject(t, valid, func(subject map[string]any) { delete(subject, "hardware") }),
		"missing software": mutateDiagnosisSubject(t, valid, func(subject map[string]any) { delete(subject, "software") }),
		"nested hardware": mutateDiagnosisSubject(t, valid, func(subject map[string]any) {
			subject["hardware"] = map[string]any{"gpu": map[string]any{"nested": "not-a-scalar"}}
		}),
	} {
		_, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "invalid-" + name}, WorkID: diagnosis.ID,
			Outcome: DiagnosisReady, Report: report, Artifacts: []ArtifactInput{artifact}})
		assertFlowDomainCode(t, err, CodeInvalidCommand)
		after, inspectErr := engine.Inspect(ctx, Status{})
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if after.Optimization.Revision != before.Optimization.Revision ||
			after.DomainEventCount != before.DomainEventCount ||
			after.PendingEffectCount != before.PendingEffectCount ||
			len(pendingWorksForRole(after, RoleIteration)) != 0 ||
			len(pendingWorksForRole(after, RoleDiagnosis)) != 1 ||
			len(after.Diagnoses) != 1 || after.Diagnoses[0].Status != DiagnosisPending || len(after.Diagnoses[0].Report) != 0 ||
			pendingWorkForRole(t, after, RoleDiagnosis).Status != WorkPending {
			t.Fatalf("%s advanced Diagnosis state: before revision=%d events=%d effects=%d after=%+v",
				name, before.Optimization.Revision, before.DomainEventCount, before.PendingEffectCount, after)
		}
	}
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "empty-environment"}, WorkID: diagnosis.ID,
		Outcome: DiagnosisReady, Report: valid, Artifacts: []ArtifactInput{artifact}}); err != nil {
		t.Fatalf("empty environment objects should be accepted: %v", err)
	}
	after, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Diagnoses[0].Status != DiagnosisReady || len(pendingWorksForRole(after, RoleIteration)) == 0 {
		t.Fatalf("valid empty environment Diagnosis did not finish: %+v", after.Diagnoses)
	}
}

func mutateDiagnosisSubject(t *testing.T, report json.RawMessage, mutate func(map[string]any)) json.RawMessage {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(report, &parsed); err != nil {
		t.Fatal(err)
	}
	subject, _ := parsed["subject"].(map[string]any)
	if subject == nil {
		t.Fatal("Diagnosis report missing subject")
	}
	mutate(subject)
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestFlowV2DrainLetsDiagnosisFinishWithoutSchedulingIteration(t *testing.T) {
	ctx := context.Background()
	engine, _, baseSHA := flowV2AtDiagnosis(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	diagnosis := pendingWorkForRole(t, view, RoleDiagnosis)
	if _, err := engine.Apply(ctx, RequestShutdown{Meta: CommandMeta{RequestID: "drain"}}); err != nil {
		t.Fatal(err)
	}
	diagnosisArtifact := contractArtifact("diagnosis/drain-profile.json")
	if _, err := engine.Apply(ctx, FinishDiagnosis{Meta: CommandMeta{RequestID: "finish-diagnosis"}, WorkID: diagnosis.ID,
		Outcome: DiagnosisReady, Report: validDiagnosisReport(view.Baseline.ID, baseSHA, diagnosisArtifact.RelativePath), Artifacts: []ArtifactInput{diagnosisArtifact}}); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Inspect(ctx, Status{})
	if after.Optimization.Status != OptimizationDraining || len(pendingWorksForRole(after, RoleIteration)) != 0 || len(after.Diagnoses) != 1 || after.Diagnoses[0].Status != DiagnosisReady {
		t.Fatalf("draining Diagnosis scheduled a successor or lost state: %+v", after)
	}
}

func TestFlowV2CancellingDiagnosisPausesForOperatorIntervention(t *testing.T) {
	ctx := context.Background()
	engine, _, _ := flowV2AtDiagnosis(t, ctx)
	view, _ := engine.Inspect(ctx, Status{})
	diagnosis := pendingWorkForRole(t, view, RoleDiagnosis)
	if _, err := engine.Apply(ctx, CancelWork{Meta: CommandMeta{RequestID: "cancel-diagnosis"}, WorkID: diagnosis.ID}); err != nil {
		t.Fatal(err)
	}
	after, _ := engine.Inspect(ctx, Status{})
	if after.Optimization.Status != OptimizationPaused || len(after.Diagnoses) != 1 || after.Diagnoses[0].Status != DiagnosisCancelled || len(pendingWorksForRole(after, RoleIteration)) != 0 {
		t.Fatalf("cancelled Diagnosis did not pause without fabricated Iteration state: %+v", after)
	}
}

func flowV2AtDiagnosis(t *testing.T, ctx context.Context) (*Engine, string, string) {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := Open(ctx, databasePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	snapshot := testSkillSnapshot(t)
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "v2", Repository: "/repo", FlowVersion: FlowVersion2, SkillSnapshot: &snapshot, IterationConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "accept"}, WorkID: verification.ID, Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	return engine, databasePath, baseSHA
}

func ptrSnapshot(value SkillSnapshotInput) *SkillSnapshotInput { return &value }

func pendingWorkForRole(t *testing.T, view View, role WorkRole) WorkView {
	t.Helper()
	works := pendingWorksForRole(view, role)
	if len(works) != 1 {
		t.Fatalf("pending %s works = %+v", role, works)
	}
	return works[0]
}

func pendingWorksForRole(view View, role WorkRole) []WorkView {
	var works []WorkView
	for _, work := range view.Works {
		if work.Role == role && work.Status == WorkPending {
			works = append(works, work)
		}
	}
	return works
}

func contractArtifact(path string) ArtifactInput {
	return ArtifactInput{RelativePath: path, ByteSize: 2, ContentSHA256: strings.Repeat("d", 64), ContractVersion: 1}
}

func validDiagnosisReport(baselineID, bestSHA, artifactPath string) json.RawMessage {
	return json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"` + baselineID + `","best_sha":"` + bestSHA + `","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"` + artifactPath + `","kind":"profile"}],"observations":[{"id":"observation-1","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["` + artifactPath + `"],"summary":"profile shows launch overhead"}],"bottlenecks":[{"id":"bottleneck-1","class":"launch-overhead","confidence":"high","observation_ids":["observation-1"],"summary":"launch overhead dominates"}],"hypotheses":[{"id":"hypothesis-1","rank":1,"summary":"fuse launch boundaries","mechanism":"remove launches","bottleneck_ids":["bottleneck-1"],"target_case_ids":["case-1"],"expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)
}

func iterationExperiment(parent, checkpoint, outcome, artifactPath string) json.RawMessage {
	var evidence struct {
		BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity"`
	}
	_ = json.Unmarshal(testcontract.Evidence(), &evidence)
	value := map[string]any{
		"schema_version": 1, "parent_checkpoint_sha": parent,
		"hypothesis": map[string]any{"diagnosis_hypothesis_id": "hypothesis-1", "summary": "fuse launch boundaries"},
		"change":     map[string]any{"summary": "fuse two launches", "paths": []string{"target.txt"}, "mechanism": "fusion"},
		"outcome":    outcome, "summary": outcome + " result",
		"artifacts": []any{map[string]any{"path": artifactPath, "kind": "benchmark"}},
	}
	if outcome == "kept" {
		value["checkpoint_sha"] = checkpoint
		value["correctness"] = map[string]any{"benchmark_integrity": evidence.BenchmarkIntegrity}
		value["benchmark_measurements"] = map[string]any{
			"schema_version": 1,
			"comparisons": []any{
				map[string]any{"case_id": "case-1", "reference": map[string]any{"latency": 10.0}, "candidate": map[string]any{"latency": 9.0}},
			},
		}
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func candidateEvidence() json.RawMessage {
	var evidence struct {
		BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity"`
	}
	_ = json.Unmarshal(testcontract.Evidence(), &evidence)
	return json.RawMessage(`{"benchmark_integrity":` + string(evidence.BenchmarkIntegrity) + `}`)
}

func receiptExperimentID(t *testing.T, receipt Receipt) string {
	t.Helper()
	var value struct {
		ExperimentID string `json:"experiment_id"`
	}
	if err := json.Unmarshal(receipt.Result, &value); err != nil || value.ExperimentID == "" {
		t.Fatalf("Experiment receipt = %s, err=%v", receipt.Result, err)
	}
	return value.ExperimentID
}
