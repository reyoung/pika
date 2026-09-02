package toolapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity"
	"github.com/reyoung/pika-go/internal/candidatepolicy"
	"github.com/reyoung/pika-go/internal/evidence"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/kdacontract"
	"github.com/reyoung/pika-go/internal/symphony"
)

type Store interface {
	ResolveAgentGrant(context.Context, string) (symphony.AgentGrant, error)
	Apply(context.Context, symphony.Command) (symphony.Receipt, error)
	Replay(context.Context, symphony.Command) (symphony.Receipt, bool, error)
	ReplayDiagnosis(context.Context, string, string, symphony.DiagnosisStatus, json.RawMessage) (symphony.Receipt, bool, error)
	ReplayIterationExperiment(context.Context, string, string, json.RawMessage) (symphony.Receipt, bool, error)
	RuntimeWork(context.Context, string) (symphony.RuntimeWork, error)
	GitIntent(context.Context, string) (symphony.GitIntentView, error)
	RecordCommitCapability(context.Context, symphony.CommitCapabilityReceipt) error
	CommitCapability(context.Context, string, string) (symphony.CommitCapabilityReceipt, error)
}

type iterationCheckpointInitializer interface {
	InitializeIterationRoundCheckpoint(context.Context, string, int64, string, string) (string, error)
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type Call struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Invocation struct {
	Value    any
	Mutated  bool
	Terminal bool
}

// CommitChangesResult is the MCP response for a scoped Git commit. Checkpoint
// fields describe the flow-v2 control-plane side effect and therefore do not
// belong to gitworkspace.CommitResult.
type CommitChangesResult struct {
	gitworkspace.CommitResult
	CurrentCheckpointSHA string `json:"current_checkpoint_sha,omitempty"`
}

type Application struct {
	Store               Store
	MaxEvidenceBytes    int64
	WorktreeRoot        string
	Repository          string
	BranchNamespace     string
	WorktreeRecorder    gitworkspace.WorktreeRecorder
	AuthorizeInvocation func(context.Context, symphony.AgentGrant, Call) error
	// EvidenceRoot is the Pika-owned Workspace evidence directory. KDA roles
	// write raw artifacts there rather than into source worktrees whose clean
	// Git state is independently verified.
	EvidenceRoot string
}

func (a Application) Catalog(ctx context.Context, token string) ([]Tool, error) {
	grant, err := a.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	if grant.Revoked {
		return nil, forbidden("agent grant is revoked")
	}
	var names []string
	if err := json.Unmarshal(grant.Catalog, &names); err != nil {
		return nil, fmt.Errorf("decode grant catalog: %w", err)
	}
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tool, ok := toolByName(name)
		if !ok {
			return nil, fmt.Errorf("grant contains unsupported tool %q", name)
		}
		if name == "record_iteration_experiment" {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return nil, err
			}
			tool.InputSchema = kdacontract.ExperimentInputSchemaForFlow(int64(work.FlowVersion))
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (a Application) Invoke(ctx context.Context, token string, call Call) (Invocation, error) {
	grant, err := a.resolve(ctx, token)
	if err != nil {
		return Invocation{}, err
	}
	if a.AuthorizeInvocation != nil {
		if err := a.AuthorizeInvocation(ctx, grant, call); err != nil {
			return Invocation{}, err
		}
	}
	var names []string
	if err := json.Unmarshal(grant.Catalog, &names); err != nil {
		return Invocation{}, fmt.Errorf("decode grant catalog: %w", err)
	}
	if !slices.Contains(names, call.Name) {
		return Invocation{}, forbidden(fmt.Sprintf("tool %q is not allowed for role %s", call.Name, grant.Role))
	}
	switch call.Name {
	case "commit_changes":
		if (grant.Role != symphony.RoleBaselineDraft && grant.Role != symphony.RoleIteration) || grant.Revoked {
			return Invocation{}, forbidden("scoped commit requires an active write-capable role")
		}
		if !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string   `json:"idempotency_key"`
			Message        string   `json:"message"`
			Paths          []string `json:"paths"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" || strings.TrimSpace(input.Message) == "" || len(input.Paths) == 0 {
			return Invocation{}, errors.New("idempotency_key, message, and at least one path are required")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		if grant.Role == symphony.RoleIteration && work.FlowVersion == symphony.FlowVersion3 && len(work.IterationExperiments) != 0 {
			return Invocation{}, errors.New("flow v3 Iteration Work is locked after its single Experiment is recorded")
		}
		repository, err := a.repositoryFor(work)
		if err != nil {
			return Invocation{}, err
		}
		workspace := gitworkspace.Workspace{Repository: repository, Root: a.WorktreeRoot, Namespace: a.BranchNamespace}
		if work.CandidateChangePolicy != nil && grant.Role == symphony.RoleIteration {
			for _, path := range input.Paths {
				if work.CandidateChangePolicy.Protects(filepath.ToSlash(filepath.Clean(path))) {
					return Invocation{}, fmt.Errorf("commit path %q is protected by the Baseline candidate change policy", path)
				}
			}
		}
		commitResult, err := workspace.CommitChanges(ctx, grant.WorkID, input.IdempotencyKey, input.Message, input.Paths)
		if err != nil {
			return Invocation{}, err
		}
		if err := a.Store.RecordCommitCapability(ctx, symphony.CommitCapabilityReceipt{WorkID: grant.WorkID, CommitSHA: commitResult.CommitSHA, CommitKeySHA256: commitResult.CommitKeySHA256, RequestSHA256: commitResult.RequestSHA256}); err != nil {
			return Invocation{}, err
		}
		result := CommitChangesResult{CommitResult: commitResult}
		if grant.Role == symphony.RoleIteration && work.FlowVersion == symphony.FlowVersion2 {
			result.CurrentCheckpointSHA = work.CurrentCheckpointSHA
			if work.IterationKind != "initial" && work.CurrentCheckpointSHA == work.BaseSHA {
				mergeResolution, mergeErr := workspace.MergeCommitHasExactParents(ctx, repository, result.CommitSHA, work.CandidateSHA, work.BaseSHA)
				if mergeErr != nil {
					return Invocation{}, mergeErr
				}
				if mergeResolution {
					initializer, ok := a.Store.(iterationCheckpointInitializer)
					if !ok {
						return Invocation{}, errors.New("stale Iteration checkpoint initializer is required")
					}
					checkpoint, checkpointErr := initializer.InitializeIterationRoundCheckpoint(ctx, work.Work.AttemptID, work.Work.IterationRound, work.CurrentCheckpointSHA, result.CommitSHA)
					if checkpointErr != nil {
						return Invocation{}, checkpointErr
					}
					result.CurrentCheckpointSHA = checkpoint
				}
			}
		}
		if a.WorktreeRecorder != nil {
			record := symphony.GitWorktreeRecord{Repository: repository, HeadSHA: result.CommitSHA, State: "active"}
			if grant.Role == symphony.RoleIteration {
				record.Role, record.AttemptID, record.IterationRound = "attempt", work.Work.AttemptID, work.Work.IterationRound
				record.Branch = a.gitWorkspace(work).AttemptBranch(work.Work.AttemptID, work.Work.IterationRound)
			} else if a.Repository != "" {
				record.Role, record.Branch = "base", a.gitWorkspace(work).BaseBranch()
			}
			if record.Role != "" {
				if err := a.WorktreeRecorder.UpsertGitWorktree(ctx, record); err != nil {
					return Invocation{}, err
				}
			}
		}
		return Invocation{Value: result, Mutated: true}, nil
	case "submit_baseline_definition":
		if grant.Role != symphony.RoleBaselineDraft {
			return Invocation{}, forbidden("baseline definition requires baseline_draft role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string          `json:"idempotency_key"`
			Definition     json.RawMessage `json:"definition"`
			DefinitionPath string          `json:"definition_path"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" || (len(input.Definition) == 0) == (input.DefinitionPath == "") {
			return Invocation{}, errors.New("idempotency_key and exactly one of definition or definition_path are required")
		}
		var artifact *symphony.ArtifactInput
		if input.DefinitionPath != "" {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			repository, resolveErr := a.repositoryFor(work)
			if resolveErr != nil {
				return Invocation{}, resolveErr
			}
			contents, metadata, err := evidence.ReadStable(repository, input.DefinitionPath, a.MaxEvidenceBytes)
			if err != nil {
				return Invocation{}, err
			}
			input.Definition = contents
			artifact = &metadata
		}
		var definitionObject map[string]any
		if err := json.Unmarshal(input.Definition, &definitionObject); err != nil || definitionObject == nil {
			return Invocation{}, errors.New("definition must be a JSON object")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		workspace := a.gitWorkspace(work)
		policy, present, err := candidatepolicy.Parse(input.Definition)
		if err != nil {
			return Invocation{}, fmt.Errorf("invalid candidate change policy: %w", err)
		}
		snapshot, err := workspace.SourceSnapshot(ctx)
		if err != nil {
			return Invocation{}, err
		}
		if !snapshot.Clean {
			return Invocation{}, fmt.Errorf("repository must be clean before Baseline submission: %s", snapshot.Status)
		}
		if present {
			if err := policy.ValidateRepositoryPaths(ctx, workspace.Repository, snapshot.CommitSHA); err != nil {
				return Invocation{}, err
			}
		}
		command := symphony.SubmitBaselineDefinition{
			Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Definition: input.Definition,
			RepositorySHA: snapshot.CommitSHA, Artifact: artifact,
		}
		receipt, err := a.applyTerminal(ctx, grant, command)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "finish_baseline_verification":
		if grant.Role != symphony.RoleBaselineVerification {
			return Invocation{}, forbidden("baseline verification result requires baseline_verification role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string                        `json:"idempotency_key"`
			Decision       symphony.VerificationDecision `json:"decision"`
			FailureKind    string                        `json:"failure_kind"`
			Reason         string                        `json:"reason"`
			Requested      string                        `json:"requested_changes"`
			Evidence       json.RawMessage               `json:"evidence"`
			EvidencePath   string                        `json:"evidence_path"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		var artifacts []symphony.ArtifactInput
		if input.EvidencePath != "" {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			repository, resolveErr := a.repositoryFor(work)
			if resolveErr != nil {
				return Invocation{}, resolveErr
			}
			contents, metadata, err := evidence.ReadStable(repository, input.EvidencePath, a.MaxEvidenceBytes)
			if err != nil {
				return Invocation{}, err
			}
			if !json.Valid(contents) {
				return Invocation{}, errors.New("evidence file must contain valid JSON")
			}
			input.Evidence = contents
			artifacts = append(artifacts, metadata)
		}
		command := symphony.FinishBaselineVerification{
			Meta:             symphony.CommandMeta{RequestID: input.IdempotencyKey},
			WorkID:           grant.WorkID,
			Decision:         input.Decision,
			FailureKind:      input.FailureKind,
			Reason:           input.Reason,
			RequestedChanges: input.Requested,
			Evidence:         input.Evidence,
			Artifacts:        artifacts,
		}
		if input.Decision == symphony.VerificationAccepted {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			workspace := a.gitWorkspace(work)
			snapshot, err := workspace.SourceSnapshot(ctx)
			if err != nil {
				return Invocation{}, err
			}
			if work.BaselineRepositorySHA == "" {
				return Invocation{}, errors.New("Baseline repository snapshot is missing")
			}
			if !snapshot.Clean || snapshot.CommitSHA != work.BaselineRepositorySHA {
				return Invocation{}, fmt.Errorf("repository changed after Baseline submission: HEAD is %s, frozen snapshot is %s, status is %q", snapshot.CommitSHA, work.BaselineRepositorySHA, snapshot.Status)
			}
			best, err := workspace.EnsureBest(ctx, work.BaselineRepositorySHA)
			if err != nil {
				return Invocation{}, err
			}
			if a.WorktreeRecorder != nil {
				if err := a.WorktreeRecorder.UpsertGitWorktree(ctx, symphony.GitWorktreeRecord{
					Role: "best", Branch: best.Branch, Repository: best.Repository, HeadSHA: best.SHA, State: "active",
				}); err != nil {
					return Invocation{}, err
				}
			}
			command.InitialBestSHA = best.SHA
		}
		receipt, err := a.applyTerminal(ctx, grant, command)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "finish_diagnosis":
		if grant.Role != symphony.RoleDiagnosis {
			return Invocation{}, forbidden("Diagnosis result requires diagnosis role")
		}
		if err := kdacontract.ValidateDiagnosisInput(call.Arguments); err != nil {
			return Invocation{}, fmt.Errorf("finish_diagnosis input does not satisfy Diagnosis v1 contract: %w", err)
		}
		var input struct {
			IdempotencyKey string                   `json:"idempotency_key"`
			Outcome        symphony.DiagnosisStatus `json:"outcome"`
			Report         json.RawMessage          `json:"report"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" || len(input.Report) == 0 {
			return Invocation{}, errors.New("idempotency_key and report are required")
		}
		if receipt, found, err := a.Store.ReplayDiagnosis(ctx, grant.WorkID, input.IdempotencyKey, input.Outcome, input.Report); err != nil {
			return Invocation{}, err
		} else if found {
			return Invocation{Value: receipt, Terminal: true}, nil
		}
		if grant.Revoked {
			return Invocation{}, forbidden("Diagnosis result requires an active diagnosis role")
		}
		if !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		workspace := a.gitWorkspace(work)
		snapshot, err := workspace.SourceSnapshot(ctx)
		if err != nil {
			return Invocation{}, err
		}
		if work.BaselineRepositorySHA == "" || !snapshot.Clean || snapshot.CommitSHA != work.BaselineRepositorySHA {
			return Invocation{}, fmt.Errorf("Diagnosis source repository changed: HEAD is %s, frozen Baseline snapshot is %s, status is %q", snapshot.CommitSHA, work.BaselineRepositorySHA, snapshot.Status)
		}
		artifactRoot, err := a.diagnosisEvidenceRoot(work)
		if err != nil {
			return Invocation{}, err
		}
		artifactPaths, err := diagnosisArtifactPaths(input.Report)
		if err != nil {
			return Invocation{}, err
		}
		artifacts, err := a.readArtifacts(artifactRoot, artifactPaths)
		if err != nil {
			return Invocation{}, err
		}
		receipt, err := a.applyTerminal(ctx, grant, symphony.FinishDiagnosis{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Outcome: input.Outcome, Report: input.Report, Artifacts: artifacts})
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "record_iteration_experiment":
		if grant.Role != symphony.RoleIteration {
			return Invocation{}, forbidden("Experiment recording requires an Iteration role")
		}
		if err := kdacontract.ValidateExperimentInput(call.Arguments); err != nil {
			return Invocation{}, fmt.Errorf("record_iteration_experiment input does not satisfy the Experiment contract: %w", err)
		}
		var input struct {
			IdempotencyKey string          `json:"idempotency_key"`
			Experiment     json.RawMessage `json:"experiment"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" || len(input.Experiment) == 0 || !json.Valid(input.Experiment) {
			return Invocation{}, errors.New("idempotency_key and a valid experiment are required")
		}
		if receipt, found, err := a.Store.ReplayIterationExperiment(ctx, grant.WorkID, input.IdempotencyKey, input.Experiment); err != nil {
			return Invocation{}, err
		} else if found {
			return Invocation{Value: receipt}, nil
		}
		if grant.Revoked {
			return Invocation{}, forbidden("Experiment recording requires an active Iteration role")
		}
		if !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		experiment, err := kdacontract.ParseExperimentForFlow(input.Experiment, int64(work.FlowVersion))
		if err != nil {
			return Invocation{}, fmt.Errorf("parse flow v%d Experiment contract: %w", work.FlowVersion, err)
		}
		artifactPaths := contractArtifactPaths(experiment.Artifacts)
		artifactRoot, err := evidence.EnsureWorkRoot(a.EvidenceRoot, evidence.IterationScope, work.Work.ID)
		if err != nil {
			return Invocation{}, err
		}
		workspace := a.gitWorkspace(work)
		repository, err := workspace.AttemptRepository(work.Work.AttemptID, work.Work.IterationRound)
		if err != nil {
			return Invocation{}, err
		}
		if experiment.Outcome == "kept" {
			authorizations, err := workspace.VerifyExperimentCheckpoint(ctx, repository, experiment.ParentCheckpointSHA, experiment.CheckpointSHA, experiment.Change.Paths)
			if err != nil {
				return Invocation{}, err
			}
			for _, authorization := range authorizations {
				receipt, err := a.Store.CommitCapability(ctx, grant.WorkID, authorization.CommitSHA)
				if err != nil {
					return Invocation{}, err
				}
				if receipt.CommitKeySHA256 != authorization.CommitKeySHA256 || receipt.RequestSHA256 != authorization.RequestSHA256 {
					return Invocation{}, errors.New("Experiment commit trailers do not match the durable commit_changes receipt")
				}
			}
		} else if experiment.Outcome == "rejected" || experiment.Outcome == "inconclusive" {
			if err := workspace.VerifyExperimentRestored(ctx, repository, experiment.ParentCheckpointSHA); err != nil {
				return Invocation{}, err
			}
		}
		artifacts, err := a.readArtifacts(artifactRoot, artifactPaths)
		if err != nil {
			return Invocation{}, err
		}
		receipt, err := a.Store.Apply(ctx, symphony.RecordIterationExperiment{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Experiment: input.Experiment, Artifacts: artifacts})
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true}, nil
	case "finish_iteration_benchmark":
		if grant.Role != symphony.RoleBenchmark {
			return Invocation{}, forbidden("Benchmark result requires benchmark role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string                    `json:"idempotency_key"`
			Outcome        symphony.BenchmarkOutcome `json:"outcome"`
			Measurements   json.RawMessage           `json:"measurements"`
			Environment    json.RawMessage           `json:"environment"`
			Model          string                    `json:"model"`
			Reason         string                    `json:"reason"`
			Artifacts      []artifactPath            `json:"artifacts"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		if input.Outcome == symphony.BenchmarkMeasured {
			if work.ExperimentCycle == nil || work.CurrentCheckpointSHA == "" || work.ExperimentCycle.CheckpointSHA != work.CurrentCheckpointSHA {
				return Invocation{}, errors.New("Benchmark Work has no exact Experiment Cycle checkpoint")
			}
			if _, err := a.gitWorkspace(work).EnsureBenchmark(ctx, grant.WorkID, work.CurrentCheckpointSHA); err != nil {
				return Invocation{}, fmt.Errorf("Benchmark reference checkout is not the exact clean Cycle checkpoint: %w", err)
			}
		}
		artifactRoot, err := evidence.EnsureWorkRoot(a.EvidenceRoot, evidence.BenchmarkScope, work.Work.ID)
		if err != nil {
			return Invocation{}, err
		}
		artifacts, err := a.readArtifacts(artifactRoot, input.Artifacts)
		if err != nil {
			return Invocation{}, err
		}
		command := symphony.FinishIterationBenchmark{
			Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Outcome: input.Outcome,
			Measurements: input.Measurements, Environment: input.Environment, Provider: grant.AgentKind, Model: input.Model,
			Reason: input.Reason, Artifacts: artifacts,
		}
		receipt, err := a.applyTerminal(ctx, grant, command)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "start_next_experiment":
		if grant.Role != symphony.RoleIteration {
			return Invocation{}, forbidden("next Experiment requires iteration role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string `json:"idempotency_key"`
			Reason         string `json:"reason"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		if !grant.Revoked {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			workspace := a.gitWorkspace(work)
			repository, err := workspace.AttemptRepository(work.Work.AttemptID, work.Work.IterationRound)
			if err != nil {
				return Invocation{}, err
			}
			snapshot, err := (gitworkspace.Workspace{Repository: repository, Root: a.WorktreeRoot, Namespace: a.BranchNamespace}).SourceSnapshot(ctx)
			if err != nil {
				return Invocation{}, err
			}
			if !snapshot.Clean || snapshot.CommitSHA != work.CurrentCheckpointSHA {
				return Invocation{}, fmt.Errorf("next Experiment requires the exact clean current checkpoint: HEAD=%s checkpoint=%s status=%q", snapshot.CommitSHA, work.CurrentCheckpointSHA, snapshot.Status)
			}
		}
		receipt, err := a.applyTerminal(ctx, grant, symphony.StartNextExperiment{
			Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Reason: input.Reason,
		})
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "finish_iteration":
		if grant.Role != symphony.RoleIteration {
			return Invocation{}, forbidden("Iteration result requires iteration role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string                    `json:"idempotency_key"`
			Outcome        symphony.IterationOutcome `json:"outcome"`
			ExperimentID   string                    `json:"experiment_id"`
			CandidateSHA   string                    `json:"candidate_sha"`
			Summary        string                    `json:"summary"`
			Evidence       json.RawMessage           `json:"evidence"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		if input.Outcome == symphony.IterationCandidate && !grant.Revoked {
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			workspace := a.gitWorkspace(work)
			repository, err := workspace.AttemptRepository(work.Work.AttemptID, work.Work.IterationRound)
			if err != nil {
				return Invocation{}, err
			}
			if err := workspace.VerifyCandidate(ctx, repository, work.BaseSHA, input.CandidateSHA); err != nil {
				return Invocation{}, err
			}
			if err := workspace.VerifyCandidateChangePolicy(ctx, repository, work.BaseSHA, input.CandidateSHA, work.CandidateChangePolicy); err != nil {
				return Invocation{}, err
			}
		}
		command := symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID,
			Outcome: input.Outcome, ExperimentID: input.ExperimentID, CandidateSHA: input.CandidateSHA, Summary: input.Summary, Evidence: input.Evidence}
		receipt, err := a.applyTerminal(ctx, grant, command)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "prepare_best_update":
		if grant.Role != symphony.RoleIntegration || grant.Revoked {
			return Invocation{}, forbidden("Best update preparation requires active integration role")
		}
		if !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey string          `json:"idempotency_key"`
			Validation     json.RawMessage `json:"validation"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		workspace := a.gitWorkspace(work)
		if err := workspace.VerifyCandidateChangePolicy(ctx, workspace.Repository, work.ExpectedBestSHA, work.CandidateSHA, work.CandidateChangePolicy); err != nil {
			return Invocation{}, err
		}
		receipt, err := a.Store.Apply(ctx, symphony.PrepareBestUpdate{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Validation: input.Validation})
		if err != nil {
			return Invocation{}, err
		}
		var result struct {
			IntentID        string `json:"intent_id"`
			ExpectedBestSHA string `json:"expected_best_sha"`
			CandidateSHA    string `json:"candidate_sha"`
		}
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return Invocation{}, fmt.Errorf("decode prepared Git intent: %w", err)
		}
		work, err = a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		workspace = a.gitWorkspace(work)
		intent, err := workspace.PrepareBestUpdate(ctx, result.IntentID, result.ExpectedBestSHA, result.CandidateSHA)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: map[string]any{"receipt": receipt, "git_intent": intent}, Mutated: true}, nil
	case "apply_best_update":
		if grant.Role != symphony.RoleIntegration || grant.Revoked {
			return Invocation{}, forbidden("Best update application requires active integration role")
		}
		if !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IntentID string `json:"intent_id"`
			Message  string `json:"message"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IntentID == "" {
			return Invocation{}, errors.New("intent_id is required")
		}
		stored, err := a.Store.GitIntent(ctx, input.IntentID)
		if err != nil {
			return Invocation{}, err
		}
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		if stored.IntegrationID != work.Work.IntegrationID || stored.State != "pending" {
			return Invocation{}, forbidden("Git intent does not belong to this active Integration")
		}
		workspace := a.gitWorkspace(work)
		appliedSHA, err := workspace.ApplyAuthorizedBestUpdate(ctx, stored.ID, stored.ExpectedBestSHA, stored.CandidateSHA, input.Message)
		if err != nil {
			return Invocation{}, err
		}
		if a.WorktreeRecorder != nil {
			if err := a.WorktreeRecorder.UpsertGitWorktree(ctx, symphony.GitWorktreeRecord{
				Role: "best", Branch: workspace.BestBranch(), Repository: filepath.Join(a.WorktreeRoot, "best", "repo"), HeadSHA: appliedSHA, State: "active",
			}); err != nil {
				return Invocation{}, err
			}
		}
		return Invocation{Value: map[string]any{
			"intent_id":   stored.ID,
			"applied_sha": appliedSHA,
			// Applying the bounded patch is deliberately non-terminal. Publish the
			// exact required terminal transition so every MCP client can discover
			// that Best is not accepted until finish_integration commits it.
			"postcondition": map[string]any{
				"required_tool": "finish_integration",
				"terminal":      true,
				"arguments": map[string]any{
					"outcome":     "accepted",
					"intent_id":   stored.ID,
					"applied_sha": appliedSHA,
				},
				"missing_required_fields": []string{"idempotency_key", "result"},
				"result_contract":         "result must be the verified Integration result object accepted by finish_integration",
			},
		}, Mutated: true}, nil
	case "finish_integration":
		if grant.Role != symphony.RoleIntegration {
			return Invocation{}, forbidden("Integration result requires integration role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("agent session is not active yet")
		}
		var input struct {
			IdempotencyKey  string                      `json:"idempotency_key"`
			Outcome         symphony.IntegrationOutcome `json:"outcome"`
			IntentID        string                      `json:"intent_id"`
			AppliedSHA      string                      `json:"applied_sha"`
			Result          json.RawMessage             `json:"result"`
			RegressionCases []symphony.RegressionCase   `json:"regression_cases"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" {
			return Invocation{}, errors.New("idempotency_key is required")
		}
		var observedBest string
		if input.Outcome == symphony.IntegrationAccepted && !grant.Revoked {
			stored, err := a.Store.GitIntent(ctx, input.IntentID)
			if err != nil {
				return Invocation{}, err
			}
			work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
			if err != nil {
				return Invocation{}, err
			}
			if stored.IntegrationID != work.Work.IntegrationID || stored.State != "pending" {
				return Invocation{}, forbidden("Git intent does not belong to this active Integration")
			}
			workspace := a.gitWorkspace(work)
			intent := gitworkspace.Intent{ID: stored.ID, ExpectedBestSHA: stored.ExpectedBestSHA, CandidateSHA: stored.CandidateSHA,
				BestRepository: filepath.Join(a.WorktreeRoot, "best", "repo")}
			if err := workspace.VerifyBestUpdate(ctx, intent, input.AppliedSHA); err != nil {
				return Invocation{}, err
			}
			observedBest = stored.ExpectedBestSHA
		}
		command := symphony.FinishIntegration{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID,
			Outcome: input.Outcome, Result: input.Result, RegressionCases: input.RegressionCases,
			ObservedBestSHA: observedBest, AppliedSHA: input.AppliedSHA}
		receipt, err := a.applyTerminal(ctx, grant, command)
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	case "submit_followup_message":
		if grant.Role != symphony.RoleFollowUp {
			return Invocation{}, forbidden("Follow-up message requires follow_up role")
		}
		if !grant.Revoked && !agentSessionActive(grant.SessionStatus) {
			return Invocation{}, forbidden("Follow-up Agent Session is not active yet")
		}
		var input struct {
			IdempotencyKey string `json:"idempotency_key"`
			Message        string `json:"message"`
		}
		if err := decodeArguments(call.Arguments, &input); err != nil {
			return Invocation{}, err
		}
		if input.IdempotencyKey == "" || strings.TrimSpace(input.Message) == "" {
			return Invocation{}, errors.New("idempotency_key and message are required")
		}
		receipt, err := a.applyTerminal(ctx, grant, symphony.SubmitFollowUpMessage{
			Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID, Message: input.Message,
		})
		if err != nil {
			return Invocation{}, err
		}
		return Invocation{Value: receipt, Mutated: true, Terminal: true}, nil
	default:
		return Invocation{}, fmt.Errorf("unsupported tool %q", call.Name)
	}
}

type artifactPath struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (a Application) readArtifacts(root string, requested []artifactPath) ([]symphony.ArtifactInput, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("Pika evidence root is required")
	}
	artifacts := make([]symphony.ArtifactInput, 0, len(requested))
	seen := map[string]bool{}
	for _, requestedArtifact := range requested {
		if requestedArtifact.Path == "" || requestedArtifact.Kind == "" || seen[requestedArtifact.Path] {
			return nil, errors.New("artifact path and kind must be unique and non-empty")
		}
		_, metadata, err := evidence.ReadStable(root, requestedArtifact.Path, a.MaxEvidenceBytes)
		if err != nil {
			return nil, err
		}
		seen[requestedArtifact.Path] = true
		artifacts = append(artifacts, metadata)
	}
	return artifacts, nil
}

// Diagnosis and Experiment artifacts are part of their canonical nested
// contracts. The terminal MCP operation performs stable reads and creates the
// durable receipts from that single declaration; it never accepts a duplicate
// top-level artifact payload.
func diagnosisArtifactPaths(report json.RawMessage) ([]artifactPath, error) {
	value, err := kdacontract.ParseDiagnosisReport(report)
	if err != nil {
		return nil, errors.New("Diagnosis report must be a JSON object")
	}
	return contractArtifactPaths(value.Artifacts), nil
}

func contractArtifactPaths(artifacts []kdacontract.ArtifactRef) []artifactPath {
	paths := make([]artifactPath, 0, len(artifacts))
	for _, artifact := range artifacts {
		paths = append(paths, artifactPath{Path: artifact.Path, Kind: artifact.Kind})
	}
	return paths
}

func (a Application) diagnosisEvidenceRoot(work symphony.RuntimeWork) (string, error) {
	return evidence.EnsureWorkRoot(a.EvidenceRoot, evidence.DiagnosisScope, work.Work.ID)
}

func (a Application) applyTerminal(ctx context.Context, grant symphony.AgentGrant, command symphony.Command) (symphony.Receipt, error) {
	if !grant.Revoked {
		return a.Store.Apply(ctx, command)
	}
	receipt, found, err := a.Store.Replay(ctx, command)
	if err != nil {
		return symphony.Receipt{}, err
	}
	if !found {
		return symphony.Receipt{}, forbidden("agent grant is revoked")
	}
	return receipt, nil
}

func agentSessionActive(status symphony.AgentSessionStatus) bool {
	return status == symphony.AgentSessionStarting || status == symphony.AgentSessionRunning
}

func (a Application) resolve(ctx context.Context, token string) (symphony.AgentGrant, error) {
	if a.Store == nil {
		return symphony.AgentGrant{}, errors.New("tool application store is required")
	}
	return a.Store.ResolveAgentGrant(ctx, token)
}

func decodeArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func (a Application) gitWorkspace(work symphony.RuntimeWork) gitworkspace.Workspace {
	repository := a.Repository
	if repository == "" {
		repository = work.OptimizationRepository
	}
	return gitworkspace.Workspace{Repository: repository, Root: a.WorktreeRoot, Namespace: a.BranchNamespace}
}

func (a Application) repositoryFor(work symphony.RuntimeWork) (string, error) {
	workspace := a.gitWorkspace(work)
	switch work.Work.Role {
	case symphony.RoleIteration, symphony.RoleIntegration:
		return workspace.AttemptRepository(work.Work.AttemptID, work.Work.IterationRound)
	default:
		return workspace.Repository, nil
	}
}

func forbidden(message string) error {
	return &symphony.DomainError{Code: symphony.CodeForbidden, Message: message}
}

func toolByName(name string) (Tool, bool) {
	object := func(properties map[string]any, required ...string) map[string]any {
		// A variadic argument with no values is a nil slice, which JSON encodes as
		// null. Cursor validates JSON Schema more strictly than Codex and requires
		// the keyword, when present, to always be an array.
		requiredFields := append([]string{}, required...)
		return map[string]any{"type": "object", "properties": properties, "required": requiredFields, "additionalProperties": false}
	}
	finishIterationSchema := object(map[string]any{
		"idempotency_key": map[string]any{"type": "string", "minLength": 1},
		"outcome":         map[string]any{"type": "string", "enum": []string{"candidate", "rejected"}},
		"experiment_id":   map[string]any{"type": "string", "minLength": 1},
		"candidate_sha":   map[string]any{"type": "string"},
		"summary":         map[string]any{"type": "string", "minLength": 1},
		"evidence":        map[string]any{"type": "object"},
	}, "idempotency_key", "outcome", "summary")
	finishIterationSchema["allOf"] = []any{map[string]any{
		"if":   map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "candidate"}}},
		"then": map[string]any{"required": []string{"candidate_sha", "evidence"}, "properties": map[string]any{"evidence": benchmarkintegrity.EvidenceSchema()}},
	}}
	finishBenchmarkSchema := object(map[string]any{
		"idempotency_key": map[string]any{"type": "string", "minLength": 1},
		"outcome":         map[string]any{"type": "string", "enum": []string{"measured", "unavailable"}},
		"measurements":    benchmarkintegrity.StrictMeasurementSetSchema(),
		"environment":     map[string]any{"type": "object"},
		"model":           map[string]any{"type": "string", "minLength": 1},
		"reason":          map[string]any{"type": "string"},
		"artifacts": map[string]any{
			"type": "array", "items": object(map[string]any{
				"path": map[string]any{"type": "string", "minLength": 1},
				"kind": map[string]any{"type": "string", "minLength": 1},
			}, "path", "kind"),
		},
	}, "idempotency_key", "outcome")
	finishBenchmarkSchema["allOf"] = []any{
		map[string]any{
			"if": map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "measured"}}, "required": []string{"outcome"}},
			"then": map[string]any{"required": []string{"measurements", "environment", "model", "artifacts"}, "properties": map[string]any{
				"artifacts": map[string]any{"type": "array", "minItems": 1, "items": object(map[string]any{
					"path": map[string]any{"type": "string", "minLength": 1},
					"kind": map[string]any{"type": "string", "minLength": 1},
				}, "path", "kind")},
			}},
		},
		map[string]any{
			"if": map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "unavailable"}}, "required": []string{"outcome"}},
			"then": map[string]any{"required": []string{"reason"}, "properties": map[string]any{
				"reason": map[string]any{"type": "string", "minLength": 1},
			}},
		},
	}
	regressionCaseSchema := object(map[string]any{
		"case_id":  map[string]any{"type": "string", "minLength": 1},
		"kind":     map[string]any{"type": "string", "enum": []string{"correctness", "performance"}},
		"summary":  map[string]any{"type": "string", "minLength": 1},
		"evidence": map[string]any{"type": "object"},
	}, "case_id", "kind", "summary", "evidence")
	tools := map[string]Tool{
		"finish_iteration_benchmark": {
			Name: "finish_iteration_benchmark", Description: "Finish reference-only Benchmark Work. measured requires a complete reference Measurement Set plus protected artifacts; unavailable pauses the Optimization and is retried by pika-go resume.",
			InputSchema: finishBenchmarkSchema,
		},
		"finish_diagnosis": {
			Name: "finish_diagnosis", Description: "Complete the Baseline-owned profiler Diagnosis as ready or unavailable. report.artifacts paths are relative to the protected Diagnosis evidence directory; this terminal call stable-reads them and creates their durable receipts.",
			InputSchema: kdacontract.DiagnosisInputSchema(),
		},
		"record_iteration_experiment": {
			Name: "record_iteration_experiment", Description: "Durably record one measured Iteration experiment. experiment.artifacts paths are relative to iteration_context.evidence_root and are stable-read outside the source worktree. Only a kept record advances the Round checkpoint; negative outcomes must already be restored to their parent checkpoint.",
			InputSchema: kdacontract.ExperimentInputSchema(),
		},
		"submit_baseline_definition": {
			Name: "submit_baseline_definition", Description: "Submit the immutable Baseline definition and complete this Work. The daemon requires candidate_change_policy schema_version 1 and benchmark_integrity schema_version 1; implementation allowlists are not authoritative.",
			InputSchema: object(map[string]any{
				"idempotency_key": map[string]any{"type": "string", "minLength": 1},
				"definition":      benchmarkintegrity.DefinitionSchema(),
				"definition_path": map[string]any{"type": "string", "minLength": 1},
			}, "idempotency_key"),
		},
		"commit_changes": {
			Name: "commit_changes", Description: "Stage explicit repository-relative paths and create an idempotent commit for the active Baseline Draft or Iteration Work.",
			InputSchema: object(map[string]any{
				"idempotency_key": map[string]any{"type": "string", "minLength": 1},
				"message":         map[string]any{"type": "string", "minLength": 1},
				"paths": map[string]any{
					"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1},
				},
			}, "idempotency_key", "message", "paths"),
		},
		"finish_baseline_verification": {
			Name: "finish_baseline_verification", Description: "Accept or reject one immutable Baseline revision and complete this Work. accepted evidence requires benchmark_integrity schema_version 1 with exact per-invocation counts. evidence_path may be repository-relative or absolute, but must identify one JSON file inside the assigned Work repository; use inline evidence for multiple files or directories.",
			InputSchema: object(map[string]any{
				"idempotency_key":   map[string]any{"type": "string", "minLength": 1},
				"decision":          map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}},
				"failure_kind":      map[string]any{"type": "string"},
				"reason":            map[string]any{"type": "string"},
				"requested_changes": map[string]any{"type": "string"},
				"evidence":          map[string]any{"type": "object", "description": "For decision=accepted, must satisfy the benchmark_integrity v1 Evidence contract described by the Role System Prompt."},
				"evidence_path":     map[string]any{"type": "string", "minLength": 1, "description": "One JSON file inside the assigned Work repository, as a repository-relative or absolute path."},
			}, "idempotency_key", "decision"),
		},
		"finish_iteration": {
			Name: "finish_iteration", Description: "Submit a verified candidate or reject this Iteration Attempt. Candidate evidence must exactly cover the Round-frozen Iteration Case Snapshot; rejected evidence may be incomplete.",
			InputSchema: finishIterationSchema,
		},
		"start_next_experiment": {
			Name: "start_next_experiment", Description: "Close this flow v3 Iteration Work and require a fresh Benchmark Work before another Experiment. A reason is required when abandoning an unused Reference Receipt.",
			InputSchema: object(map[string]any{
				"idempotency_key": map[string]any{"type": "string", "minLength": 1},
				"reason":          map[string]any{"type": "string"},
			}, "idempotency_key"),
		},
		"prepare_best_update": {
			Name: "prepare_best_update", Description: "Validate Integration evidence and issue a bounded Git intent. Requires benchmark_integrity and performance_claim schema_version 1; >=10x claims require independent_retest.",
			InputSchema: object(map[string]any{"idempotency_key": map[string]any{"type": "string", "minLength": 1}, "validation": benchmarkintegrity.IntegrationSchema()}, "idempotency_key", "validation"),
		},
		"apply_best_update": {
			Name: "apply_best_update", Description: "Apply exactly one active Integration's bounded Git intent inside the Pika control plane. A successful response is non-terminal and includes the required finish_integration accepted arguments; call that tool next to commit Best.",
			InputSchema: object(map[string]any{
				"intent_id": map[string]any{"type": "string", "minLength": 1},
				"message":   map[string]any{"type": "string"},
			}, "intent_id"),
		},
		"finish_integration": {
			Name: "finish_integration", Description: "Complete Integration after verified Git postconditions, or reject it. On rejection, report every obvious case-specific correctness/performance regression in descending severity; Pika appends at most three previously unseen Cases for future Iteration Rounds.",
			InputSchema: object(map[string]any{
				"idempotency_key": map[string]any{"type": "string", "minLength": 1}, "outcome": map[string]any{"type": "string", "enum": []string{"accepted", "rejected"}},
				"intent_id": map[string]any{"type": "string"}, "applied_sha": map[string]any{"type": "string"}, "result": map[string]any{},
				"regression_cases": map[string]any{"type": "array", "items": regressionCaseSchema},
			}, "idempotency_key", "outcome"),
		},
		"submit_followup_message": {
			Name: "submit_followup_message", Description: "Submit one generated message for the still-active target Work.",
			InputSchema: object(map[string]any{
				"idempotency_key": map[string]any{"type": "string", "minLength": 1},
				"message":         map[string]any{"type": "string", "minLength": 1},
			}, "idempotency_key", "message"),
		},
	}
	tool, ok := tools[name]
	return tool, ok
}

func CatalogForRole(role symphony.WorkRole) []string {
	if descriptor, ok := symphony.DescribeRole(role); ok {
		return descriptor.ToolCatalog
	}
	return nil
}

func CatalogForWork(work symphony.RuntimeWork) []string {
	catalog := CatalogForRole(work.Work.Role)
	if work.FlowVersion == symphony.FlowVersion3 && work.Work.Role == symphony.RoleIteration {
		catalog = append(catalog, "start_next_experiment")
	}
	return catalog
}
