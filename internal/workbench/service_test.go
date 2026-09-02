package workbench_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/workbench"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type fixtureStore struct {
	view       symphony.View
	records    symphony.WorkbenchRecords
	artifact   symphony.EvidenceArtifact
	work       symphony.RuntimeWork
	repository string
}

func (s fixtureStore) Inspect(context.Context, symphony.Query) (symphony.View, error) {
	return s.view, nil
}
func (s fixtureStore) WorkbenchRecords(context.Context) (symphony.WorkbenchRecords, error) {
	return s.records, nil
}
func (s fixtureStore) EvidenceArtifact(context.Context, string) (symphony.EvidenceArtifact, error) {
	return s.artifact, nil
}
func (s fixtureStore) RuntimeWork(context.Context, string) (symphony.RuntimeWork, error) {
	return s.work, nil
}
func (s fixtureStore) WorkRepository(context.Context, string) (string, error) {
	if s.repository != "" {
		return s.repository, nil
	}
	return s.work.Repository, nil
}

func TestSnapshotBuildsBestSpineAndKeepsRuntimeSeparate(t *testing.T) {
	t.Parallel()
	aggregateSpeedup, maxCaseSpeedup := 1.25, 1.4
	store := fixtureStore{view: symphony.View{
		Optimization:     symphony.OptimizationView{ID: "optimization", Revision: 12},
		DomainEventCount: 30,
		Baselines:        []symphony.BaselineView{{ID: "baseline", Number: 1, Status: symphony.BaselineAccepted, MeasurementContractVersion: 1}},
		Bests:            []symphony.BestView{{ID: "best-0", Sequence: 0, CommitSHA: "0000000"}, {ID: "best-1", Sequence: 1, CommitSHA: "1111111", SourceAttemptID: "attempt-a"}},
		Attempts:         []symphony.AttemptView{{ID: "attempt-a", Status: "accepted", BaseBestSequence: 0}, {ID: "attempt-b", Status: "iterating", BaseBestSequence: 1}},
		IterationRounds:  []symphony.IterationRoundView{{AttemptID: "attempt-a", Round: 1, Status: "candidate"}, {AttemptID: "attempt-b", Round: 1, Status: "running"}},
		Integrations:     []symphony.IntegrationView{{ID: "integration-a", AttemptID: "attempt-a", IterationRound: 1, Status: "accepted", CandidateExperimentID: "experiment-a"}},
		IterationExperiments: []symphony.IterationExperimentView{{
			ID: "experiment-a", AttemptID: "attempt-a", IterationRound: 1, Sequence: 1, Outcome: "kept",
			ParentCheckpointSHA: "0000000", CheckpointSHA: "candidate-a", ReceiptID: "receipt-a", ScopeBestSHA: "0000000",
		}},
		Works: []symphony.WorkView{
			{ID: "work-a", BaselineRevisionID: "baseline", AttemptID: "attempt-a", IterationRound: 1, IntegrationID: "integration-a", Role: symphony.RoleIntegration},
			{ID: "work-b", BaselineRevisionID: "baseline", AttemptID: "attempt-b", IterationRound: 1, Role: symphony.RoleIteration, Status: symphony.WorkPending},
		},
		AgentSessions: []symphony.AgentSession{{ID: "session-b", WorkID: "work-b", AgentName: "pika-b", Status: symphony.AgentSessionRunning}},
	}, records: symphony.WorkbenchRecords{
		MeasurementSequence: 7,
		Metrics: []symphony.BenchmarkMetricDefinitionView{
			{BaselineRevisionID: "baseline", MetricID: "latency", Label: "Latency", Unit: "us", Role: "primary", Direction: "lower_is_better", SampleStatistic: "median", Aggregation: "weighted_geomean_of_ratios"},
			{BaselineRevisionID: "baseline", MetricID: "accuracy", Label: "Accuracy", Unit: "score", Role: "guard", Direction: "higher_is_better", SampleStatistic: "mean", Aggregation: "weighted_geomean_of_ratios"},
		},
		Comparisons: []symphony.BenchmarkComparisonView{
			{IntegrationID: "integration-a", MetricID: "latency", AggregateSpeedup: &aggregateSpeedup, MaxCaseSpeedup: &maxCaseSpeedup},
			{IntegrationID: "integration-a", MetricID: "accuracy", AggregateSpeedup: floatPointer(1.02)},
			{ExperimentID: "experiment-a", MetricID: "latency", AggregateSpeedup: &aggregateSpeedup, MaxCaseSpeedup: &maxCaseSpeedup},
		},
		Artifacts: []symphony.EvidenceArtifact{{ID: "artifact-a", WorkID: "work-a", ReceiptID: "receipt-a", RelativePath: "evidence/profile.json"}},
	}}
	service := workbench.New(store, func(context.Context) (workruntime.Snapshot, error) {
		return workruntime.Snapshot{Sessions: []workruntime.Observation{{AgentName: "pika-b", Status: "idle", PaneID: "pane-b"}}}, nil
	})
	snapshot, err := service.Snapshot(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version.DomainRevision != 12 || snapshot.Version.MeasurementSequence != 7 || snapshot.Version.EventSequence != 30 || snapshot.Version.RuntimeGeneration != 1 {
		t.Fatalf("snapshot version = %+v", snapshot.Version)
	}
	if !hasEdge(snapshot, "best-0", "attempt-a", "branch") || !hasEdge(snapshot, "integration-a", "best-1", "accepted") ||
		!hasEdge(snapshot, "attempt-a:round:1", "experiment-a", "checkpoint") ||
		!hasEdge(snapshot, "experiment-a", "integration-a", "candidate") ||
		!hasEdge(snapshot, "experiment-a", "artifact-a", "evidence") {
		t.Fatalf("lineage edges = %+v", snapshot.Edges)
	}
	experimentNode := findNode(t, snapshot, "experiment", "experiment-a")
	if experimentNode.DomainStatus != "kept" || experimentNode.Primary == nil || experimentNode.Primary.AggregateSpeedup != aggregateSpeedup {
		t.Fatalf("Experiment node = %+v", experimentNode)
	}
	if artifactNode := findNode(t, snapshot, "artifact", "artifact-a"); artifactNode.DomainStatus != "verified" {
		t.Fatalf("artifact node = %+v", artifactNode)
	}
	node := findNode(t, snapshot, "attempt", "attempt-b")
	if node.DomainStatus != "iterating" || node.Runtime == nil || node.Runtime.Status != "idle" {
		t.Fatalf("attempt node merged domain and runtime state: %+v", node)
	}
	for _, identity := range [][2]string{{"integration", "integration-a"}, {"best", "best-1"}} {
		node := findNode(t, snapshot, identity[0], identity[1])
		if len(node.Aggregates) != 2 || node.Aggregates[0].MetricID != "latency" || node.Aggregates[1].MetricID != "accuracy" || node.Aggregates[0].Aggregation != "weighted_geomean_of_ratios" || node.Aggregates[0].AggregateSpeedup != aggregateSpeedup {
			t.Fatalf("%s node aggregate summaries = %+v", identity[0], node.Aggregates)
		}
	}
}

func TestSnapshotProjectsVersionedLegacyLatencyGeomeanWithoutInventingAccuracy(t *testing.T) {
	t.Parallel()
	store := fixtureStore{view: symphony.View{
		Optimization: symphony.OptimizationView{ID: "optimization", Revision: 8},
		Baselines: []symphony.BaselineView{{
			ID: "baseline", Number: 1, Status: symphony.BaselineAccepted,
			Definition: []byte(`{"schema_version":1,"metrics":{"primary":{"name":"kernel_latency_geomean_ratio","direction":"lower_is_better","unit":"ratio","aggregate":"Equal-weight geometric mean over cases."},"informational":[{"name":"max_rel_error"}]}}`),
		}},
		Bests:        []symphony.BestView{{ID: "best-0", Sequence: 0}, {ID: "best-1", Sequence: 1, SourceAttemptID: "attempt-a"}},
		Attempts:     []symphony.AttemptView{{ID: "attempt-a", Status: "accepted", BaseBestSequence: 0}},
		Integrations: []symphony.IntegrationView{{ID: "integration-a", AttemptID: "attempt-a", IterationRound: 1, Status: "accepted", Result: []byte(`{"performance":{"kernel_latency_geomean_ratio":0.8,"primary_speedup":1.25}}`)}},
		Works:        []symphony.WorkView{{ID: "integration-work", BaselineRevisionID: "baseline", AttemptID: "attempt-a", IntegrationID: "integration-a", Role: symphony.RoleIntegration}},
	}}
	snapshot, err := workbench.New(store, nil).Snapshot(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"integration", "integration-a"}, {"best", "best-1"}} {
		node := findNode(t, snapshot, identity[0], identity[1])
		if len(node.Aggregates) != 1 || node.Aggregates[0].MetricID != "kernel_latency_geomean_ratio" || node.Aggregates[0].AggregateSpeedup != 1.25 || node.Aggregates[0].Source != "legacy_evidence" {
			t.Fatalf("%s legacy aggregate summaries = %+v", identity[0], node.Aggregates)
		}
	}
}

func TestSnapshotProjectsFlowV3BenchmarkGateAndReferenceReceipt(t *testing.T) {
	t.Parallel()
	store := fixtureStore{view: symphony.View{
		Optimization:    symphony.OptimizationView{ID: "optimization", Revision: 7, FlowVersion: symphony.FlowVersion3},
		Baselines:       []symphony.BaselineView{{ID: "baseline", Number: 1, Status: symphony.BaselineAccepted}},
		Bests:           []symphony.BestView{{ID: "best", Sequence: 0, CommitSHA: "aaaaaaaa"}},
		Attempts:        []symphony.AttemptView{{ID: "attempt", Status: "iterating", BaseBestSequence: 0}},
		IterationRounds: []symphony.IterationRoundView{{AttemptID: "attempt", Round: 1, Status: "running"}},
		Works: []symphony.WorkView{
			{ID: "benchmark-work", BaselineRevisionID: "baseline", Role: symphony.RoleBenchmark, AttemptID: "attempt", IterationRound: 1, ExperimentCycleID: "cycle", Status: symphony.WorkCompleted},
			{ID: "iteration-work", BaselineRevisionID: "baseline", Role: symphony.RoleIteration, AttemptID: "attempt", IterationRound: 1, ExperimentCycleID: "cycle", Status: symphony.WorkPending},
		},
		ExperimentCycles: []symphony.ExperimentCycleView{{
			ID: "cycle", AttemptID: "attempt", IterationRound: 1, Sequence: 1, CheckpointSHA: "aaaaaaaa",
			Status: "iteration_active", BenchmarkWorkID: "benchmark-work", IterationWorkID: "iteration-work", ReferenceReceiptID: "reference",
		}},
		ReferenceReceipts: []symphony.ReferenceReceiptView{{
			ID: "reference", ExperimentCycleID: "cycle", BenchmarkWorkID: "benchmark-work", CheckpointSHA: "aaaaaaaa", ConsumedExperimentID: "experiment",
		}},
		IterationExperiments: []symphony.IterationExperimentView{{
			ID: "experiment", AttemptID: "attempt", IterationRound: 1, Sequence: 1, Outcome: "kept", WorkID: "iteration-work",
			ReferenceReceiptID: "reference", ParentCheckpointSHA: "aaaaaaaa", CheckpointSHA: "bbbbbbbb", ReceiptID: "experiment-receipt",
		}},
	}, records: symphony.WorkbenchRecords{Artifacts: []symphony.EvidenceArtifact{{
		ID: "reference-artifact", WorkID: "benchmark-work", ReceiptID: "reference", RelativePath: "raw.json",
	}}}}
	snapshot, err := workbench.New(store, nil).Snapshot(context.Background(), 25)
	if err != nil {
		t.Fatal(err)
	}
	if findNode(t, snapshot, "experiment_cycle", "cycle").DomainStatus != "iteration_active" || findNode(t, snapshot, "reference_receipt", "reference").DomainStatus != "consumed" {
		t.Fatalf("flow v3 gate nodes = %+v", snapshot.Nodes)
	}
	for _, edge := range [][3]string{
		{"attempt:round:1", "cycle", "benchmark_gate"},
		{"cycle", "reference", "reference"},
		{"reference", "experiment", "checkpoint"},
		{"reference", "reference-artifact", "evidence"},
	} {
		if !hasEdge(snapshot, edge[0], edge[1], edge[2]) {
			t.Fatalf("flow v3 lineage omitted edge %v: %+v", edge, snapshot.Edges)
		}
	}
}

func TestArtifactRevalidatesRegistrationAndNeverBrowsesOutsideWork(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	contents := []byte(`{"metric":"latency"}`)
	path := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	metadata := symphony.EvidenceArtifact{ID: "artifact", WorkID: "work", RelativePath: "evidence.json", ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1}
	service := workbench.New(fixtureStore{artifact: metadata, work: symphony.RuntimeWork{Repository: root}}, nil)
	artifact, err := service.Artifact(context.Background(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.Previewable || string(artifact.Content) != string(contents) {
		t.Fatalf("artifact = %+v", artifact)
	}

	if err := os.WriteFile(path, []byte(`{"metric":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Artifact(context.Background(), "artifact"); err == nil {
		t.Fatal("changed artifact was accepted")
	}

	escape := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(escape, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.json")
	if err := os.Symlink(escape, link); err != nil {
		t.Fatal(err)
	}
	metadata.RelativePath = "escape.json"
	service = workbench.New(fixtureStore{artifact: metadata, work: symphony.RuntimeWork{Repository: root}}, nil)
	if _, err := service.Artifact(context.Background(), "artifact"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape error = %v", err)
	}

	metadata.RelativePath = "../secret.json"
	service = workbench.New(fixtureStore{artifact: metadata, work: symphony.RuntimeWork{Repository: root}}, nil)
	if _, err := service.Artifact(context.Background(), "artifact"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("traversal error = %v", err)
	}
}

func TestArtifactUsesDurableWorktreeInsteadOfOptimizationSource(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	contents := []byte(`{"metric":"latency"}`)
	if err := os.WriteFile(filepath.Join(worktree, "evidence.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	metadata := symphony.EvidenceArtifact{ID: "artifact", WorkID: "work", RelativePath: "evidence.json", ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1}
	service := workbench.New(fixtureStore{artifact: metadata, work: symphony.RuntimeWork{Repository: filepath.Join(t.TempDir(), "source")}, repository: worktree}, nil)
	artifact, err := service.Artifact(context.Background(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.Content) != string(contents) {
		t.Fatalf("artifact content = %q", artifact.Content)
	}
}

func TestBenchmarkArtifactUsesProtectedEvidenceRoot(t *testing.T) {
	t.Parallel()
	evidenceRoot := t.TempDir()
	workRoot := filepath.Join(evidenceRoot, "benchmarks", "benchmark-work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"latency":10}`)
	if err := os.WriteFile(filepath.Join(workRoot, "raw.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	metadata := symphony.EvidenceArtifact{ID: "artifact", WorkID: "benchmark-work", RelativePath: "raw.json", ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1}
	store := fixtureStore{
		artifact: metadata, repository: filepath.Join(t.TempDir(), "wrong-worktree"),
		work: symphony.RuntimeWork{Work: symphony.WorkView{ID: "benchmark-work", Role: symphony.RoleBenchmark}},
	}
	artifact, err := workbench.New(store, nil, evidenceRoot).Artifact(context.Background(), "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact.Content) != string(contents) {
		t.Fatalf("Benchmark artifact content = %q", artifact.Content)
	}
}

func TestLargeRegisteredArtifactIsDownloadOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	contents := []byte(strings.Repeat("x", workbench.MaxPreviewBytes+1))
	if err := os.WriteFile(filepath.Join(root, "large.txt"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	metadata := symphony.EvidenceArtifact{ID: "large", WorkID: "work", RelativePath: "large.txt", ByteSize: int64(len(contents)), ContentSHA256: hex.EncodeToString(digest[:]), ContractVersion: 1}
	artifact, err := workbench.New(fixtureStore{artifact: metadata, work: symphony.RuntimeWork{Repository: root}}, nil).Artifact(context.Background(), "large")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Previewable {
		t.Fatal("large artifact unexpectedly previewable")
	}
}

func hasEdge(snapshot workbench.Snapshot, source, target, kind string) bool {
	for _, edge := range snapshot.Edges {
		if edge.Source == source && edge.Target == target && edge.Kind == kind {
			return true
		}
	}
	return false
}

func findNode(t *testing.T, snapshot workbench.Snapshot, kind, id string) workbench.Node {
	t.Helper()
	for _, node := range snapshot.Nodes {
		if node.Kind == kind && node.ID == id {
			return node
		}
	}
	t.Fatalf("node %s/%s not found", kind, id)
	return workbench.Node{}
}

func floatPointer(value float64) *float64 { return &value }
