// Package workbench owns the read-only lineage projection consumed by the
// browser UI. It is the only layer allowed to combine durable Symphony state,
// normalized measurements, and non-authoritative runtime observations.
package workbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/workruntime"
)

const (
	APIVersion      = 1
	DefaultLimit    = 25
	MinLimit        = 5
	MaxLimit        = 100
	MaxPreviewBytes = 1 << 20
)

type Store interface {
	Inspect(context.Context, symphony.Query) (symphony.View, error)
	WorkbenchRecords(context.Context) (symphony.WorkbenchRecords, error)
	EvidenceArtifact(context.Context, string) (symphony.EvidenceArtifact, error)
	WorkRepository(context.Context, string) (string, error)
}

type RuntimeSnapshot func(context.Context) (workruntime.Snapshot, error)

type Service struct {
	store             Store
	runtime           RuntimeSnapshot
	mu                sync.Mutex
	runtimeDigest     string
	runtimeGeneration int64
}

func New(store Store, runtime RuntimeSnapshot) *Service {
	return &Service{store: store, runtime: runtime}
}

type Version struct {
	APIVersion          int   `json:"api_version"`
	DomainRevision      int64 `json:"domain_revision"`
	EventSequence       int64 `json:"event_sequence"`
	MeasurementSequence int64 `json:"measurement_sequence"`
	RuntimeGeneration   int64 `json:"runtime_generation"`
}

type RuntimeObservation struct {
	AgentName   string `json:"agent_name"`
	AgentKind   string `json:"agent_kind,omitempty"`
	Status      string `json:"status"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TerminalID  string `json:"terminal_id,omitempty"`
}

type PrimarySummary struct {
	MetricID         string  `json:"metric_id,omitempty"`
	Label            string  `json:"label,omitempty"`
	Role             string  `json:"role,omitempty"`
	Direction        string  `json:"direction,omitempty"`
	AggregateSpeedup float64 `json:"aggregate_speedup,omitempty"`
	MaxCaseSpeedup   float64 `json:"max_case_speedup,omitempty"`
	Unit             string  `json:"unit,omitempty"`
	Aggregation      string  `json:"aggregation,omitempty"`
	Source           string  `json:"source,omitempty"`
}

type Node struct {
	Kind         string              `json:"kind"`
	ID           string              `json:"id"`
	Label        string              `json:"label"`
	DomainStatus string              `json:"domain_status"`
	Subtitle     string              `json:"subtitle,omitempty"`
	Runtime      *RuntimeObservation `json:"runtime,omitempty"`
	Primary      *PrimarySummary     `json:"primary,omitempty"`
	Aggregates   []PrimarySummary    `json:"aggregates,omitempty"`
	detail       any
	workIDs      []string
	artifactIDs  []string
}

type Edge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Dashed bool   `json:"dashed,omitempty"`
}

type Snapshot struct {
	Version              Version                   `json:"snapshot_version"`
	Optimization         symphony.OptimizationView `json:"optimization"`
	Scheduler            symphony.SchedulerView    `json:"scheduler"`
	Nodes                []Node                    `json:"nodes"`
	Edges                []Edge                    `json:"edges"`
	Metrics              symphony.WorkbenchRecords `json:"metrics"`
	LegacyMeasurement    bool                      `json:"legacy_measurement"`
	TerminalAttemptLimit int                       `json:"terminal_attempt_limit"`
}

type NodeDetail struct {
	Node      Node                        `json:"node"`
	Domain    any                         `json:"domain"`
	Metrics   symphony.WorkbenchRecords   `json:"metrics"`
	Artifacts []symphony.EvidenceArtifact `json:"artifacts,omitempty"`
}

func (s *Service) Snapshot(ctx context.Context, terminalLimit int) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, errors.New("Workbench store is required")
	}
	terminalLimit = normalizeLimit(terminalLimit)
	view, err := s.store.Inspect(ctx, symphony.Status{})
	if err != nil {
		return Snapshot{}, err
	}
	records, err := s.store.WorkbenchRecords(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	runtimeSnapshot := workruntime.Snapshot{}
	if s.runtime != nil {
		runtimeSnapshot, err = s.runtime(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("observe runtime: %w", err)
		}
	}
	generation, err := s.observeRuntimeGeneration(runtimeSnapshot)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		Version:      Version{APIVersion: APIVersion, DomainRevision: view.Optimization.Revision, EventSequence: view.DomainEventCount, MeasurementSequence: records.MeasurementSequence, RuntimeGeneration: generation},
		Optimization: view.Optimization, Scheduler: view.Scheduler, Metrics: records, TerminalAttemptLimit: terminalLimit,
	}
	if view.Baseline != nil {
		snapshot.LegacyMeasurement = view.Baseline.MeasurementContractVersion == 0
	}
	snapshot.Nodes, snapshot.Edges = project(view, records, runtimeSnapshot, terminalLimit)
	return snapshot, nil
}

func (s *Service) Version(ctx context.Context) (Version, error) {
	snapshot, err := s.Snapshot(ctx, DefaultLimit)
	return snapshot.Version, err
}

func (s *Service) Node(ctx context.Context, kind, id string) (NodeDetail, error) {
	snapshot, err := s.Snapshot(ctx, MaxLimit)
	if err != nil {
		return NodeDetail{}, err
	}
	for _, node := range snapshot.Nodes {
		if node.Kind == kind && node.ID == id {
			artifacts := artifactsForNode(snapshot.Metrics.Artifacts, node)
			return NodeDetail{Node: node, Domain: node.detail, Metrics: snapshot.Metrics, Artifacts: artifacts}, nil
		}
	}
	return NodeDetail{}, os.ErrNotExist
}

func (s *Service) observeRuntimeGeneration(snapshot workruntime.Snapshot) (int64, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(encoded)
	value := hex.EncodeToString(digest[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if value != s.runtimeDigest {
		s.runtimeDigest = value
		s.runtimeGeneration++
	}
	return s.runtimeGeneration, nil
}

func normalizeLimit(limit int) int {
	if limit == 0 {
		return DefaultLimit
	}
	if limit < MinLimit {
		return MinLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

func project(view symphony.View, records symphony.WorkbenchRecords, runtime workruntime.Snapshot, terminalLimit int) ([]Node, []Edge) {
	runtimeByAgent := make(map[string]RuntimeObservation, len(runtime.Sessions))
	for _, item := range runtime.Sessions {
		runtimeByAgent[item.AgentName] = RuntimeObservation{AgentName: item.AgentName, AgentKind: item.AgentKind, Status: item.Status, WorkspaceID: item.WorkspaceID, TabID: item.TabID, PaneID: item.PaneID, TerminalID: item.TerminalID}
	}
	sessionByWork := make(map[string]symphony.AgentSession)
	for _, session := range view.AgentSessions {
		if session.Status == symphony.AgentSessionStarting || session.Status == symphony.AgentSessionRunning {
			sessionByWork[session.WorkID] = session
		}
	}
	workByAttemptRound := make(map[string]symphony.WorkView)
	workByIntegration := make(map[string]symphony.WorkView)
	worksByBaseline := make(map[string][]string)
	worksByAttempt := make(map[string][]string)
	for _, work := range view.Works {
		worksByBaseline[work.BaselineRevisionID] = append(worksByBaseline[work.BaselineRevisionID], work.ID)
		if work.AttemptID != "" {
			worksByAttempt[work.AttemptID] = append(worksByAttempt[work.AttemptID], work.ID)
		}
		if work.AttemptID != "" {
			workByAttemptRound[roundKey(work.AttemptID, work.IterationRound)] = work
		}
		if work.IntegrationID != "" {
			workByIntegration[work.IntegrationID] = work
		}
	}
	selected := selectAttempts(view.Attempts, terminalLimit)
	selectedIDs := make(map[string]bool, len(selected))
	for _, attempt := range selected {
		selectedIDs[attempt.ID] = true
	}
	aggregatesByIntegration := make(map[string][]PrimarySummary)
	aggregatesByExperiment := make(map[string][]PrimarySummary)
	metricDefinitions := make(map[string]symphony.BenchmarkMetricDefinitionView)
	for _, metric := range records.Metrics {
		metricDefinitions[metric.BaselineRevisionID+":"+metric.MetricID] = metric
	}
	for _, comparison := range records.Comparisons {
		if comparison.CaseID != "" || comparison.AggregateSpeedup == nil {
			continue
		}
		if comparison.ExperimentID != "" {
			var baselineID string
			for _, experiment := range view.IterationExperiments {
				if experiment.ID == comparison.ExperimentID {
					baselineID = workByAttemptRound[roundKey(experiment.AttemptID, experiment.IterationRound)].BaselineRevisionID
					break
				}
			}
			if metric, exists := metricDefinitions[baselineID+":"+comparison.MetricID]; exists {
				aggregatesByExperiment[comparison.ExperimentID] = append(aggregatesByExperiment[comparison.ExperimentID], PrimarySummary{
					MetricID: metric.MetricID, Label: metric.Label, Role: metric.Role, Direction: metric.Direction,
					AggregateSpeedup: *comparison.AggregateSpeedup, Unit: metric.Unit, MaxCaseSpeedup: dereference(comparison.MaxCaseSpeedup), Aggregation: metric.Aggregation, Source: "structured",
				})
			}
			continue
		}
		work, exists := workByIntegration[comparison.IntegrationID]
		if !exists {
			continue
		}
		metric, exists := metricDefinitions[work.BaselineRevisionID+":"+comparison.MetricID]
		if !exists {
			continue
		}
		aggregatesByIntegration[comparison.IntegrationID] = append(aggregatesByIntegration[comparison.IntegrationID], PrimarySummary{
			MetricID: metric.MetricID, Label: metric.Label, Role: metric.Role, Direction: metric.Direction,
			AggregateSpeedup: *comparison.AggregateSpeedup, Unit: metric.Unit, MaxCaseSpeedup: dereference(comparison.MaxCaseSpeedup), Aggregation: metric.Aggregation, Source: "structured",
		})
	}
	baselineByID := make(map[string]symphony.BaselineView, len(view.Baselines))
	for _, baseline := range view.Baselines {
		baselineByID[baseline.ID] = baseline
	}
	for _, integration := range view.Integrations {
		if len(aggregatesByIntegration[integration.ID]) != 0 {
			continue
		}
		work, exists := workByIntegration[integration.ID]
		if !exists {
			continue
		}
		if baseline, exists := baselineByID[work.BaselineRevisionID]; exists {
			aggregatesByIntegration[integration.ID] = legacyIntegrationAggregates(baseline, integration)
		}
	}
	for integrationID := range aggregatesByIntegration {
		sort.SliceStable(aggregatesByIntegration[integrationID], func(left, right int) bool {
			return aggregateRoleOrder(aggregatesByIntegration[integrationID][left].Role) < aggregateRoleOrder(aggregatesByIntegration[integrationID][right].Role)
		})
	}
	for experimentID := range aggregatesByExperiment {
		sort.SliceStable(aggregatesByExperiment[experimentID], func(left, right int) bool {
			return aggregateRoleOrder(aggregatesByExperiment[experimentID][left].Role) < aggregateRoleOrder(aggregatesByExperiment[experimentID][right].Role)
		})
	}
	aggregatesByAttempt := make(map[string][]PrimarySummary)
	for _, integration := range view.Integrations {
		if integration.Status == "accepted" {
			if aggregates := aggregatesByIntegration[integration.ID]; len(aggregates) != 0 {
				aggregatesByAttempt[integration.AttemptID] = aggregates
			}
		}
	}
	var nodes []Node
	var edges []Edge
	addEdge := func(source, target, kind string, dashed bool) {
		edges = append(edges, Edge{ID: source + "->" + target + ":" + kind, Source: source, Target: target, Kind: kind, Dashed: dashed})
	}
	for _, baseline := range view.Baselines {
		nodes = append(nodes, Node{Kind: "baseline", ID: baseline.ID, Label: fmt.Sprintf("Baseline r%d", baseline.Number), DomainStatus: string(baseline.Status), Subtitle: shortSHA(baseline.RepositorySHA), detail: baseline, workIDs: worksByBaseline[baseline.ID]})
	}
	for index, best := range view.Bests {
		node := Node{Kind: "best", ID: best.ID, Label: fmt.Sprintf("Best %d", best.Sequence), DomainStatus: "accepted", Subtitle: shortSHA(best.CommitSHA), detail: best, workIDs: worksByAttempt[best.SourceAttemptID]}
		if aggregates := aggregatesByAttempt[best.SourceAttemptID]; len(aggregates) != 0 {
			node.Aggregates = aggregates
			node.Primary = primaryAggregate(aggregates)
		}
		nodes = append(nodes, node)
		if index > 0 {
			addEdge(view.Bests[index-1].ID, best.ID, "best_spine", false)
		}
	}
	if len(view.Baselines) != 0 && len(view.Bests) != 0 {
		addEdge(view.Baselines[len(view.Baselines)-1].ID, view.Bests[0].ID, "seed", false)
	}
	bestBySequence := make(map[int64]string)
	bestByAttempt := make(map[string]string)
	for _, best := range view.Bests {
		bestBySequence[best.Sequence] = best.ID
		if best.SourceAttemptID != "" {
			bestByAttempt[best.SourceAttemptID] = best.ID
		}
	}
	for _, attempt := range selected {
		node := Node{Kind: "attempt", ID: attempt.ID, Label: "Attempt " + shortID(attempt.ID), DomainStatus: attempt.Status, Subtitle: shortSHA(attempt.CandidateSHA), detail: attempt, workIDs: worksByAttempt[attempt.ID]}
		for key, work := range workByAttemptRound {
			if strings.HasPrefix(key, attempt.ID+":") {
				if observed := runtimeForWork(work.ID, sessionByWork, runtimeByAgent); observed != nil {
					node.Runtime = observed
				}
			}
		}
		nodes = append(nodes, node)
		if base := bestBySequence[attempt.BaseBestSequence]; base != "" {
			addEdge(base, attempt.ID, "branch", false)
		}
	}
	for _, round := range view.IterationRounds {
		if !selectedIDs[round.AttemptID] {
			continue
		}
		id := roundKey(round.AttemptID, round.Round)
		node := Node{Kind: "round", ID: id, Label: fmt.Sprintf("Round %d", round.Round), DomainStatus: round.Status, Subtitle: round.Kind, detail: round}
		if work, exists := workByAttemptRound[id]; exists {
			node.workIDs = []string{work.ID}
			node.Runtime = runtimeForWork(work.ID, sessionByWork, runtimeByAgent)
		}
		nodes = append(nodes, node)
		if round.Round == 1 {
			addEdge(round.AttemptID, id, "round", false)
		} else {
			addEdge(roundKey(round.AttemptID, round.Round-1), id, "refresh", true)
		}
	}
	checkpointOwner := map[string]string{}
	artifactNodeAdded := map[string]bool{}
	artifactsByReceipt := map[string][]symphony.EvidenceArtifact{}
	for _, artifact := range records.Artifacts {
		artifactsByReceipt[artifact.ReceiptID] = append(artifactsByReceipt[artifact.ReceiptID], artifact)
	}
	for _, experiment := range view.IterationExperiments {
		if !selectedIDs[experiment.AttemptID] {
			continue
		}
		node := Node{
			Kind: "experiment", ID: experiment.ID, Label: fmt.Sprintf("Experiment %d", experiment.Sequence),
			DomainStatus: experiment.Outcome, Subtitle: shortSHA(experiment.CheckpointSHA), detail: experiment,
		}
		if work, exists := workByAttemptRound[roundKey(experiment.AttemptID, experiment.IterationRound)]; exists {
			node.workIDs = []string{work.ID}
		}
		if aggregates := aggregatesByExperiment[experiment.ID]; len(aggregates) != 0 {
			node.Aggregates = aggregates
			node.Primary = primaryAggregate(aggregates)
		}
		for _, artifact := range artifactsByReceipt[experiment.ReceiptID] {
			node.artifactIDs = append(node.artifactIDs, artifact.ID)
		}
		nodes = append(nodes, node)
		source := roundKey(experiment.AttemptID, experiment.IterationRound)
		if owner := checkpointOwner[experiment.ParentCheckpointSHA]; owner != "" {
			source = owner
		}
		addEdge(source, experiment.ID, "checkpoint", false)
		if experiment.CheckpointSHA != "" {
			checkpointOwner[experiment.CheckpointSHA] = experiment.ID
		}
		for _, artifact := range artifactsByReceipt[experiment.ReceiptID] {
			if !artifactNodeAdded[artifact.ID] {
				nodes = append(nodes, Node{Kind: "artifact", ID: artifact.ID, Label: "Evidence " + shortID(artifact.ID), DomainStatus: "verified", Subtitle: artifact.RelativePath, detail: artifact, artifactIDs: []string{artifact.ID}})
				artifactNodeAdded[artifact.ID] = true
			}
			addEdge(experiment.ID, artifact.ID, "evidence", true)
		}
	}
	for _, integration := range view.Integrations {
		if !selectedIDs[integration.AttemptID] {
			continue
		}
		node := Node{Kind: "integration", ID: integration.ID, Label: "Integration " + shortID(integration.ID), DomainStatus: integration.Status, Subtitle: shortSHA(integration.CandidateSHA), detail: integration}
		if aggregates := aggregatesByIntegration[integration.ID]; len(aggregates) != 0 {
			node.Aggregates = aggregates
			node.Primary = primaryAggregate(aggregates)
		}
		if work, exists := workByIntegration[integration.ID]; exists {
			node.workIDs = []string{work.ID}
			node.Runtime = runtimeForWork(work.ID, sessionByWork, runtimeByAgent)
		}
		nodes = append(nodes, node)
		addEdge(roundKey(integration.AttemptID, integration.IterationRound), integration.ID, "integration", false)
		if integration.CandidateExperimentID != "" {
			addEdge(integration.CandidateExperimentID, integration.ID, "candidate", false)
		}
		if target := bestByAttempt[integration.AttemptID]; target != "" && integration.Status == "accepted" {
			addEdge(integration.ID, target, "accepted", false)
		}
	}
	return nodes, edges
}

func selectAttempts(attempts []symphony.AttemptView, limit int) []symphony.AttemptView {
	active := make([]symphony.AttemptView, 0)
	terminal := make([]symphony.AttemptView, 0)
	for _, attempt := range attempts {
		switch attempt.Status {
		case "accepted", "rejected", "cancelled":
			terminal = append(terminal, attempt)
		default:
			active = append(active, attempt)
		}
	}
	if len(terminal) > limit {
		terminal = terminal[len(terminal)-limit:]
	}
	return append(terminal, active...)
}

func runtimeForWork(workID string, sessions map[string]symphony.AgentSession, observations map[string]RuntimeObservation) *RuntimeObservation {
	session, exists := sessions[workID]
	if !exists {
		return nil
	}
	observation, exists := observations[session.AgentName]
	if !exists {
		return &RuntimeObservation{AgentName: session.AgentName, AgentKind: session.AgentKind, Status: "unobserved"}
	}
	copy := observation
	return &copy
}

func artifactsForNode(artifacts []symphony.EvidenceArtifact, node Node) []symphony.EvidenceArtifact {
	wanted := make(map[string]bool, len(node.workIDs))
	for _, workID := range node.workIDs {
		wanted[workID] = true
	}
	wantedArtifacts := make(map[string]bool, len(node.artifactIDs))
	for _, artifactID := range node.artifactIDs {
		wantedArtifacts[artifactID] = true
	}
	var result []symphony.EvidenceArtifact
	for _, artifact := range artifacts {
		if wantedArtifacts[artifact.ID] || (len(wantedArtifacts) == 0 && wanted[artifact.WorkID]) {
			result = append(result, artifact)
		}
	}
	return result
}

func roundKey(attemptID string, round int64) string {
	return fmt.Sprintf("%s:round:%d", attemptID, round)
}
func shortID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
func shortSHA(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func primaryAggregate(aggregates []PrimarySummary) *PrimarySummary {
	for index := range aggregates {
		if aggregates[index].Role == "primary" {
			return &aggregates[index]
		}
	}
	if len(aggregates) != 0 {
		return &aggregates[0]
	}
	return nil
}

func aggregateRoleOrder(role string) int {
	switch role {
	case "primary":
		return 0
	case "guard":
		return 1
	default:
		return 2
	}
}

// legacyIntegrationAggregates is an explicit adapter for the frozen schema-v1
// latency contract. It never contributes normalized measurement records or
// gates: the source remains visibly marked as legacy evidence.
func legacyIntegrationAggregates(baseline symphony.BaselineView, integration symphony.IntegrationView) []PrimarySummary {
	if baseline.MeasurementContractVersion != 0 || len(baseline.Definition) == 0 || len(integration.Result) == 0 {
		return nil
	}
	var definition struct {
		SchemaVersion int64 `json:"schema_version"`
		Metrics       struct {
			Primary struct {
				Name      string `json:"name"`
				Direction string `json:"direction"`
				Unit      string `json:"unit"`
				Aggregate string `json:"aggregate"`
			} `json:"primary"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(baseline.Definition, &definition); err != nil || definition.SchemaVersion != 1 || definition.Metrics.Primary.Name != "kernel_latency_geomean_ratio" || definition.Metrics.Primary.Direction != "lower_is_better" || !strings.Contains(strings.ToLower(definition.Metrics.Primary.Aggregate), "geometric mean") {
		return nil
	}
	var result struct {
		Performance struct {
			KernelLatencyGeomeanRatio float64 `json:"kernel_latency_geomean_ratio"`
		} `json:"performance"`
	}
	if err := json.Unmarshal(integration.Result, &result); err != nil {
		return nil
	}
	ratio := result.Performance.KernelLatencyGeomeanRatio
	if ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return nil
	}
	return []PrimarySummary{{
		MetricID: definition.Metrics.Primary.Name, Label: "Latency", Role: "primary", Direction: definition.Metrics.Primary.Direction,
		AggregateSpeedup: 1 / ratio, Unit: definition.Metrics.Primary.Unit, Aggregation: "weighted_geomean_of_ratios", Source: "legacy_evidence",
	}}
}

func dereference(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

type Artifact struct {
	Metadata    symphony.EvidenceArtifact `json:"metadata"`
	ContentType string                    `json:"content_type"`
	Previewable bool                      `json:"previewable"`
	Content     []byte                    `json:"-"`
}

func (s *Service) Artifact(ctx context.Context, artifactID string) (Artifact, error) {
	metadata, err := s.store.EvidenceArtifact(ctx, artifactID)
	if err != nil {
		return Artifact{}, err
	}
	repository, err := s.store.WorkRepository(ctx, metadata.WorkID)
	if err != nil {
		return Artifact{}, err
	}
	root, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve Work repository: %w", err)
	}
	relative := filepath.Clean(filepath.FromSlash(metadata.RelativePath))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Artifact{}, errors.New("registered artifact path escapes Work repository")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve registered artifact: %w", err)
	}
	within, err := filepath.Rel(root, resolved)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return Artifact{}, errors.New("registered artifact symlink escapes Work repository")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return Artifact{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Artifact{}, errors.New("registered artifact is not a regular file")
	}
	if info.Size() != metadata.ByteSize {
		return Artifact{}, errors.New("registered artifact size changed")
	}
	contents, err := io.ReadAll(io.LimitReader(file, metadata.ByteSize+1))
	if err != nil {
		return Artifact{}, err
	}
	if int64(len(contents)) != metadata.ByteSize {
		return Artifact{}, errors.New("registered artifact size changed while reading")
	}
	digest := sha256.Sum256(contents)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), metadata.ContentSHA256) {
		return Artifact{}, errors.New("registered artifact digest changed")
	}
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(resolved)))
	if contentType == "" {
		contentType = http.DetectContentType(contents)
	}
	previewable := metadata.ByteSize <= MaxPreviewBytes && (strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") || strings.HasSuffix(strings.ToLower(resolved), ".md"))
	return Artifact{Metadata: metadata, ContentType: contentType, Previewable: previewable, Content: contents}, nil
}

func SortComparisonsByRegression(items []symphony.BenchmarkComparisonView) {
	sort.SliceStable(items, func(i, j int) bool { return items[i].RegressionFraction > items[j].RegressionFraction })
}
