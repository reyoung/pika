package symphony

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
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

func TestInitializeIterationRoundCheckpointIsCompareAndSetAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "pika.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	baseSHA := strings.Repeat("a", 40)
	preparedSHA := strings.Repeat("b", 40)
	if _, err := engine.Apply(ctx, Init{Meta: CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo", IterationConcurrency: 1}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, Status{})
	if _, err := engine.Apply(ctx, SubmitBaselineDefinition{Meta: CommandMeta{RequestID: "submit"}, WorkID: view.Works[0].ID, Definition: testcontract.Definition()}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	verification := pendingWorkForRole(t, view, RoleBaselineVerification)
	if _, err := engine.Apply(ctx, FinishBaselineVerification{Meta: CommandMeta{RequestID: "accept"}, WorkID: verification.ID, Decision: VerificationAccepted, Evidence: testcontract.Evidence(), InitialBestSHA: baseSHA}); err != nil {
		t.Fatal(err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	iteration := pendingWorkForRole(t, view, RoleIteration)
	checkpoint, err := engine.InitializeIterationRoundCheckpoint(ctx, iteration.AttemptID, iteration.IterationRound, baseSHA, preparedSHA)
	if err != nil || checkpoint != preparedSHA {
		t.Fatalf("initialize checkpoint = %q, %v", checkpoint, err)
	}
	if replay, err := engine.InitializeIterationRoundCheckpoint(ctx, iteration.AttemptID, iteration.IterationRound, baseSHA, preparedSHA); err != nil || replay != preparedSHA {
		t.Fatalf("replay checkpoint = %q, %v", replay, err)
	}
	runtimeWork, err := engine.RuntimeWork(ctx, iteration.ID)
	if err != nil || runtimeWork.CurrentCheckpointSHA != preparedSHA {
		t.Fatalf("runtime checkpoint = %q, %v", runtimeWork.CurrentCheckpointSHA, err)
	}
	view, _ = engine.Inspect(ctx, Status{})
	if len(view.IterationRounds) != 1 || view.IterationRounds[0].BaseSHA != baseSHA || view.IterationRounds[0].CurrentCheckpointSHA != preparedSHA {
		t.Fatalf("canonical Round checkpoint = %+v", view.IterationRounds)
	}
	if _, err := engine.InitializeIterationRoundCheckpoint(ctx, iteration.AttemptID, iteration.IterationRound, baseSHA, strings.Repeat("c", 40)); err == nil {
		t.Fatal("initialized checkpoint changed after compare-and-set completed")
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
