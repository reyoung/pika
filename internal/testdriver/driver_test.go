package testdriver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/testdriver"
)

func TestKillAtStopsProcessWhenOutboxEffectIsDispatching(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	effects, err := engine.ClaimPendingEffects(ctx, 1)
	if err != nil || len(effects) != 1 || effects[0].Type != "work.start_requested" {
		t.Fatalf("claimed effects=%+v err=%v", effects, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	})

	var stdout, stderr bytes.Buffer
	code := testdriver.Run(context.Background(), []string{
		"kill-at",
		"--pid", strconv.Itoa(process.Process.Pid),
		"--database", databasePath,
		"--point", "outbox-dispatching",
		"--effect-type", "work.start_requested",
		"--timeout", time.Second.String(),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("kill-at exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := process.Wait(); err == nil || process.ProcessState == nil || process.ProcessState.Success() {
		t.Fatalf("process was not killed: state=%v err=%v", process.ProcessState, err)
	}
	if stdout.String() == "" || stderr.Len() != 0 {
		t.Fatalf("kill-at output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestKillAtStopsProcessWhenTerminalReceiptIsCommitted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "terminal-submit"}, WorkID: view.Works[0].ID,
		Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	})

	var stdout, stderr bytes.Buffer
	code := testdriver.Run(context.Background(), []string{
		"kill-at",
		"--pid", strconv.Itoa(process.Process.Pid),
		"--database", databasePath,
		"--point", "terminal-committed",
		"--request-id", "terminal-submit",
		"--timeout", time.Second.String(),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("kill-at exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := process.Wait(); err == nil || process.ProcessState == nil || process.ProcessState.Success() {
		t.Fatalf("process was not killed: state=%v err=%v", process.ProcessState, err)
	}
}

func TestKillAtStopsProcessWhenAgentSessionIsRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	session := symphony.AgentSession{
		ID: "session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft,
		AgentKind: "codex", AgentName: "pika-session", Status: symphony.AgentSessionStarting,
	}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{
		WorkspaceID: "w1", TabID: "w1:t1", PaneID: "w1:p2", TerminalID: "term-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	})

	var stdout, stderr bytes.Buffer
	code := testdriver.Run(context.Background(), []string{
		"kill-at",
		"--pid", strconv.Itoa(process.Process.Pid),
		"--database", databasePath,
		"--point", "agent-session-running",
		"--work-id", view.Works[0].ID,
		"--timeout", time.Second.String(),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("kill-at exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := process.Wait(); err == nil || process.ProcessState == nil || process.ProcessState.Success() {
		t.Fatalf("process was not killed: state=%v err=%v", process.ProcessState, err)
	}
}

func TestKillAtStopsProcessAfterGitIntentCommitBeforeDomainFinish(t *testing.T) {
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
		t.Fatal(err)
	}

	databasePath := filepath.Join(t.TempDir(), "pika.db")
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	if _, err := engine.Apply(ctx, symphony.SubmitBaselineDefinition{
		Meta: symphony.CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: json.RawMessage(`{"target":"kernel"}`),
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	verification := pendingWork(t, view, symphony.RoleBaselineVerification)
	if _, err := engine.Apply(ctx, symphony.FinishBaselineVerification{
		Meta: symphony.CommandMeta{RequestID: "verify"}, WorkID: verification.ID,
		Decision: symphony.VerificationAccepted, InitialBestSHA: baselineSHA,
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	iteration := pendingWork(t, view, symphony.RoleIteration)
	round, err := workspace.CreateAttempt(ctx, iteration.AttemptID, iteration.IterationRound, baselineSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(round.Repository, "kernel.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, round.Repository, "add", "kernel.txt")
	runGit(t, round.Repository, "commit", "-m", "candidate")
	candidateSHA := strings.TrimSpace(runGit(t, round.Repository, "rev-parse", "HEAD"))
	if _, err := engine.Apply(ctx, symphony.FinishIteration{
		Meta: symphony.CommandMeta{RequestID: "finish-iteration"}, WorkID: iteration.ID,
		Outcome: symphony.IterationCandidate, CandidateSHA: candidateSHA, Summary: "faster",
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	integration := pendingWork(t, view, symphony.RoleIntegration)
	if _, err := engine.Apply(ctx, symphony.PrepareBestUpdate{
		Meta: symphony.CommandMeta{RequestID: "prepare"}, WorkID: integration.ID, Validation: json.RawMessage(`{"guard":"passed"}`),
	}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, symphony.Status{})
	intentID := view.Integrations[0].IntentID
	stored, err := engine.GitIntent(ctx, intentID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := workspace.PrepareBestUpdate(ctx, stored.ID, stored.ExpectedBestSHA, stored.CandidateSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ApplyBestUpdate(ctx, intent, "accepted candidate"); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	process := exec.Command("sleep", "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	})
	var stdout, stderr bytes.Buffer
	code := testdriver.Run(ctx, []string{
		"kill-at", "--pid", strconv.Itoa(process.Process.Pid), "--database", databasePath,
		"--point", "git-intent-applied", "--timeout", time.Second.String(),
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("kill-at exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if err := process.Wait(); err == nil || process.ProcessState == nil || process.ProcessState.Success() {
		t.Fatalf("process was not killed: state=%v err=%v", process.ProcessState, err)
	}
}

func pendingWork(t *testing.T, view symphony.View, role symphony.WorkRole) symphony.WorkView {
	t.Helper()
	for _, work := range view.Works {
		if work.Role == role && work.Status == symphony.WorkPending {
			return work
		}
	}
	t.Fatalf("pending %s Work not found: %+v", role, view.Works)
	return symphony.WorkView{}
}

func runGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
