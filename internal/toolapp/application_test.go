package toolapp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

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
	if err != nil || len(tools) != 3 || tools[1].Name != "commit_changes" || tools[2].Name != "submit_baseline_definition" {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	required, ok := tools[0].InputSchema["required"].([]string)
	if !ok || required == nil || len(required) != 0 {
		t.Fatalf("zero-argument tool must expose Cursor-compatible required array: %#v", tools[0].InputSchema["required"])
	}
	encodedCatalog, err := json.Marshal(tools)
	if err != nil || !strings.Contains(string(encodedCatalog), `"name":"get_context"`) || !strings.Contains(string(encodedCatalog), `"required":[]`) {
		t.Fatalf("serialized tool catalog is not strict JSON Schema: %s err=%v", encodedCatalog, err)
	}
	if _, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("draft grant invoked verification tool")
	}
	largeOutput := strings.Repeat("x", 10<<10)
	hook, _ := json.Marshal(map[string]any{
		"session_id": "codex-session", "turn_id": "turn-1", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "tool-1", "tool_input": map[string]any{"command": "benchmark"},
		"tool_response": map[string]any{"output": largeOutput},
	})
	if err := engine.IngestProviderEvent(ctx, "codex", session.ID, hook); err != nil {
		t.Fatalf("ingest journal event: %v", err)
	}
	contextResult, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "get_context", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("get context: %v", err)
	}
	contextValue := contextResult.Value.(map[string]any)
	journal := contextValue["conversation_journal"].(symphony.ConversationJournalView)
	if len(journal.Tools) != 1 || !strings.Contains(string(journal.Tools[0].Output), `"truncated":true`) {
		t.Fatalf("bounded journal = %+v", journal)
	}
	if contextValue["terminal_operation"] != "submit_baseline_definition" {
		t.Fatalf("context terminal operation = %v", contextValue["terminal_operation"])
	}
	arguments := json.RawMessage(`{"idempotency_key":"terminal-1","definition":{"target":"kernel","metric":"latency"}}`)
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
	if _, err := app.Invoke(ctx, grant.Token, toolapp.Call{Name: "get_context", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("revoked grant read context")
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
	if _, err := app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: json.RawMessage(`{"idempotency_key":"submit","definition":{"target":"kernel"}}`)}); err != nil {
		t.Fatalf("submit definition: %v", err)
	}

	if err := os.WriteFile(filepath.Join(repository, "target.txt"), []byte("drifted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "target.txt")
	runGit(t, repository, "commit", "-m", "drift")
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verificationGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineVerification), "verification-session")
	_, err = app.Invoke(ctx, verificationGrant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: json.RawMessage(`{"idempotency_key":"accept","decision":"accepted","evidence":{"verified":true}}`)})
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
	_, err = app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: json.RawMessage(`{"idempotency_key":"submit","definition":{"target":"kernel"}}`)})
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
	if _, err := app.Invoke(ctx, draftGrant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: json.RawMessage(`{"idempotency_key":"submit","definition":{"target":"kernel"}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "uncommitted.txt"), []byte("drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verificationGrant := runningGrant(t, ctx, engine, pendingRole(t, view, symphony.RoleBaselineVerification), "verification-session")
	_, err = app.Invoke(ctx, verificationGrant.Token, toolapp.Call{Name: "finish_baseline_verification", Arguments: json.RawMessage(`{"idempotency_key":"accept","decision":"accepted","evidence":{"verified":true}}`)})
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
	runGit(t, repository, "add", "kernel.txt")
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
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`)}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{Meta: symphony.CommandMeta{RequestID: "accept"}, WorkID: pendingRole(t, view, symphony.RoleBaselineVerification).ID, Decision: symphony.VerificationAccepted, InitialBestSHA: baselineSHA}); err != nil {
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
	committed, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "commit_changes", Arguments: json.RawMessage(`{"idempotency_key":"commit-candidate","message":"candidate","paths":["kernel.txt"]}`)})
	if err != nil {
		t.Fatalf("commit Candidate through scoped MCP: %v", err)
	}
	commitValue := committed.Value.(gitworkspace.CommitResult)
	candidateSHA := commitValue.CommitSHA
	if candidateSHA == "" || !commitValue.Clean {
		t.Fatalf("commit result = %+v", commitValue)
	}
	if _, err := app.Invoke(ctx, iterationGrant.Token, toolapp.Call{Name: "finish_iteration", Arguments: json.RawMessage(`{"idempotency_key":"finish-iteration","outcome":"candidate","candidate_sha":"` + candidateSHA + `","summary":"faster","evidence":{"verified":true}}`)}); err != nil {
		t.Fatalf("finish Iteration: %v", err)
	}

	view, _ = engine.Inspect(ctx, symphony.Status{})
	integration := pendingRole(t, view, symphony.RoleIntegration)
	integrationGrant := runningGrant(t, ctx, engine, integration, "integration-session")
	prepared, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "prepare_best_update", Arguments: json.RawMessage(`{"idempotency_key":"prepare","validation":{"guard":"passed"}}`)})
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
	contextResult, err := app.Invoke(ctx, integrationGrant.Token, toolapp.Call{Name: "get_context", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	contextValue := contextResult.Value.(map[string]any)
	if contextValue["expected_best_sha"] != baselineSHA || contextValue["integration_fifo_position"] != int64(1) ||
		contextValue["integration_status"] != "best_update_prepared" || contextValue["git_intent_id"] != intentID ||
		contextValue["git_intent_state"] != "pending" {
		t.Fatalf("Integration get_context omitted recovery state: %+v", contextValue)
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
	definition := []byte(`{"target":"kernel","metric":"latency"}`)
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "baseline.json"), definition, 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "baseline.json")
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
