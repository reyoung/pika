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
	"github.com/reyoung/pika-go/internal/evidence"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
)

type Store interface {
	ResolveAgentGrant(context.Context, string) (symphony.AgentGrant, error)
	Apply(context.Context, symphony.Command) (symphony.Receipt, error)
	Replay(context.Context, symphony.Command) (symphony.Receipt, bool, error)
	RuntimeWork(context.Context, string) (symphony.RuntimeWork, error)
	GitIntent(context.Context, string) (symphony.GitIntentView, error)
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

type Application struct {
	Store            Store
	MaxEvidenceBytes int64
	WorktreeRoot     string
	Repository       string
	BranchNamespace  string
	WorktreeRecorder gitworkspace.WorktreeRecorder
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
		tools = append(tools, tool)
	}
	return tools, nil
}

func (a Application) Invoke(ctx context.Context, token string, call Call) (Invocation, error) {
	grant, err := a.resolve(ctx, token)
	if err != nil {
		return Invocation{}, err
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
		repository, err := a.repositoryFor(work)
		if err != nil {
			return Invocation{}, err
		}
		workspace := gitworkspace.Workspace{Repository: repository, Root: a.WorktreeRoot, Namespace: a.BranchNamespace}
		result, err := workspace.CommitChanges(ctx, grant.WorkID, input.IdempotencyKey, input.Message, input.Paths)
		if err != nil {
			return Invocation{}, err
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
		snapshot, err := workspace.SourceSnapshot(ctx)
		if err != nil {
			return Invocation{}, err
		}
		if !snapshot.Clean {
			return Invocation{}, fmt.Errorf("repository must be clean before Baseline submission: %s", snapshot.Status)
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
		}
		command := symphony.FinishIteration{Meta: symphony.CommandMeta{RequestID: input.IdempotencyKey}, WorkID: grant.WorkID,
			Outcome: input.Outcome, CandidateSHA: input.CandidateSHA, Summary: input.Summary, Evidence: input.Evidence}
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
		work, err := a.Store.RuntimeWork(ctx, grant.WorkID)
		if err != nil {
			return Invocation{}, err
		}
		workspace := a.gitWorkspace(work)
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
		return Invocation{Value: map[string]any{"intent_id": stored.ID, "applied_sha": appliedSHA}, Mutated: true}, nil
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
		"candidate_sha":   map[string]any{"type": "string"},
		"summary":         map[string]any{"type": "string", "minLength": 1},
		"evidence":        map[string]any{"type": "object"},
	}, "idempotency_key", "outcome", "summary")
	finishIterationSchema["allOf"] = []any{map[string]any{
		"if":   map[string]any{"properties": map[string]any{"outcome": map[string]any{"const": "candidate"}}},
		"then": map[string]any{"required": []string{"candidate_sha", "evidence"}, "properties": map[string]any{"evidence": benchmarkintegrity.EvidenceSchema()}},
	}}
	regressionCaseSchema := object(map[string]any{
		"case_id":  map[string]any{"type": "string", "minLength": 1},
		"kind":     map[string]any{"type": "string", "enum": []string{"correctness", "performance"}},
		"summary":  map[string]any{"type": "string", "minLength": 1},
		"evidence": map[string]any{"type": "object"},
	}, "case_id", "kind", "summary", "evidence")
	tools := map[string]Tool{
		"submit_baseline_definition": {
			Name: "submit_baseline_definition", Description: "Submit the immutable Baseline definition and complete this Work. The daemon requires benchmark_integrity schema_version 1.",
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
		"prepare_best_update": {
			Name: "prepare_best_update", Description: "Validate Integration evidence and issue a bounded Git intent. Requires benchmark_integrity and performance_claim schema_version 1; >=10x claims require independent_retest.",
			InputSchema: object(map[string]any{"idempotency_key": map[string]any{"type": "string", "minLength": 1}, "validation": benchmarkintegrity.IntegrationSchema()}, "idempotency_key", "validation"),
		},
		"apply_best_update": {
			Name: "apply_best_update", Description: "Apply exactly one active Integration's bounded Git intent inside the Pika control plane.",
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
	switch role {
	case symphony.RoleBaselineDraft:
		return []string{"commit_changes", "submit_baseline_definition"}
	case symphony.RoleBaselineVerification:
		return []string{"finish_baseline_verification"}
	case symphony.RoleIteration:
		return []string{"commit_changes", "finish_iteration"}
	case symphony.RoleIntegration:
		return []string{"prepare_best_update", "apply_best_update", "finish_integration"}
	case symphony.RoleFollowUp:
		return []string{"submit_followup_message"}
	default:
		return nil
	}
}
