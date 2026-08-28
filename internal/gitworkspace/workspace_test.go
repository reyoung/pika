package gitworkspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/gitworkspace"
)

func TestBestAndAttemptWorktreesNeverMutateUserBranch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository, baselineSHA := fixtureRepository(t)
	root := filepath.Join(t.TempDir(), "pika-worktrees")
	workspace := gitworkspace.Workspace{Repository: repository, Root: root}
	best, err := workspace.EnsureBest(ctx, baselineSHA)
	if err != nil {
		t.Fatalf("ensure Best: %v", err)
	}
	if best.SHA != baselineSHA || best.Branch != "pika/best" {
		t.Fatalf("Best = %+v", best)
	}
	round, err := workspace.CreateAttempt(ctx, "attempt-1", 1, baselineSHA)
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	writeFile(t, filepath.Join(round.Repository, "kernel.txt"), "candidate\n")
	git(t, round.Repository, "add", "kernel.txt")
	git(t, round.Repository, "commit", "-m", "candidate")
	candidateSHA := strings.TrimSpace(git(t, round.Repository, "rev-parse", "HEAD"))

	intent, err := workspace.PrepareBestUpdate(ctx, "intent-1", baselineSHA, candidateSHA)
	if err != nil {
		t.Fatalf("prepare Best update: %v", err)
	}
	appliedSHA, err := workspace.ApplyBestUpdate(ctx, intent, "accept attempt-1")
	if err != nil {
		t.Fatalf("apply Best update: %v", err)
	}
	if appliedSHA == candidateSHA {
		t.Fatal("Best update must be a Pika-owned squash commit, not the Attempt commit")
	}
	if err := workspace.VerifyBestUpdate(ctx, intent, appliedSHA); err != nil {
		t.Fatalf("verify Best update: %v", err)
	}
	if again, err := workspace.ApplyBestUpdate(ctx, intent, "accept attempt-1"); err != nil || again != appliedSHA {
		t.Fatalf("idempotent apply = %q, %v; want %q", again, err, appliedSHA)
	}
	if got := strings.TrimSpace(git(t, repository, "branch", "--show-current")); got != "main" {
		t.Fatalf("user branch = %q, want main", got)
	}
	if got := strings.TrimSpace(git(t, repository, "rev-parse", "HEAD")); got != baselineSHA {
		t.Fatalf("user HEAD changed to %q, want %q", got, baselineSHA)
	}
	if got := strings.TrimSpace(git(t, repository, "rev-parse", "refs/heads/pika/best")); got != appliedSHA {
		t.Fatalf("pika/best = %q, want %q", got, appliedSHA)
	}
}

func TestBestUpdateRetrySurvivesFailureImmediatelyBeforeGitCommit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository, baselineSHA := fixtureRepository(t)
	workspace := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(t.TempDir(), "pika-worktrees")}
	if _, err := workspace.EnsureBest(ctx, baselineSHA); err != nil {
		t.Fatal(err)
	}
	round, err := workspace.CreateAttempt(ctx, "attempt-crash", 1, baselineSHA)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(round.Repository, "kernel.txt"), "candidate after crash\n")
	git(t, round.Repository, "add", "kernel.txt")
	git(t, round.Repository, "commit", "-m", "candidate")
	candidateSHA := strings.TrimSpace(git(t, round.Repository, "rev-parse", "HEAD"))
	intent, err := workspace.PrepareBestUpdate(ctx, "intent-crash", baselineSHA, candidateSHA)
	if err != nil {
		t.Fatal(err)
	}
	hookPath := strings.TrimSpace(git(t, intent.BestRepository, "rev-parse", "--git-path", "hooks/commit-msg"))
	if !filepath.IsAbs(hookPath) {
		hookPath = filepath.Join(intent.BestRepository, hookPath)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 86\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ApplyBestUpdate(ctx, intent, "crash before commit"); err == nil {
		t.Fatal("Git hook failure did not interrupt Best commit")
	}
	if got := strings.TrimSpace(git(t, repository, "rev-parse", "refs/heads/pika/best")); got != baselineSHA {
		t.Fatalf("failed pre-commit attempt advanced Best to %s", got)
	}
	if err := os.Remove(hookPath); err != nil {
		t.Fatal(err)
	}
	appliedSHA, err := workspace.ApplyBestUpdate(ctx, intent, "retry same intent")
	if err != nil {
		t.Fatalf("retry staged intent: %v", err)
	}
	if err := workspace.VerifyBestUpdate(ctx, intent, appliedSHA); err != nil {
		t.Fatal(err)
	}
	if again, err := workspace.ApplyBestUpdate(ctx, intent, "lost response retry"); err != nil || again != appliedSHA {
		t.Fatalf("retry after committed Git response loss = %q, %v; want %q", again, err, appliedSHA)
	}
	if count := strings.TrimSpace(git(t, repository, "rev-list", "--count", baselineSHA+"..refs/heads/pika/best")); count != "1" {
		t.Fatalf("Best contains %s commits for one intent", count)
	}
}

func TestRefreshCreatesNewRoundAndMergesBestWithoutRebase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repository, baselineSHA := fixtureRepository(t)
	workspace := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(t.TempDir(), "worktrees")}
	if _, err := workspace.EnsureBest(ctx, baselineSHA); err != nil {
		t.Fatalf("ensure Best: %v", err)
	}
	old, err := workspace.CreateAttempt(ctx, "stale-attempt", 1, baselineSHA)
	if err != nil {
		t.Fatalf("create stale attempt: %v", err)
	}
	writeFile(t, filepath.Join(old.Repository, "candidate.txt"), "candidate\n")
	git(t, old.Repository, "add", "candidate.txt")
	git(t, old.Repository, "commit", "-m", "stale candidate")
	oldCandidate := strings.TrimSpace(git(t, old.Repository, "rev-parse", "HEAD"))

	winner, err := workspace.CreateAttempt(ctx, "winner", 1, baselineSHA)
	if err != nil {
		t.Fatalf("create winner: %v", err)
	}
	writeFile(t, filepath.Join(winner.Repository, "winner.txt"), "winner\n")
	git(t, winner.Repository, "add", "winner.txt")
	git(t, winner.Repository, "commit", "-m", "winner")
	winnerSHA := strings.TrimSpace(git(t, winner.Repository, "rev-parse", "HEAD"))
	intent, err := workspace.PrepareBestUpdate(ctx, "winner-intent", baselineSHA, winnerSHA)
	if err != nil {
		t.Fatalf("prepare winner: %v", err)
	}
	bestSHA, err := workspace.ApplyBestUpdate(ctx, intent, "accept winner")
	if err != nil {
		t.Fatalf("apply winner: %v", err)
	}

	refreshed, err := workspace.RefreshFromBest(ctx, "stale-attempt", 2, oldCandidate, bestSHA)
	if err != nil {
		t.Fatalf("refresh stale attempt: %v", err)
	}
	if refreshed.Repository == old.Repository || refreshed.Branch == old.Branch {
		t.Fatalf("Round 2 reused Round 1 code workspace: old=%+v refreshed=%+v", old, refreshed)
	}
	mergeSHA := strings.TrimSpace(git(t, refreshed.Repository, "rev-parse", "HEAD"))
	parents := strings.Fields(strings.TrimSpace(git(t, refreshed.Repository, "show", "-s", "--format=%P", mergeSHA)))
	if len(parents) != 2 || parents[0] != oldCandidate || parents[1] != bestSHA {
		t.Fatalf("refresh parents = %v, want merge of candidate %s and Best %s", parents, oldCandidate, bestSHA)
	}
}

func TestAttemptIdentityCannotBecomeGitArgumentsOrPaths(t *testing.T) {
	t.Parallel()

	repository, baselineSHA := fixtureRepository(t)
	workspace := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(t.TempDir(), "worktrees")}
	if _, err := workspace.CreateAttempt(context.Background(), "../unsafe", 1, baselineSHA); err == nil {
		t.Fatal("unsafe attempt identity was accepted")
	}
}

func TestCommitChangesIsScopedAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, _ := fixtureRepository(t)
	workspace := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(t.TempDir(), "worktrees")}
	writeFile(t, filepath.Join(repository, "definition.json"), "{\"target\":\"kernel\"}\n")

	first, err := workspace.CommitChanges(ctx, "draft-work-1", "commit-definition", "record Baseline definition", []string{"definition.json"})
	if err != nil {
		t.Fatalf("commit scoped changes: %v", err)
	}
	if first.CommitSHA == "" || !first.Clean || first.Status != "" {
		t.Fatalf("commit result = %+v", first)
	}
	replayed, err := workspace.CommitChanges(ctx, "draft-work-1", "commit-definition", "record Baseline definition", []string{"definition.json"})
	if err != nil || replayed.CommitSHA != first.CommitSHA {
		t.Fatalf("idempotent commit = %+v, %v; want %s", replayed, err, first.CommitSHA)
	}
	if _, err := workspace.CommitChanges(ctx, "draft-work-1", "commit-definition", "different request", []string{"definition.json"}); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("changed request under same key error = %v", err)
	}
	if _, err := workspace.CommitChanges(ctx, "draft-work-2", "unsafe", "unsafe", []string{"../outside"}); err == nil {
		t.Fatal("commit accepted a path outside the assigned repository")
	}
}

func TestCommitChangesRejectsPreStagedPathsOutsideScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, baselineSHA := fixtureRepository(t)
	workspace := gitworkspace.Workspace{Repository: repository, Root: filepath.Join(t.TempDir(), "worktrees")}
	writeFile(t, filepath.Join(repository, "definition.json"), "{\"target\":\"kernel\"}\n")
	writeFile(t, filepath.Join(repository, "unrelated.txt"), "user staged content\n")
	git(t, repository, "add", "unrelated.txt")

	if _, err := workspace.CommitChanges(ctx, "draft-work-1", "commit-definition", "record Baseline definition", []string{"definition.json"}); err == nil || !strings.Contains(err.Error(), "outside the authorized paths") {
		t.Fatalf("out-of-scope staged change error = %v", err)
	}
	if head := strings.TrimSpace(git(t, repository, "rev-parse", "HEAD")); head != baselineSHA {
		t.Fatalf("rejected scoped commit advanced HEAD to %s, want %s", head, baselineSHA)
	}
	if staged := strings.TrimSpace(git(t, repository, "diff", "--cached", "--name-only")); staged != "unrelated.txt" {
		t.Fatalf("rejected scoped commit changed user's index: %q", staged)
	}
}

func fixtureRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatalf("create repository: %v", err)
	}
	git(t, repository, "init", "--quiet", "--initial-branch=main")
	git(t, repository, "config", "user.name", "Pika Test")
	git(t, repository, "config", "user.email", "pika@example.invalid")
	writeFile(t, filepath.Join(repository, "kernel.txt"), "baseline\n")
	git(t, repository, "add", "kernel.txt")
	git(t, repository, "commit", "-m", "baseline")
	return repository, strings.TrimSpace(git(t, repository, "rev-parse", "HEAD"))
}

func git(t *testing.T, repository string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repository}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
