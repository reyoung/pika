package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/evidence"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestValidateFlowV2EvidenceDetectsChangedArtifact(t *testing.T) {
	ctx := context.Background()
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	manifest := []byte(`{"schema_version":1}`)
	manifestDigest := sha256.Sum256(manifest)
	snapshot := symphony.SkillSnapshotInput{SchemaVersion: symphony.SkillSnapshotSchemaV1, SnapshotID: strings.Repeat("c", 64), RootPath: filepath.Join(t.TempDir(), "snapshot"), Manifest: manifest, ManifestSHA256: hex.EncodeToString(manifestDigest[:]), Entries: []symphony.SkillSnapshotEntry{
		{Name: "KernelWiki", Repository: symphony.KernelWikiRepository, Branch: symphony.KernelWikiBranch, CommitSHA: strings.Repeat("1", 40), RelativePath: "skills/KernelWiki", ContentSHA256: strings.Repeat("a", 64)},
		{Name: "ncu-report-skill", Repository: symphony.NCUReportSkillRepository, Branch: symphony.NCUReportSkillBranch, CommitSHA: strings.Repeat("2", 40), RelativePath: "skills/ncu-report-skill", ContentSHA256: strings.Repeat("b", 64)},
	}}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo", FlowVersion: symphony.FlowVersion2, SkillSnapshot: &snapshot}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	var verification symphony.WorkView
	for _, work := range view.Works {
		if work.Role == symphony.RoleBaselineVerification && work.Status == symphony.WorkPending {
			verification = work
		}
	}
	bestSHA := strings.Repeat("d", 40)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: verification.ID, Decision: symphony.VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: bestSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	var diagnosis symphony.WorkView
	for _, work := range view.Works {
		if work.Role == symphony.RoleDiagnosis && work.Status == symphony.WorkPending {
			diagnosis = work
		}
	}
	evidenceRoot := filepath.Join(t.TempDir(), "evidence")
	diagnosisRoot := filepath.Join(evidenceRoot, "diagnoses", diagnosis.ID)
	if err := os.MkdirAll(diagnosisRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("profile\n")
	path := filepath.Join(diagnosisRoot, "profile.txt")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	artifact := symphony.ArtifactInput{RelativePath: "profile.txt", ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1}
	report := json.RawMessage(`{"schema_version":1,"subject":{"baseline_revision_id":"` + view.Baseline.ID + `","best_sha":"` + bestSHA + `","hardware":{},"software":{}},"coverage":{"case_ids":["case-1"],"dispatch_paths":["kernel"]},"artifacts":[{"path":"profile.txt","kind":"profile"}],"observations":[{"id":"o","case_ids":["case-1"],"metric":"latency","value":1,"unit":"ms","source_artifacts":["profile.txt"],"summary":"measured"}],"bottlenecks":[{"id":"b","class":"launch","confidence":"high","observation_ids":["o"],"summary":"launch"}],"hypotheses":[{"id":"h","rank":1,"summary":"fuse","mechanism":"remove launch","target_case_ids":["case-1"],"expected_effect":"lower latency","risk":"correctness","bottleneck_ids":["b"],"knowledge_refs":[]}],"limitations":[]}`)
	if _, err := engine.Apply(ctx, symphony.FinishDiagnosis{Meta: symphony.CommandMeta{RequestID: "diagnosis"}, WorkID: diagnosis.ID, Outcome: symphony.DiagnosisReady, Report: report, Artifacts: []symphony.ArtifactInput{artifact}}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	var iteration symphony.WorkView
	for _, work := range view.Works {
		if work.Role == symphony.RoleIteration && work.Status == symphony.WorkPending {
			iteration = work
		}
	}
	iterationRoot, err := evidence.EnsureWorkRoot(evidenceRoot, evidence.IterationScope, iteration.ID)
	if err != nil {
		t.Fatal(err)
	}
	partialContents := []byte("partial benchmark\n")
	partialPath := filepath.Join(iterationRoot, "partial.txt")
	if err := os.WriteFile(partialPath, partialContents, 0o600); err != nil {
		t.Fatal(err)
	}
	partialDigest := sha256.Sum256(partialContents)
	partialArtifact := symphony.ArtifactInput{RelativePath: "partial.txt", ByteSize: int64(len(partialContents)), ContentSHA256: hex.EncodeToString(partialDigest[:]), ContractVersion: 1}
	experiment := json.RawMessage(`{"schema_version":1,"parent_checkpoint_sha":"` + bestSHA + `","hypothesis":{"diagnosis_hypothesis_id":"h","summary":"fuse"},"change":{"summary":"try fusion","paths":["kernel.cu"],"mechanism":"remove launch"},"outcome":"rejected","artifacts":[{"path":"partial.txt","kind":"benchmark"}],"summary":"negative"}`)
	if _, err := engine.Apply(ctx, symphony.RecordIterationExperiment{Meta: symphony.CommandMeta{RequestID: "experiment"}, WorkID: iteration.ID, Experiment: experiment, Artifacts: []symphony.ArtifactInput{partialArtifact}}); err != nil {
		t.Fatal(err)
	}
	if err := validateFlowV2Evidence(ctx, engine, evidenceRoot); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
	if err := os.WriteFile(partialPath, []byte("tampered partial benchmark\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFlowV2Evidence(ctx, engine, evidenceRoot); err == nil {
		t.Fatal("tampered Iteration evidence passed workspace/backup validation")
	}
	if err := os.WriteFile(partialPath, partialContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFlowV2Evidence(ctx, engine, evidenceRoot); err != nil {
		t.Fatalf("restored Iteration evidence: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFlowV2Evidence(ctx, engine, evidenceRoot); err == nil {
		t.Fatal("tampered evidence passed workspace/backup validation")
	}
}
