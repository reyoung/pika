package toolapp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/kdacontract"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

type checkpointChainStore struct {
	grant        symphony.AgentGrant
	runtime      symphony.RuntimeWork
	capabilities map[string]symphony.CommitCapabilityReceipt
}

func (s *checkpointChainStore) ResolveAgentGrant(context.Context, string) (symphony.AgentGrant, error) {
	return s.grant, nil
}
func (s *checkpointChainStore) Apply(_ context.Context, command symphony.Command) (symphony.Receipt, error) {
	record, ok := command.(symphony.RecordIterationExperiment)
	if !ok {
		return symphony.Receipt{}, fmt.Errorf("unexpected command %T", command)
	}
	experiment, err := kdacontract.ParseExperiment(record.Experiment)
	if err != nil {
		return symphony.Receipt{}, err
	}
	if experiment.ParentCheckpointSHA != s.runtime.CurrentCheckpointSHA {
		return symphony.Receipt{}, fmt.Errorf("Experiment parent %s does not equal durable checkpoint %s", experiment.ParentCheckpointSHA, s.runtime.CurrentCheckpointSHA)
	}
	s.runtime.CurrentCheckpointSHA = experiment.CheckpointSHA
	return symphony.Receipt{ID: "experiment-receipt", Result: json.RawMessage(`{"experiment_id":"experiment"}`)}, nil
}
func (s *checkpointChainStore) Replay(context.Context, symphony.Command) (symphony.Receipt, bool, error) {
	return symphony.Receipt{}, false, nil
}
func (s *checkpointChainStore) ReplayDiagnosis(context.Context, string, string, symphony.DiagnosisStatus, json.RawMessage) (symphony.Receipt, bool, error) {
	return symphony.Receipt{}, false, nil
}
func (s *checkpointChainStore) ReplayIterationExperiment(context.Context, string, string, json.RawMessage) (symphony.Receipt, bool, error) {
	return symphony.Receipt{}, false, nil
}
func (s *checkpointChainStore) RuntimeWork(context.Context, string) (symphony.RuntimeWork, error) {
	return s.runtime, nil
}
func (s *checkpointChainStore) GitIntent(context.Context, string) (symphony.GitIntentView, error) {
	return symphony.GitIntentView{}, fmt.Errorf("unexpected GitIntent lookup")
}
func (s *checkpointChainStore) RecordCommitCapability(_ context.Context, receipt symphony.CommitCapabilityReceipt) error {
	s.capabilities[receipt.CommitSHA] = receipt
	return nil
}
func (s *checkpointChainStore) CommitCapability(_ context.Context, workID, commitSHA string) (symphony.CommitCapabilityReceipt, error) {
	receipt, ok := s.capabilities[commitSHA]
	if !ok || receipt.WorkID != workID {
		return symphony.CommitCapabilityReceipt{}, fmt.Errorf("capability receipt not found")
	}
	return receipt, nil
}
func (s *checkpointChainStore) InitializeIterationRoundCheckpoint(_ context.Context, _ string, _ int64, expected, prepared string) (string, error) {
	if s.runtime.CurrentCheckpointSHA != expected {
		return "", fmt.Errorf("checkpoint=%s expected=%s", s.runtime.CurrentCheckpointSHA, expected)
	}
	s.runtime.CurrentCheckpointSHA = prepared
	return prepared, nil
}

func TestFlowV2MCPCatalogAndDiagnosisExperimentPostconditions(t *testing.T) {
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	for name, contents := range map[string]string{"kernel.txt": "baseline\n", "target.txt": "validation baseline\n"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repository, "add", "kernel.txt", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	baseSHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	snapshot := toolappV2Snapshot(t)
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository,
		FlowVersion: symphony.FlowVersion2, SkillSnapshot: &snapshot, IterationConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID,
		Definition: testcontract.Definition(), RepositorySHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verification := pendingRole(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept-baseline"}, WorkID: verification.ID,
		Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	diagnosis := pendingRole(t, view, symphony.RoleDiagnosis)
	app := toolapp.Application{Store: engine, Repository: repository, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees"), EvidenceRoot: filepath.Join(t.TempDir(), "evidence")}
	diagnosisGrant := runningGrant(t, ctx, engine, diagnosis, "diagnosis-session")
	catalog, err := app.Catalog(ctx, diagnosisGrant.Token)
	if err != nil || len(catalog) != 1 || catalog[0].Name != "finish_diagnosis" {
		t.Fatalf("Diagnosis catalog = %+v, err=%v", catalog, err)
	}
	assertStrictKDAWireSchema(t, catalog[0].InputSchema, "report", "subject", "baseline_revision_id")
	if catalog[0].InputSchema["allOf"] == nil {
		t.Fatal("Diagnosis MCP schema has no outcome-conditional branch")
	}
	diagnosisRoot := filepath.Join(app.EvidenceRoot, "diagnoses", diagnosis.ID)
	if err := os.MkdirAll(diagnosisRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(diagnosisRoot, "profile.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := toolappDiagnosisReport(view.Baseline.ID, baseSHA)
	diagnosisArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"diagnosis","outcome":"ready","report":%s}`, report))
	finishedDiagnosis, err := app.Invoke(ctx, diagnosisGrant.Token, toolapp.Call{Name: "finish_diagnosis", Arguments: diagnosisArguments})
	if err != nil {
		t.Fatalf("finish Diagnosis through MCP: %v", err)
	}
	diagnosisReceipt := finishedDiagnosis.Value.(symphony.Receipt)
	if artifacts, err := engine.EvidenceArtifacts(ctx, diagnosis.ID); err != nil || len(artifacts) != 1 || artifacts[0].RelativePath != "profile.json" {
		t.Fatalf("Diagnosis artifact receipt = %+v, err=%v", artifacts, err)
	}
	if err := os.Remove(filepath.Join(diagnosisRoot, "profile.json")); err != nil {
		t.Fatal(err)
	}
	replayedDiagnosis, err := app.Invoke(ctx, diagnosisGrant.Token, toolapp.Call{Name: "finish_diagnosis", Arguments: diagnosisArguments})
	if err != nil {
		t.Fatalf("replay Diagnosis after artifact cleanup: %v", err)
	}
	if got := replayedDiagnosis.Value.(symphony.Receipt); got.ID != diagnosisReceipt.ID || !got.Replayed {
		t.Fatalf("replayed Diagnosis receipt = %+v, want ID %s and Replayed", got, diagnosisReceipt.ID)
	}
	changedReport := bytes.Replace(report, []byte("measured launch overhead"), []byte("different measured launch overhead"), 1)
	changedDiagnosisArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"diagnosis","outcome":"ready","report":%s}`, changedReport))
	if _, err := app.Invoke(ctx, diagnosisGrant.Token, toolapp.Call{Name: "finish_diagnosis", Arguments: changedDiagnosisArguments}); err == nil || !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("same Diagnosis key with different input error = %v", err)
	}

	view, _ = engine.Inspect(ctx, symphony.Status{})
	iteration := pendingRole(t, view, symphony.RoleIteration)
	runtime, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (gitworkspace.RuntimePreparer{Repository: repository, Root: app.WorktreeRoot}).PrepareWork(ctx, runtime)
	if err != nil {
		t.Fatalf("prepare Iteration worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Repository, "kernel.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	iterationEvidenceRoot := filepath.Join(app.EvidenceRoot, "iterations", iteration.ID)
	if err := os.MkdirAll(filepath.Join(iterationEvidenceRoot, "experiments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iterationEvidenceRoot, "experiments", "result.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	iterationGrant := runningGrant(t, ctx, engine, iteration, "iteration-session")
	catalog, err = app.Catalog(ctx, iterationGrant.Token)
	if err != nil || toolNames(catalog) != "commit_changes,record_iteration_experiment,finish_iteration" {
		t.Fatalf("Iteration catalog = %+v, err=%v", catalog, err)
	}
	for _, tool := range catalog {
		if tool.Name == "record_iteration_experiment" {
			assertStrictKDAWireSchema(t, tool.InputSchema, "experiment", "change", "paths")
			experiment := tool.InputSchema["properties"].(map[string]any)["experiment"].(map[string]any)
			if experiment["allOf"] == nil {
				t.Fatal("Experiment MCP schema has no kept/rejected conditional branch")
			}
		}
	}
	commit, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"commit","message":"candidate","paths":["kernel.txt"]}`)})
	if err != nil {
		t.Fatalf("commit Experiment paths: %v", err)
	}
	checkpoint := commit.Value.(toolapp.CommitChangesResult).CommitSHA
	experiment := toolappKeptExperiment(baseSHA, checkpoint)
	unknownHypothesis := bytes.Replace(experiment, []byte(`"diagnosis_hypothesis_id":"hypothesis-1"`), []byte(`"diagnosis_hypothesis_id":"missing-hypothesis"`), 1)
	unknownArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record-unknown-hypothesis","experiment":%s}`, unknownHypothesis))
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: unknownArguments}); err == nil || !strings.Contains(err.Error(), "unknown Diagnosis hypothesis") {
		t.Fatalf("unknown Diagnosis hypothesis error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Repository, "unrecorded.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record","experiment":%s}`, experiment))
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: arguments}); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("dirty Experiment postcondition error = %v", err)
	}
	if err := os.Remove(filepath.Join(prepared.Repository, "unrecorded.txt")); err != nil {
		t.Fatal(err)
	}
	recordedKept, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: arguments})
	if err != nil {
		t.Fatalf("record Experiment through MCP after clean postcondition: %v", err)
	}
	keptReceipt := recordedKept.Value.(symphony.Receipt)
	runtime, err = engine.RuntimeWork(ctx, iteration.ID)
	if err != nil || runtime.CurrentCheckpointSHA != checkpoint || len(runtime.IterationExperiments) != 1 {
		t.Fatalf("MCP Experiment did not ratchet checkpoint: work=%+v err=%v", runtime, err)
	}
	if _, err := os.Stat(filepath.Join(prepared.Repository, "experiments", "result.json")); !os.IsNotExist(err) {
		t.Fatalf("Experiment evidence leaked into source worktree: %v", err)
	}
	var rejectedArguments json.RawMessage
	var rejectedReceipt symphony.Receipt
	for index, outcome := range []string{"rejected", "inconclusive"} {
		artifactPath := filepath.Join("experiments", outcome+".json")
		if err := os.WriteFile(filepath.Join(iterationEvidenceRoot, artifactPath), []byte("{\"partial\":true}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prepared.Repository, "kernel.txt"), []byte(outcome+" candidate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, prepared.Repository, "restore", "--source", checkpoint, "--staged", "--worktree", "kernel.txt")
		negative := toolappNegativeExperiment(checkpoint, outcome, artifactPath)
		negativeArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record-%s","experiment":%s}`, outcome, negative))
		recordedNegative, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: negativeArguments})
		if err != nil {
			t.Fatalf("record restored %s Experiment through MCP: %v", outcome, err)
		}
		if outcome == "rejected" {
			rejectedArguments = append(json.RawMessage{}, negativeArguments...)
			rejectedReceipt = recordedNegative.Value.(symphony.Receipt)
		}
		runtime, err = engine.RuntimeWork(ctx, iteration.ID)
		if err != nil || runtime.CurrentCheckpointSHA != checkpoint || len(runtime.IterationExperiments) != index+2 {
			t.Fatalf("%s Experiment changed checkpoint or was not persisted: work=%+v err=%v", outcome, runtime, err)
		}
	}
	artifacts, err := engine.EvidenceArtifacts(ctx, iteration.ID)
	if err != nil || len(artifacts) != 3 {
		t.Fatalf("Iteration artifact receipts = %+v, err=%v", artifacts, err)
	}
	wantContents := map[string][]byte{
		"experiments/result.json":       []byte("{}\n"),
		"experiments/rejected.json":     []byte("{\"partial\":true}\n"),
		"experiments/inconclusive.json": []byte("{\"partial\":true}\n"),
	}
	for _, artifact := range artifacts {
		contents := wantContents[artifact.RelativePath]
		digest := sha256.Sum256(contents)
		if len(contents) == 0 || artifact.ByteSize != int64(len(contents)) || artifact.ContentSHA256 != hex.EncodeToString(digest[:]) || artifact.ContractVersion != 1 {
			t.Fatalf("Iteration artifact receipt does not match stable-read bytes: %+v", artifact)
		}
	}
	reused := toolappNegativeExperiment(checkpoint, "rejected", "experiments/rejected.json")
	reusedArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record-reused-artifact","experiment":%s}`, reused))
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: reusedArguments}); err == nil || !strings.Contains(err.Error(), "already has a durable receipt") {
		t.Fatalf("reused Experiment artifact path error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(iterationEvidenceRoot, "escape.json")); err != nil {
		t.Fatal(err)
	}
	for index, unsafePath := range []string{"missing.json", "../escape.json", outside, "escape.json"} {
		unsafe := toolappNegativeExperiment(checkpoint, "inconclusive", unsafePath)
		unsafeArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record-unsafe-%d","experiment":%s}`, index, unsafe))
		if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: unsafeArguments}); err == nil {
			t.Fatalf("unsafe Experiment artifact path %q was accepted", unsafePath)
		}
	}
	runtime, err = engine.RuntimeWork(ctx, iteration.ID)
	if err != nil || runtime.CurrentCheckpointSHA != checkpoint || len(runtime.IterationExperiments) != 3 {
		t.Fatalf("failed artifact requests advanced Experiment sequence: work=%+v err=%v", runtime, err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Repository, "kernel.txt"), []byte("forged candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, prepared.Repository, "add", "kernel.txt")
	runGit(t, prepared.Repository, "commit", "-m", "forged capability", "-m", "Pika-Commit-Key: "+strings.Repeat("a", 64)+"\nPika-Commit-Request: "+strings.Repeat("b", 64))
	forgedSHA := strings.TrimSpace(runGit(t, prepared.Repository, "rev-parse", "HEAD"))
	for _, path := range []string{"experiments/result.json", "experiments/rejected.json"} {
		if err := os.Remove(filepath.Join(iterationEvidenceRoot, path)); err != nil {
			t.Fatal(err)
		}
	}
	for _, replay := range []struct {
		name string
		args json.RawMessage
		want symphony.Receipt
	}{{name: "kept", args: arguments, want: keptReceipt}, {name: "rejected", args: rejectedArguments, want: rejectedReceipt}} {
		result, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: replay.args})
		if err != nil {
			t.Fatalf("replay old %s Experiment after Git/artifact drift: %v", replay.name, err)
		}
		got := result.Value.(symphony.Receipt)
		if got.ID != replay.want.ID || !got.Replayed {
			t.Fatalf("replayed %s receipt = %+v, want ID %s and Replayed", replay.name, got, replay.want.ID)
		}
	}
	conflictingArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record","experiment":%s}`, reused))
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: conflictingArguments}); err == nil || !strings.Contains(err.Error(), "idempotency_conflict") {
		t.Fatalf("same Experiment key with different input error = %v", err)
	}
	forged := toolappKeptExperiment(checkpoint, forgedSHA)
	forgedArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"record-forged","experiment":%s}`, forged))
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "record_iteration_experiment", Arguments: forgedArguments}); err == nil || !strings.Contains(err.Error(), "capability receipt") {
		t.Fatalf("forged commit_changes trailers error = %v", err)
	}
}

func TestConflictResolutionCommitInitializesMachineCheckpointForFirstExperiment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "kernel.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "kernel.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	baselineSHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	workspace := gitworkspace.Workspace{Repository: repository, Root: worktreeRoot}
	if _, err := workspace.EnsureBest(ctx, baselineSHA); err != nil {
		t.Fatal(err)
	}
	prior, err := workspace.CreateAttempt(ctx, "conflict-chain", 1, baselineSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prior.Repository, "kernel.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, prior.Repository, "add", "kernel.txt")
	runGit(t, prior.Repository, "commit", "-m", "candidate")
	candidateSHA := strings.TrimSpace(runGit(t, prior.Repository, "rev-parse", "HEAD"))

	winner, err := workspace.CreateAttempt(ctx, "conflict-winner", 1, baselineSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(winner.Repository, "kernel.txt"), []byte("winner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, winner.Repository, "add", "kernel.txt")
	runGit(t, winner.Repository, "commit", "-m", "winner")
	winnerSHA := strings.TrimSpace(runGit(t, winner.Repository, "rev-parse", "HEAD"))
	intent, err := workspace.PrepareBestUpdate(ctx, "conflict-chain-winner", baselineSHA, winnerSHA)
	if err != nil {
		t.Fatal(err)
	}
	bestSHA, err := workspace.ApplyBestUpdate(ctx, intent, "accept winner")
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := workspace.RefreshFromBest(ctx, "conflict-chain", 2, candidateSHA, bestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, refreshed.Repository, "rev-parse", "MERGE_HEAD")); got != bestSHA {
		t.Fatalf("MERGE_HEAD=%s want=%s", got, bestSHA)
	}

	workID := "conflict-work"
	store := &checkpointChainStore{
		grant: symphony.AgentGrant{WorkID: workID, Role: symphony.RoleIteration, SessionStatus: symphony.AgentSessionRunning, Catalog: json.RawMessage(`["commit_changes","record_iteration_experiment"]`)},
		runtime: symphony.RuntimeWork{
			Work:                   symphony.WorkView{ID: workID, Role: symphony.RoleIteration, AttemptID: "conflict-chain", IterationRound: 2},
			OptimizationRepository: repository, FlowVersion: symphony.FlowVersion2, IterationKind: "stale_best",
			BaseSHA: bestSHA, CandidateSHA: candidateSHA, CurrentCheckpointSHA: bestSHA,
		},
		capabilities: map[string]symphony.CommitCapabilityReceipt{},
	}
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	app := toolapp.Application{Store: store, Repository: repository, WorktreeRoot: worktreeRoot, EvidenceRoot: evidenceRoot}
	if err := os.WriteFile(filepath.Join(refreshed.Repository, "kernel.txt"), []byte("candidate plus winner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := app.Invoke(ctx, "grant", toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"resolve","message":"resolve setup merge","paths":["kernel.txt"]}`)})
	if err != nil {
		t.Fatalf("resolve conflict through commit_changes: %v", err)
	}
	setup := resolved.Value.(toolapp.CommitChangesResult)
	if setup.CommitSHA == "" || setup.CurrentCheckpointSHA != setup.CommitSHA || store.runtime.CurrentCheckpointSHA != setup.CommitSHA {
		t.Fatalf("setup response=%+v durable=%s", setup, store.runtime.CurrentCheckpointSHA)
	}
	if _, ok := store.capabilities[setup.CommitSHA]; !ok {
		t.Fatal("setup merge has no durable commit capability receipt")
	}

	if err := os.WriteFile(filepath.Join(refreshed.Repository, "agent.txt"), []byte("experiment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := app.Invoke(ctx, "grant", toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"experiment-commit","message":"experiment","paths":["agent.txt"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := committed.Value.(toolapp.CommitChangesResult)
	if checkpoint.CurrentCheckpointSHA != setup.CommitSHA {
		t.Fatalf("Experiment commit response parent=%s want setup=%s", checkpoint.CurrentCheckpointSHA, setup.CommitSHA)
	}
	artifactRoot := filepath.Join(evidenceRoot, "iterations", workID, "experiments")
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "result.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	experiment := bytes.ReplaceAll(toolappKeptExperiment(setup.CommitSHA, checkpoint.CommitSHA), []byte(`"kernel.txt"`), []byte(`"agent.txt"`))
	arguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"first-experiment","experiment":%s}`, experiment))
	if _, err := app.Invoke(ctx, "grant", toolapp.Call{Name: "record_iteration_experiment", Arguments: arguments}); err != nil {
		t.Fatalf("record first Experiment from machine checkpoint: %v", err)
	}
	if store.runtime.CurrentCheckpointSHA != checkpoint.CommitSHA || len(store.capabilities) != 2 {
		t.Fatalf("Experiment chain durable=%s capabilities=%d", store.runtime.CurrentCheckpointSHA, len(store.capabilities))
	}
	oldParent := bytes.Replace(experiment, []byte(setup.CommitSHA), []byte(bestSHA), 1)
	oldArguments := json.RawMessage(fmt.Sprintf(`{"idempotency_key":"old-parent","experiment":%s}`, oldParent))
	if _, err := app.Invoke(ctx, "grant", toolapp.Call{Name: "record_iteration_experiment", Arguments: oldArguments}); err == nil {
		t.Fatal("Experiment accepted the pre-setup Best as its parent checkpoint")
	}
}

func TestRoleCatalogAndTerminalReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	wantRepositorySHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatalf("init: %v", err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := symphony.AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-test", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1", TerminalID: "term-1"}); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(session.Role), time.Hour)
	if err != nil {
		t.Fatalf("mint grant: %v", err)
	}
	app := toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}
	tools, err := app.Catalog(ctx, grant.Token)
	if err != nil || len(tools) != 2 || tools[0].Name != "commit_changes" || tools[1].Name != "submit_baseline_definition" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	encodedCatalog, err := json.Marshal(tools)
	if err != nil || strings.Contains(string(encodedCatalog), `"name":"get_context"`) {
		t.Fatalf("serialized tool catalog is not strict JSON Schema: %s err=%v", encodedCatalog, err)
	}
	if _, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("draft grant invoked verification tool")
	}
	arguments := submitDefinitionArguments("terminal-1")
	first, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: arguments})
	if err != nil {
		t.Fatalf("submit definition: %v", err)
	}
	submitted, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	verification := pendingRole(t, submitted, symphony.RoleBaselineVerification)
	frozen, err := engine.RuntimeWork(ctx, verification.ID)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.BaselineRepositorySHA != wantRepositorySHA {
		t.Fatalf("frozen repository snapshot = %q, want %q", frozen.BaselineRepositorySHA, wantRepositorySHA)
	}
	if err := engine.RevokeAgentGrant(ctx, session.ID); err != nil {
		t.Fatalf("revoke terminal grant: %v", err)
	}
	replayed, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: arguments})
	if err != nil {
		t.Fatalf("replay terminal definition: %v", err)
	}
	firstReceipt := first.Value.(symphony.Receipt)
	replayedReceipt := replayed.Value.(symphony.Receipt)
	if replayedReceipt.ID != firstReceipt.ID || !replayedReceipt.Replayed {
		t.Fatalf("replayed receipt=%+v first=%+v", replayedReceipt, firstReceipt)
	}
}

func TestBaselineAcceptanceRejectsRepositoryDriftAfterSubmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	draftGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineDraft), "draft-session")
	app := toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}
	if _, err := app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: submitDefinitionArguments("submit")}); err != nil {
		t.Fatalf("submit definition: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("drifted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "drift")
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verificationGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineVerification), "verification-session")
	_, err = app.Invoke(ctx, verificationGrant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: acceptVerificationArguments("accept")})
	if err == nil || !strings.Contains(err.Error(), "repository changed after Baseline submission") {
		t.Fatalf("accept after repository drift error = %v", err)
	}
}

func TestBaselineSubmissionRejectsDirtyRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	if err := os.WriteFile(filepath.Join(repository, "uncommitted.txt"), []byte("not frozen\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	draftGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineDraft), "draft-session")
	app := toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}
	_, err = app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: submitDefinitionArguments("submit")})
	if err == nil || !strings.Contains(err.Error(), "repository must be clean") {
		t.Fatalf("dirty Baseline submission error = %v", err)
	}
}

func TestBaselineAcceptanceRejectsDirtyRepositoryAfterSubmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	draftGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineDraft), "draft-session")
	app := toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}
	if _, err := app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: submitDefinitionArguments("submit")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "uncommitted.txt"), []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verificationGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineVerification), "verification-session")
	_, err = app.Invoke(ctx, verificationGrant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: acceptVerificationArguments("accept")})
	if err == nil || !strings.Contains(err.Error(), "repository changed after Baseline submission") {
		t.Fatalf("accept with dirty repository error = %v", err)
	}
}

func TestIterationAndIntegrationToolsVerifyGitBeforeAdvancingBest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "kernel.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("validation baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "kernel.txt", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	baselineSHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	workspace := gitworkspace.Workspace{Repository: repository, Root: worktreeRoot}
	if _, err := workspace.EnsureBest(ctx, baselineSHA); err != nil {
		t.Fatalf("ensure Best: %v", err)
	}

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: pendingRole(t, view, symphony.RoleBaselineVerification).ID, Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baselineSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	iteration := pendingRole(t, view, symphony.RoleIteration)
	runtimeWork, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeWork, err = (gitworkspace.RuntimePreparer{Root: worktreeRoot}).PrepareWork(ctx, runtimeWork)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeWork.Repository, "kernel.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	iterationGrant := runningGrant(t, ctx, engine, iteration, "iteration-session")
	app := toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot}
	if err := os.WriteFile(filepath.Join(runtimeWork.Repository, "target.txt"), []byte("changed validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"commit-protected","message":"protected","paths":["target.txt"]}`)}); err == nil {
		t.Fatal("commit_changes accepted a protected validation path")
	}
	protectedView, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if pendingRole(t, protectedView, symphony.RoleIteration).ID != iteration.ID {
		t.Fatalf("early protected commit did not leave Work pending: %+v", protectedView)
	}
	if got := strings.TrimSpace(runGit(t, runtimeWork.Repository, "rev-parse", "HEAD")); got != runtimeWork.BaseSHA {
		t.Fatalf("early protected commit changed HEAD to %s", got)
	}
	if err := os.WriteFile(filepath.Join(runtimeWork.Repository, "target.txt"), []byte("validation baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"commit-candidate","message":"candidate","paths":["kernel.txt"]}`)})
	if err != nil {
		t.Fatalf("commit Candidate through scoped MCP: %v", err)
	}
	commitValue := committed.Value.(toolapp.CommitChangesResult)
	candidateSHA := commitValue.CommitSHA
	if candidateSHA == "" || !commitValue.Clean {
		t.Fatalf("commit result = %+v", commitValue)
	}
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "finish_iteration", Arguments: json.RawMessage(`{"idempotency_key":"finish-iteration","outcome":"candidate","candidate_sha":"` + candidateSHA + `","summary":"faster","evidence":` + string(testcontract.Evidence()) + `}`)}); err != nil {
		t.Fatalf("finish Iteration: %v", err)
	}

	view, _ = engine.Inspect(ctx, symphony.Status{})
	integration := pendingRole(t, view, symphony.RoleIntegration)
	integrationGrant := runningGrant(t, ctx, engine, integration, "integration-session")
	prepared, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "prepare_best_update", Arguments: prepareBestArguments("prepare")})
	if err != nil {
		t.Fatalf("prepare Best update: %v", err)
	}
	if prepared.Terminal {
		t.Fatal("prepare_best_update must remain non-terminal")
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	intentID := view.Integrations[0].IntentID
	frozenContext, err := engine.RuntimeWork(ctx, integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if frozenContext.OptimizationID != "optimization" || frozenContext.OptimizationRevision == 0 || frozenContext.BaselineDefinitionSHA256 == "" ||
		frozenContext.ExpectedBestSHA != baselineSHA || frozenContext.IntegrationFIFOPosition != 1 || frozenContext.IntegrationStatus != "best_update_prepared" ||
		frozenContext.GitIntentID != intentID || frozenContext.GitIntentState != "pending" {
		t.Fatalf("Integration dynamic context = %+v", frozenContext)
	}
	applied, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "apply_best_update", Arguments: json.RawMessage(`{"intent_id":"` + intentID + `","message":"accept candidate"}`)})
	if err != nil {
		t.Fatalf("apply Best through scoped MCP: %v", err)
	}
	if applied.Terminal {
		t.Fatal("apply_best_update must remain non-terminal")
	}
	appliedValue, ok := applied.Value.(map[string]any)
	if !ok {
		t.Fatalf("apply_best_update value = %#v", applied.Value)
	}
	appliedSHA, _ := appliedValue["applied_sha"].(string)
	if appliedValue["intent_id"] != intentID || appliedSHA == "" {
		t.Fatalf("apply_best_update value = %#v", appliedValue)
	}
	postcondition, ok := appliedValue["postcondition"].(map[string]any)
	if !ok || postcondition["required_tool"] != "finish_integration" || postcondition["terminal"] != true {
		t.Fatalf("apply_best_update terminal postcondition = %#v", appliedValue["postcondition"])
	}
	nextArguments, ok := postcondition["arguments"].(map[string]any)
	if !ok || nextArguments["outcome"] != "accepted" || nextArguments["intent_id"] != intentID || nextArguments["applied_sha"] != appliedSHA {
		t.Fatalf("apply_best_update next terminal arguments = %#v", postcondition["arguments"])
	}
	missing, ok := postcondition["missing_required_fields"].([]string)
	if !ok || len(missing) != 2 || missing[0] != "idempotency_key" || missing[1] != "result" || postcondition["result_contract"] == "" {
		t.Fatalf("apply_best_update terminal requirements = %#v", postcondition)
	}
	replayedApply, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "apply_best_update", Arguments: json.RawMessage(`{"intent_id":"` + intentID + `","message":"lost response retry"}`)})
	if err != nil {
		t.Fatalf("retry scoped Best application: %v", err)
	}
	if replayedValue := replayedApply.Value.(map[string]any); replayedValue["applied_sha"] != appliedSHA {
		t.Fatalf("idempotent apply result = %#v, want SHA %s", replayedValue, appliedSHA)
	}
	if _, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "finish_integration", Arguments: json.RawMessage(`{"idempotency_key":"finish-integration","outcome":"accepted","intent_id":"` + intentID + `","applied_sha":"` + appliedSHA + `","result":{"verified":true}}`)}); err != nil {
		t.Fatalf("finish Integration: %v", err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.Best == nil || view.Best.Sequence != 1 || view.Best.CommitSHA != appliedSHA {
		t.Fatalf("Best = %+v", view.Best)
	}
}

func TestFinishIterationRejectsProtectedChangeWithoutCompletingWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, app, iteration, runtimeWork := candidatePolicyIterationFixture(t, ctx)
	if err := os.WriteFile(filepath.Join(runtimeWork.Repository, "target.txt"), []byte("changed validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, runtimeWork.Repository, "add", "target.txt")
	runGit(t, runtimeWork.Repository, "commit", "-m", "change protected validation")
	candidateSHA := strings.TrimSpace(runGit(t, runtimeWork.Repository, "rev-parse", "HEAD"))
	grant := runningGrant(t, ctx, engine, iteration, "protected-finish-session")
	_, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "finish_iteration", Arguments: json.RawMessage(fmt.Sprintf(`{"idempotency_key":"protected-finish","outcome":"candidate","candidate_sha":%q,"summary":"candidate","evidence":%s}`, candidateSHA, testcontract.Evidence()))})
	if err == nil || !strings.Contains(err.Error(), "protected validation path") {
		t.Fatalf("finish protected candidate error = %v", err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if pendingRole(t, view, symphony.RoleIteration).ID != iteration.ID || len(view.Integrations) != 0 {
		t.Fatalf("protected finish changed durable state: %+v", view)
	}
}

func TestPrepareBestUpdateRejectsProtectedChangeBeforeCreatingIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, app, iteration, runtimeWork := candidatePolicyIterationFixture(t, ctx)
	if err := os.WriteFile(filepath.Join(runtimeWork.Repository, "target.txt"), []byte("changed validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, runtimeWork.Repository, "add", "target.txt")
	runGit(t, runtimeWork.Repository, "commit", "-m", "change protected validation")
	candidateSHA := strings.TrimSpace(runGit(t, runtimeWork.Repository, "rev-parse", "HEAD"))
	if _, err := engine.Apply(ctx, symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: "bypass-tool-for-setup"}, WorkID: iteration.ID, Outcome: symphony.IterationCandidate, CandidateSHA: candidateSHA, Summary: "candidate", Evidence: testcontract.Evidence()}); err != nil {
		t.Fatal(err)
	}
	before, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	integration := pendingRole(t, before, symphony.RoleIntegration)
	grant := runningGrant(t, ctx, engine, integration, "protected-prepare-session")
	_, err = app.Invoke(ctx, grant.Token, toolapp.Call{Name: "prepare_best_update", Arguments: json.RawMessage(fmt.Sprintf(`{"idempotency_key":"protected-prepare","validation":%s}`, testcontract.Validation()))})
	if err == nil || !strings.Contains(err.Error(), "protected validation path") {
		t.Fatalf("prepare protected candidate error = %v", err)
	}
	after, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Optimization.Revision != before.Optimization.Revision || after.Integrations[0].Status != "running" || after.Works[len(after.Works)-1].Status != symphony.WorkPending {
		t.Fatalf("protected prepare changed durable state: before=%+v after=%+v", before, after)
	}
	runtime, err := engine.RuntimeWork(ctx, integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GitIntentID != "" || runtime.GitIntentState != "" {
		t.Fatalf("protected prepare created Git intent: %+v", runtime)
	}
}

func candidatePolicyIterationFixture(t *testing.T, ctx context.Context) (*symphony.Engine, toolapp.Application, symphony.WorkView, symphony.RuntimeWork) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	for name, contents := range map[string]string{"kernel.txt": "baseline\n", "target.txt": "validation baseline\n"} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repository, "add", "kernel.txt", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	baselineSHA := strings.TrimSpace(runGit(t, repository, "rev-parse", "HEAD"))
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "fixture-init"}, OptimizationID: "fixture", Repository: repository, IterationConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "fixture-submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verification := pendingRole(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "fixture-accept"}, WorkID: verification.ID, Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baselineSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	iteration := pendingRole(t, view, symphony.RoleIteration)
	runtimeWork, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeWork, err = (gitworkspace.RuntimePreparer{Root: worktreeRoot}).PrepareWork(ctx, runtimeWork)
	if err != nil {
		t.Fatal(err)
	}
	return engine, toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot}, iteration, runtimeWork
}

func pendingRole(t *testing.T, view symphony.View, role symphony.WorkRole) symphony.WorkView {
	t.Helper()
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			return work
		}
	}
	t.Fatalf("pending %s Work not found", role)
	return symphony.WorkView{}
}

func runningGrant(t *testing.T, ctx context.Context, engine *symphony.Engine, work symphony.WorkView, sessionID string) symphony.AgentGrant {
	t.Helper()
	session := symphony.AgentSession{ID: sessionID, WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: sessionID, Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, sessionID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab-" + sessionID, PaneID: "pane-" + sessionID, TerminalID: "terminal-" + sessionID}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, sessionID, toolapp.CatalogForRole(work.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func runGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repository}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func TestLostSessionGrantCannotMutatePendingWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := symphony.AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-stale", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p1", TerminalID: "term-1"}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(session.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceLostAgentSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	_, err = (toolapp.Application{Store: engine}).Invoke(ctx, grant.Token, toolapp.Call{
		Name: "submit_baseline_definition", Arguments: json.RawMessage(`{"idempotency_key":"stale-terminal","definition":{"target":"bad"}}`),
	})
	if err == nil {
		t.Fatal("lost session grant mutated pending Work")
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if view.Optimization.Revision != 1 || view.Works[0].Status != symphony.WorkPending {
		t.Fatalf("stale grant changed view: %+v", view)
	}
}

func TestDefinitionPathRecordsStableArtifactInTerminalTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := t.TempDir()
	definition := []byte(testcontract.Definition())
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "baseline.json"), definition, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "baseline.json")
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := symphony.AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-artifact", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(session.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}).Invoke(ctx, grant.Token, toolapp.Call{
		Name: "submit_baseline_definition", Arguments: json.RawMessage(`{"idempotency_key":"artifact-terminal","definition_path":"baseline.json"}`),
	})
	if err != nil {
		t.Fatalf("starting Agent Session could not submit definition path: %v", err)
	}
	receipt := result.Value.(symphony.Receipt)
	artifacts, err := engine.EvidenceArtifacts(ctx, session.WorkID)
	if err != nil || len(artifacts) != 1 || artifacts[0].ReceiptID != receipt.ID || artifacts[0].RelativePath != "baseline.json" || artifacts[0].ByteSize != int64(len(definition)) || len(artifacts[0].ContentSHA256) != 64 {
		t.Fatalf("artifacts=%+v err=%v", artifacts, err)
	}
}

func submitDefinitionArguments(idempotencyKey string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"idempotency_key":%q,"definition":%s}`, idempotencyKey, testcontract.Definition()))
}

func acceptVerificationArguments(idempotencyKey string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"idempotency_key":%q,"decision":"accepted","evidence":%s}`, idempotencyKey, testcontract.Evidence()))
}

func prepareBestArguments(idempotencyKey string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"idempotency_key":%q,"validation":%s}`, idempotencyKey, testcontract.Validation()))
}

func toolappV2Snapshot(t *testing.T) symphony.SkillSnapshotInput {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{"schema_version": 1, "skills": []string{"KernelWiki", "ncu-report-skill"}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	return symphony.SkillSnapshotInput{SchemaVersion: symphony.SkillSnapshotSchemaV1, SnapshotID: strings.Repeat("c", 64), RootPath: filepath.Join(t.TempDir(), "snapshot"),
		Manifest: manifest, ManifestSHA256: hex.EncodeToString(digest[:]), Entries: []symphony.SkillSnapshotEntry{
			{Name: "KernelWiki", Repository: symphony.KernelWikiRepository, Branch: symphony.KernelWikiBranch, CommitSHA: strings.Repeat("1", 40), RelativePath: "skills/KernelWiki", ContentSHA256: strings.Repeat("a", 64)},
			{Name: "ncu-report-skill", Repository: symphony.NCUReportSkillRepository, Branch: symphony.NCUReportSkillBranch, CommitSHA: strings.Repeat("2", 40), RelativePath: "skills/ncu-report-skill", ContentSHA256: strings.Repeat("b", 64)},
		}}
}

func toolappDiagnosisReport(baselineID, bestSHA string) json.RawMessage {
	return json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"` + baselineID + `","best_sha":"` + bestSHA + `","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"profile.json","kind":"profile"}],"observations":[{"id":"observation-1","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["profile.json"],"summary":"measured launch overhead"}],"bottlenecks":[{"id":"bottleneck-1","class":"launch-overhead","confidence":"high","observation_ids":["observation-1"],"summary":"launch overhead"}],"hypotheses":[{"id":"hypothesis-1","rank":1,"summary":"fuse work","mechanism":"remove launches","bottleneck_ids":["bottleneck-1"],"target_case_ids":["case-1"],"expected_effect":"lower latency","risk":"correctness","knowledge_refs":[]}],"limitations":[]}`)
}

func toolappKeptExperiment(parent, checkpoint string) json.RawMessage {
	var evidence struct {
		BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity"`
	}
	_ = json.Unmarshal(testcontract.Evidence(), &evidence)
	value := map[string]any{
		"schema_version": 1, "parent_checkpoint_sha": parent,
		"hypothesis": map[string]any{"diagnosis_hypothesis_id": "hypothesis-1", "summary": "fuse work"},
		"change":     map[string]any{"summary": "fuse work", "paths": []string{"kernel.txt"}, "mechanism": "reduce launch count"},
		"outcome":    "kept", "checkpoint_sha": checkpoint, "correctness": map[string]any{"benchmark_integrity": evidence.BenchmarkIntegrity},
		"benchmark_measurements": map[string]any{
			"schema_version": 1,
			"comparisons": []any{
				map[string]any{"case_id": "case-1", "reference": map[string]any{"latency": 10.0}, "candidate": map[string]any{"latency": 9.0}},
			},
		},
		"artifacts": []any{map[string]any{"path": "experiments/result.json", "kind": "benchmark"}}, "summary": "kept result",
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func toolappNegativeExperiment(parent, outcome, artifact string) json.RawMessage {
	value := map[string]any{
		"schema_version": 1, "parent_checkpoint_sha": parent,
		"hypothesis": map[string]any{"diagnosis_hypothesis_id": "hypothesis-1", "summary": "fuse work"},
		"change":     map[string]any{"summary": "fuse work", "paths": []string{"kernel.txt"}, "mechanism": "reduce launch count"},
		"outcome":    outcome,
		"artifacts":  []any{map[string]any{"path": artifact, "kind": "benchmark"}}, "summary": outcome + " result",
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func toolNames(tools []toolapp.Tool) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, ",")
}

// Codex and Cursor both receive this catalog through the same MCP transport;
// assert the discoverable wire shape rather than trusting a role prompt.
func assertStrictKDAWireSchema(t *testing.T, schema map[string]any, fields ...string) {
	t.Helper()
	current := schema
	for _, field := range fields {
		properties, ok := current["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema at %q has no object properties: %+v", field, current)
		}
		next, ok := properties[field].(map[string]any)
		if !ok {
			t.Fatalf("schema is missing %q: %+v", field, properties)
		}
		current = next
	}
	if current["type"] == "object" && current["additionalProperties"] != false {
		t.Fatalf("schema node is not strict: %+v", current)
	}
}
