package optimizationworkspace_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

func TestCreateOwnsStableIdentityAndBaseLinkedWorktree(t *testing.T) {
	repository := newRepository(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "kernel-pika-workspace")
	originalHead := git(t, repository, "rev-parse", "HEAD")
	originalBranch := git(t, repository, "branch", "--show-current")

	workspace, err := optimizationworkspace.Create(context.Background(), root, repository)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Identity.ID == "" || workspace.Identity.Root != resolvedRoot || workspace.Identity.SourceRepository != repository {
		t.Fatalf("workspace identity = %+v", workspace.Identity)
	}
	if workspace.ConfigPath != filepath.Join(resolvedRoot, "pika.toml") || workspace.DatabasePath != filepath.Join(resolvedRoot, "pika.db") {
		t.Fatalf("workspace paths = %+v", workspace)
	}
	if workspace.BaseRepository != filepath.Join(resolvedRoot, "repo") {
		t.Fatalf("base repository = %s", workspace.BaseRepository)
	}
	if got := git(t, workspace.BaseRepository, "branch", "--show-current"); got != workspace.BaseBranch() {
		t.Fatalf("base branch = %q, want %q", got, workspace.BaseBranch())
	}
	if got := git(t, workspace.BaseRepository, "rev-parse", "HEAD"); got != originalHead {
		t.Fatalf("base HEAD = %q, want %q", got, originalHead)
	}
	if got := git(t, repository, "branch", "--show-current"); got != originalBranch {
		t.Fatalf("source branch changed to %q", got)
	}
	if got := git(t, repository, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("source repository changed: %q", got)
	}

	reopened, err := optimizationworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Identity != workspace.Identity {
		t.Fatalf("reopened identity = %+v, want %+v", reopened.Identity, workspace.Identity)
	}
	manifestBefore, err := os.ReadFile(workspace.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rootInfoBefore, err := os.Stat(workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.Create(context.Background(), root, repository); !errors.Is(err, optimizationworkspace.ErrAlreadyExists) {
		t.Fatalf("second create error: %v", err)
	}
	manifestAfter, err := os.ReadFile(workspace.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rootInfoAfter, err := os.Stat(workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) || rootInfoBefore.Mode() != rootInfoAfter.Mode() {
		t.Fatal("rejected create mutated existing Workspace")
	}
}

func TestOpenAndDiscoverDoNotEnsureLayoutOrPermissions(t *testing.T) {
	repository := newRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	missing := workspace.EvidenceRoot
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace.RuntimeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(workspace.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.Open(workspace.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.Discover(workspace.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(workspace.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resolution created missing layout: %v", err)
	}
	if before.Mode() != after.Mode() || after.Mode().Perm() != 0o755 {
		t.Fatalf("read-only resolution changed mode %v to %v", before.Mode(), after.Mode())
	}
	if err := workspace.ValidateLayout(); err == nil {
		t.Fatal("invalid layout was accepted")
	}
	if err := workspace.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.ValidateLayout(); err != nil {
		t.Fatalf("prepared layout is invalid: %v", err)
	}
}

func TestCreateForImportIsExclusiveNewAndDoesNotRepairExistingDestination(t *testing.T) {
	repository := newRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workspace.EvidenceRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace.RuntimeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(workspace.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	modeBefore, err := os.Stat(workspace.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.CreateForImport(context.Background(), workspace.Root, repository); !errors.Is(err, optimizationworkspace.ErrAlreadyExists) {
		t.Fatalf("CreateForImport existing error: %v", err)
	}
	manifestAfter, err := os.ReadFile(workspace.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	modeAfter, err := os.Stat(workspace.RuntimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) || modeBefore.Mode() != modeAfter.Mode() {
		t.Fatal("CreateForImport repaired existing destination")
	}
	if _, err := os.Stat(workspace.EvidenceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CreateForImport created missing layout: %v", err)
	}
}

func TestDefaultRootIsSiblingAndDiscoverWalksParents(t *testing.T) {
	parent := t.TempDir()
	repository := filepath.Join(parent, "kernel")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := optimizationworkspace.DefaultRoot(repository)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(parent, "kernel-pika-workspace")
	if root != want {
		t.Fatalf("default root = %s, want %s", root, want)
	}

	repository = newRepositoryAt(t, filepath.Join(parent, "source"))
	workspace, err := optimizationworkspace.Create(context.Background(), want, repository)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(workspace.Root, "attempts", "one")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	discovered, err := optimizationworkspace.Discover(nested)
	if err != nil {
		t.Fatal(err)
	}
	if discovered.Root != workspace.Root {
		t.Fatalf("discovered root = %s, want %s", discovered.Root, workspace.Root)
	}
}

func TestCreateRejectsUnsafeRootsAndDirtySource(t *testing.T) {
	repository := newRepository(t)
	if _, err := optimizationworkspace.Create(context.Background(), filepath.Join(repository, "workspace"), repository); err == nil || !strings.Contains(err.Error(), "outside the source repository") {
		t.Fatalf("nested workspace error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository); err == nil || !strings.Contains(err.Error(), "must be clean") {
		t.Fatalf("dirty repository error = %v", err)
	}
}

func TestOpenRejectsRelocatedWorkspace(t *testing.T) {
	repository := newRepository(t)
	root := filepath.Join(t.TempDir(), "workspace")
	workspace, err := optimizationworkspace.Create(context.Background(), root, repository)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(root), "moved")
	if err := os.Rename(workspace.Root, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := optimizationworkspace.Open(moved); err == nil || !strings.Contains(err.Error(), "was moved") {
		t.Fatalf("relocated workspace error = %v", err)
	}
}

func TestImportWorkingTreePreservesIndexAndDirtyFiles(t *testing.T) {
	repository := newRepository(t)
	if err := os.WriteFile(filepath.Join(repository, "kernel.go"), []byte("package optimized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "staged.txt"), []byte("staged\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := optimizationworkspace.CreateForImport(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.ImportWorkingTree(context.Background(), repository, workspace.BaseRepository); err != nil {
		t.Fatal(err)
	}
	sourceStatus := gitBytes(t, repository, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	targetStatus := gitBytes(t, workspace.BaseRepository, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if string(sourceStatus) != string(targetStatus) {
		t.Fatalf("imported status differs:\nsource=%q\ntarget=%q", sourceStatus, targetStatus)
	}
	if got := git(t, repository, "status", "--porcelain=v1", "--untracked-files=all"); got == "" {
		t.Fatal("source checkout was unexpectedly cleaned")
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	return newRepositoryAt(t, filepath.Join(t.TempDir(), "repo"))
}

func newRepositoryAt(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--quiet", "--initial-branch=main")
	runGit(t, root, "config", "user.name", "Pika Test")
	runGit(t, root, "config", "user.email", "pika@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "kernel.go"), []byte("package kernel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "kernel.go")
	runGit(t, root, "commit", "--quiet", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitBytes(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return output
}
