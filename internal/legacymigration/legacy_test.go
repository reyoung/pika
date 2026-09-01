package legacymigration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/legacymigration"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestListAndImportPreserveDurableStateAndWorkingTrees(t *testing.T) {
	ctx := context.Background()
	repository := newRepository(t)
	head := git(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "kernel.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "staged.txt"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceStatus := gitBytes(t, repository, "status", "--porcelain=v2", "-z", "--untracked-files=all")

	roots := legacymigration.Roots{Config: filepath.Join(t.TempDir(), "config"), State: filepath.Join(t.TempDir(), "state")}
	id := "legacy-instance"
	configDir := filepath.Join(roots.Config, "instances", id)
	stateDir := filepath.Join(roots.State, "instances", id)
	if err := os.MkdirAll(filepath.Join(configDir, "instructions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configuration.RenderDefaults(repository)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "instructions", "baseline.md"), []byte("legacy instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "evidence", "result.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, filepath.Join(stateDir, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: id, Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureAgentSession(ctx, symphony.AgentSession{
		ID: "legacy-session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft,
		AgentKind: "codex", AgentName: "legacy-agent", Status: symphony.AgentSessionStarting,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	legacyGit := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(stateDir, "worktrees")}
	if _, err := legacyGit.EnsureBest(ctx, head); err != nil {
		t.Fatal(err)
	}
	attempt, err := legacyGit.CreateAttempt(ctx, "attempt-1", 1, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attempt.Repository, "attempt.txt"), []byte("unfinished\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attemptStatus := gitBytes(t, attempt.Repository, "status", "--porcelain=v2", "-z", "--untracked-files=all")

	listed, err := legacymigration.List(ctx, roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != id || listed[0].Repository != repository || listed[0].Initialization != "initialized" || listed[0].DaemonActive {
		t.Fatalf("legacy list = %+v", listed)
	}
	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	workspace, err := legacymigration.Import(ctx, legacymigration.ImportOptions{Roots: roots, InstanceID: id, WorkspaceRoot: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitBytes(t, workspace.BaseRepository, "status", "--porcelain=v2", "-z", "--untracked-files=all"); string(got) != string(sourceStatus) {
		t.Fatalf("base status = %q, want %q", got, sourceStatus)
	}
	newAttempt := filepath.Join(workspace.Root, "attempts", "attempt-1", "rounds", "1", "repo")
	if got := gitBytes(t, newAttempt, "status", "--porcelain=v2", "-z", "--untracked-files=all"); string(got) != string(attemptStatus) {
		t.Fatalf("attempt status = %q, want %q", got, attemptStatus)
	}
	if got := git(t, newAttempt, "branch", "--show-current"); got != workspace.AttemptBranch("attempt-1", 1) {
		t.Fatalf("attempt branch = %s", got)
	}
	for _, path := range []string{workspace.ConfigPath, filepath.Join(workspace.InstructionsRoot, "baseline.md"), filepath.Join(workspace.EvidenceRoot, "result.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("imported artifact %s: %v", path, err)
		}
	}
	if got := gitBytes(t, repository, "status", "--porcelain=v2", "-z", "--untracked-files=all"); string(got) != string(sourceStatus) {
		t.Fatalf("legacy source changed: %q, want %q", got, sourceStatus)
	}
	imported, err := symphony.Open(ctx, workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	sessions, err := imported.ActiveAgentSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("active imported sessions = %+v", sessions)
	}
	records, err := imported.GitWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("imported worktree records = %+v", records)
	}
}

func TestImportRollsBackExclusiveWorkspaceAndRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	repository := newRepository(t)
	roots := legacymigration.Roots{Config: filepath.Join(t.TempDir(), "config"), State: filepath.Join(t.TempDir(), "state")}
	id := "rollback-import"
	configDir := filepath.Join(roots.Config, "instances", id)
	stateDir := filepath.Join(roots.State, "instances", id)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configuration.RenderDefaults(repository)), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(stateDir, "pika.db")
	if err := os.WriteFile(databasePath, []byte("not a sqlite database\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(t.TempDir(), "workspace")
	options := legacymigration.ImportOptions{Roots: roots, InstanceID: id, WorkspaceRoot: workspaceRoot}
	lease, err := optimizationworkspace.AcquireExclusiveMutationLease(ctx, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	writerEntered := make(chan *optimizationworkspace.MutationLease, 1)
	go func() {
		writerLease, writerErr := optimizationworkspace.AcquireSharedMutationLease(ctx, workspaceRoot)
		if writerErr == nil {
			writerEntered <- writerLease
		}
	}()
	if _, err := legacymigration.ImportWithLease(ctx, options, lease); err == nil {
		t.Fatal("corrupt legacy database import succeeded")
	}
	if _, err := os.Lstat(workspaceRoot); !os.IsNotExist(err) {
		t.Fatalf("failed import left claimed Workspace: %v", err)
	}
	select {
	case writerLease := <-writerEntered:
		_ = writerLease.Close()
		t.Fatal("writer entered before failed import rollback released its exclusive lease")
	case <-time.After(100 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case writerLease := <-writerEntered:
		_ = writerLease.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not enter after failed import rollback and lease release")
	}
	if err := os.Remove(databasePath); err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, databasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Apply(ctx, symphony.Init{
		Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: id, Repository: repository,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err := legacymigration.Import(ctx, options)
	if err != nil {
		t.Fatalf("retry import: %v", err)
	}
	if workspace.Root != workspaceRoot {
		t.Fatalf("retry Workspace root=%s want=%s", workspace.Root, workspaceRoot)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runGit(t, repository, "config", "user.name", "Pika Test")
	runGit(t, repository, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "kernel.go"), []byte("package kernel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "kernel.go")
	runGit(t, repository, "commit", "--quiet", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func git(t *testing.T, repository string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(string(gitBytes(t, repository, args...)))
}

func gitBytes(t *testing.T, repository string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return output
}
