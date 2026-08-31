package symphony

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceIdentityAndGitWorktreeRegistryAreDurable(t *testing.T) {
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	identity := WorkspaceIdentity{
		ID: "00112233445566778899aabbccddeeff", Root: "/workspace", SourceRepository: "/source",
		GitCommonDir: "/source/.git", GitCommonDirDevice: 1, GitCommonDirInode: 2,
		InitialSHA: strings.Repeat("a", 40),
	}
	if err := engine.EnsureWorkspaceIdentity(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnsureWorkspaceIdentity(ctx, identity); err != nil {
		t.Fatalf("idempotent identity check: %v", err)
	}
	changed := identity
	changed.Root = "/moved"
	if err := engine.EnsureWorkspaceIdentity(ctx, changed); err == nil || !strings.Contains(err.Error(), "different Optimization Workspace") {
		t.Fatalf("identity mismatch error = %v", err)
	}
	record := GitWorktreeRecord{Role: "base", Branch: "pika/id/base", Repository: "/workspace/repo", HeadSHA: strings.Repeat("a", 40), State: "active"}
	if err := engine.UpsertGitWorktree(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.HeadSHA = strings.Repeat("b", 40)
	if err := engine.UpsertGitWorktree(ctx, record); err != nil {
		t.Fatal(err)
	}
	records, err := engine.GitWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].HeadSHA != record.HeadSHA || records[0].Repository != record.Repository {
		t.Fatalf("worktree records = %+v", records)
	}
}

func TestWorkRepositoryResolvesDurableBaseWorktree(t *testing.T) {
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/source"}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, Status{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.UpsertGitWorktree(ctx, GitWorktreeRecord{Role: "base", Branch: "pika/id/base", Repository: "/workspace/repo", HeadSHA: strings.Repeat("a", 40), State: "active"}); err != nil {
		t.Fatal(err)
	}
	repository, err := engine.WorkRepository(ctx, view.Works[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if repository != "/workspace/repo" {
		t.Fatalf("Work repository = %q", repository)
	}
}
